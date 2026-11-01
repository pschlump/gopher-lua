// Command embedcmp compares native C Lua 5.5.1 embedded via its C API
// (cmd/embedcmp/c/embedbench55.c → note/native-lua55/embedbench) against
// this repo's host package, in the shape an application actually uses
// Lua (Redis EVAL): SHORT scripts compiled once and run over and over
// with per-run KEYS/ARGV arguments. The record of a run lives in
// docs/Embedded-EVAL-Perf.md.
//
// Ported from glasm's cmd/embedcmp at N3b; engine legs renamed
// glasm-* → glua-*. Mechanism differences carried into the design:
//
//   - no two-tier compile cache here — the engine's cache is
//     memory-only (a cold child really parses; glasm's cold leg
//     measured a warm disk-cache hit), so the -cachedir mandate dies
//   - NewEngine cannot fail (no blob to verify) and NewVM is µs-scale
//     (a plain LState + stdlib open), not a ~4 ms wasm instantiate —
//     the fresh legs' 200-run cap (freshCap) is kept for glasm-number
//     comparability, not out of necessity
//   - VM.UsedBytes is the native per-VM accounted counter (N2),
//     reported the same way glasm reported the frozen-GC guest total
//
// Engines:
//
//	clua-reuse  native C via the C embedding API: ONE lua_State, chunk
//	            loaded once (registry ref), N staged pcalls — the
//	            Redis-EVAL shape
//	clua-fresh  native C, the naive embedder: newstate+openlibs+load+
//	            stage+pcall+close per run (re-parses every run)
//	glua-cold   fresh process: NewEngine → Compile → exactly one
//	            e.Run. The daemon-restart posture (a real parse —
//	            there is no disk tier to hit).
//	glua-fresh  one process, Compile once, N e.Run WITHOUT the VM pool
//	            (fresh VM per run — the no-pool posture)
//	glua-pool   the daemon steady state: WithVMCacheSize(4); one
//	            untimed priming run, then N pool-hit e.Run calls via
//	            the real host API
//
// Unlike a CLI-shaped bench there is NO "KEYS={}ARGV={}" prelude: both
// harnesses stage real per-run KEYS/ARGV STRING tables (embedbench55.c
// stage_args and RunOptions carry the same constants), inside the
// timed region on both sides (Redis restages per run; host stages
// inside e.Run).
//
// Postures:
//
//	cold  one fresh process per rep (exactly one run): wall = process
//	      wall under /usr/bin/time -l (floor ~3.7 ms), plus the child's
//	      in-process first-run PERFRUN and load/compile time
//	warm  one process, N timed in-process runs (median/min reported;
//	      2 warmup runs untimed, except glua-pool's single priming run)
//
// Every leg's return values are cross-checked against clua-reuse — the
// suite doubles as a correctness tripwire for the staged-args contract.
//
// Usage (from the repo root):
//
//	cmd/embedcmp/build-embedbench55.sh
//	go run ./cmd/embedcmp [-only substr]
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pschlump/gopher-lua/host"
)

// stagedKeys/stagedArgv are staged as the KEYS/ARGV globals on EVERY
// run, in EVERY leg — they MUST mirror g_keys/g_argv in
// cmd/embedcmp/c/embedbench55.c (the RET cross-check is the tripwire).
var (
	stagedKeys = []string{
		"key:1", "key:2", "key:3", "key:4", "key:5",
		"key:6", "key:7", "key:8", "key:9", "key:10",
	}
	stagedArgv = []string{"17", "4", "alpha beta", "0"}
)

type scriptDef struct {
	name            string
	coldReps, warmN int
}

var scripts = []scriptDef{
	{"e_arg.lua", 12, 3000},
	{"e_sum.lua", 12, 3000},
	{"e_tag.lua", 12, 3000},
	{"e_norm.lua", 12, 3000},
	{"e_hash.lua", 8, 2000},
	{"e_call.lua", 8, 2000},
	{"e_sort.lua", 8, 500},
}

// freshCap: the fresh legs pay their no-reuse tax per run (clua-fresh a
// full parse + state build, glua-fresh an LState build + stdlib open),
// so their loops cap at 200 runs (glasm's cap, kept for
// comparability with its recorded numbers) and both fresh legs measure
// the same iteration count.
const freshCap = 200

var (
	childFlag  = flag.String("child", "", "child mode: glua-cold|glua-fresh|glua-pool|glua-used")
	posture    = flag.String("posture", "warm", "child posture: cold (1 run, no warmup) or warm")
	srcFlag    = flag.String("src", "", "child: script source file")
	runsFlag   = flag.Int("runs", 1, "child: timed runs")
	nameFlag   = flag.String("name", "bench.lua", "child: chunk name")
	only       = flag.String("only", "", "restrict to scripts whose name contains this substring")
	embedBin   = flag.String("embedbench", "note/native-lua55/embedbench", "native C embedding-API harness (cmd/embedcmp/build-embedbench55.sh)")
	scriptsDir = flag.String("scripts", "cmd/embedcmp/scripts", "bench script dir (repo-root relative)")
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "embedcmp: fatal:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- children

// report is the Go-child protocol tail: per-run timing (median, min),
// the last run's return values, and post-run Go memory stats.
func report(ret string, ds []time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("PERFRUN %d %d\n", ds[len(ds)/2].Nanoseconds(), ds[0].Nanoseconds())
	fmt.Printf("RET %s\n", strings.ReplaceAll(ret, "\n", `\n`))
	fmt.Printf("GOMEM %d %d %d\n", ms.HeapSys, ms.Sys, ms.HeapAlloc)
}

func resultString(res host.Result) string {
	vals := make([]string, 0, len(res.Values))
	for _, v := range res.Values {
		vals = append(vals, v.String())
	}
	return strings.Join(vals, " ")
}

// benchCall is the hostfn analog of embedbench55.c's bench_call: one
// string down, its byte-length integer back — the redis.call shape.
func benchCall(vm *host.VM, args []host.Value) ([]host.Value, error) {
	if len(args) != 1 || args[0].Kind != host.KindString {
		return nil, fmt.Errorf("bench.call: want one string")
	}
	return []host.Value{host.Int(int64(len(args[0].Str)))}, nil
}

// childGlua: registers bench.call before the first NewVM (the registry
// freezes then).
func childGlua(src []byte, name, mode string, warmup, runs int) {
	opts := []host.Option{}
	if mode == "glua-pool" {
		opts = append(opts, host.WithVMCacheSize(4))
	}
	e := host.NewEngine(opts...)
	must(e.RegisterGlobal("bench", "call", benchCall))
	tc := time.Now()
	s, err := e.Compile(src, name)
	must(err)
	fmt.Printf("COMPILENS %d\n", time.Since(tc).Nanoseconds())
	ctx := context.Background()
	opt := host.RunOptions{Keys: stagedKeys, Argv: stagedArgv}

	switch mode {
	case "glua-cold":
		// exactly one run; PERFRUN is the in-process first-run figure
		// (process wall + the /usr/bin/time exec floor is the parent's
		// business). Compile was reported separately above.
		runtime.GC()
		t0 := time.Now()
		res, err := e.Run(ctx, s, opt)
		must(err)
		report(resultString(res), []time.Duration{time.Since(t0)})
	case "glua-fresh":
		// fresh VM per run: Engine.Run without a pool pays NewVM every
		// time (each side's no-reuse tax: C re-parses, glua rebuilds
		// the LState + stdlib)
		ret := ""
		var ds []time.Duration
		for i := 0; i < warmup; i++ {
			res, err := e.Run(ctx, s, opt)
			must(err)
			ret = resultString(res)
		}
		for i := 0; i < runs; i++ {
			runtime.GC()
			t0 := time.Now()
			res, err := e.Run(ctx, s, opt)
			must(err)
			ret = resultString(res)
			ds = append(ds, time.Since(t0))
		}
		report(ret, ds)
	case "glua-pool":
		// the daemon steady state: one untimed priming run binds a VM
		// into the pool, the timed loop is pure pool hits
		_, err := e.Run(ctx, s, opt)
		must(err)
		ret := ""
		var ds []time.Duration
		for i := 0; i < runs; i++ {
			runtime.GC()
			t0 := time.Now()
			res, err := e.Run(ctx, s, opt)
			must(err)
			ret = resultString(res)
			ds = append(ds, time.Since(t0))
		}
		report(ret, ds)
	case "glua-used":
		// UsedBytes is unreachable from e.Run (host.Engine keeps its
		// pool private), so pin one VM manually. The pin serves the
		// full N runs where a pooled VM is recycled at vmRecycleRuns
		// (1024) and its gauge resets — the numbers therefore show the
		// raw per-run accumulation (USED1 → USEDN over N), not a pooled
		// VM's lifetime high-water. Read USED bytes BEFORE Close (closed
		// VMs report 0). N2 note: the native counter is the
		// accounted-allocation gauge (membudget.go) — monotone by
		// construction, the analog of glasm's frozen-GC guest total.
		vm, err := e.NewVM()
		must(err)
		res, err := vm.Run(ctx, s, opt)
		must(err)
		u1 := vm.UsedBytes()
		for i := 1; i < runs; i++ {
			res, err = vm.Run(ctx, s, opt)
			must(err)
		}
		un := vm.UsedBytes()
		must(vm.Close())
		fmt.Printf("USED1 %d\n", u1)
		fmt.Printf("USEDN %d\n", un)
		fmt.Printf("RET %s\n", strings.ReplaceAll(resultString(res), "\n", `\n`))
	default:
		must(fmt.Errorf("unknown glua child mode %q", mode))
	}
	must(e.Close())
}

func childMain() {
	src, err := os.ReadFile(*srcFlag)
	must(err)
	warmup, runs := 0, *runsFlag
	if *posture == "warm" {
		warmup = 2
	} else {
		runs = 1
	}
	switch *childFlag {
	case "glua-cold", "glua-fresh", "glua-pool", "glua-used":
		childGlua(src, *nameFlag, *childFlag, warmup, runs)
	default:
		must(fmt.Errorf("unknown -child %q", *childFlag))
	}
}

// ----------------------------------------------------------------- parent

type cell struct {
	wall    time.Duration // cold: median process wall; warm: child-reported median
	first   time.Duration // cold only: the 1-run child's in-process PERFRUN
	compile time.Duration // LOADNS (clua) / COMPILENS (glua)
	rss     int64         // bytes; cold: median across reps; warm: the loop process
	ret     string
	gomem   string // "heapsys sys heapalloc" (Go children, post-run)
	heap    string // live KB at exit, post forced collect (clua)
	used1   int64  // guest alloc bytes after run 1 (glua-used proxy)
	usedn   int64  // ... after run N
}

type leg struct {
	engine string
	cold   cell
	warm   cell
}

type row struct {
	name string
	legs []leg
}

var engineOrder = []string{"clua-reuse", "clua-fresh", "glua-cold", "glua-fresh", "glua-pool"}

var rssRe = regexp.MustCompile(`(\d+)\s+maximum resident set size`)

// childEnv pins the darwin-27 wedge-family mitigation on every child
// (glasm's memory: long-lived cgo+Go processes wedge without
// asyncpreemptoff; kept for posture parity with the recorded runs).
func childEnv() []string {
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "GODEBUG=") {
			if !strings.Contains(kv, "asyncpreemptoff=1") {
				env[i] += ",asyncpreemptoff=1"
			}
			return env
		}
	}
	return append(env, "GODEBUG=asyncpreemptoff=1")
}

// runTimed runs argv under /usr/bin/time -l and returns the process wall
// time, maxrss (bytes), and the child's stdout.
func runTimed(argv []string) (time.Duration, int64, string) {
	cmd := exec.Command("/usr/bin/time", append([]string{"-l"}, argv...)...)
	cmd.Env = childEnv()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	t0 := time.Now()
	err := cmd.Run()
	wall := time.Since(t0)
	if err != nil {
		must(fmt.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, errb.String()))
	}
	var rss int64
	if m := rssRe.FindStringSubmatch(errb.String()); m != nil {
		rss, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return wall, rss, out.String()
}

// childOut is the parsed line protocol of one child (C embedbench or Go
// self re-exec — one parser serves both).
type childOut struct {
	med, compile time.Duration
	ret, gomem   string
	heap         string
	used1, usedn int64
}

func parseChildOut(out string) (c childOut) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "PERFRUN "):
			if len(f) >= 2 {
				// the C harness prints one-decimal ns (sub-µs runs);
				// Go children print integers — ParseFloat takes both
				if v, err := strconv.ParseFloat(f[1], 64); err == nil {
					c.med = time.Duration(v)
				}
			}
		case strings.HasPrefix(line, "LOADNS "), strings.HasPrefix(line, "COMPILENS "):
			if len(f) >= 2 {
				// same one-decimal-ns tolerance as PERFRUN
				if v, err := strconv.ParseFloat(f[1], 64); err == nil {
					c.compile = time.Duration(v)
				}
			}
		case strings.HasPrefix(line, "RET "):
			c.ret = strings.TrimPrefix(line, "RET ")
		case strings.HasPrefix(line, "GOMEM "):
			c.gomem = strings.TrimPrefix(line, "GOMEM ")
		case strings.HasPrefix(line, "HEAPKB "):
			c.heap = strings.TrimPrefix(line, "HEAPKB ")
		case strings.HasPrefix(line, "USED1 "):
			if len(f) >= 2 {
				c.used1, _ = strconv.ParseInt(f[1], 10, 64)
			}
		case strings.HasPrefix(line, "USEDN "):
			if len(f) >= 2 {
				c.usedn, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	return
}

func medDur(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func medInt(xs []int64) int64 {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs[len(xs)/2]
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", float64(d)/float64(time.Second))
	}
}

func durOr(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return fmtDur(d)
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func fmtRSS(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dKB", b>>10)
	}
}

func rssOr(b int64) string {
	if b == 0 {
		return "—"
	}
	return fmtRSS(b)
}

// runLegs measures every engine on one script file (no prelude — the
// harnesses stage real KEYS/ARGV).
func runLegs(sc scriptDef, tmp, self string) []leg {
	byName := map[string]*leg{}
	legs := make([]leg, len(engineOrder))
	for i, e := range engineOrder {
		legs[i].engine = e
		byName[e] = &legs[i]
	}

	// childArgv builds a glua child invocation.
	childArgv := func(eng, posture string, runs int) []string {
		return []string{self,
			"-child=" + eng, "-posture=" + posture, "-src=" + tmp,
			"-name=" + sc.name, "-runs=" + strconv.Itoa(runs)}
	}

	// clua-reuse cold: fresh process per rep, exactly one staged run.
	{
		var walls []time.Duration
		var rsses []int64
		var last childOut
		for r := 0; r < sc.coldReps; r++ {
			w, rss, out := runTimed([]string{*embedBin, "reuse", tmp, "1", "0"})
			walls = append(walls, w)
			rsses = append(rsses, rss)
			last = parseChildOut(out)
		}
		byName["clua-reuse"].cold = cell{wall: medDur(walls), first: last.med,
			compile: last.compile, rss: medInt(rsses), ret: last.ret, heap: last.heap}
	}
	// clua-reuse warm: one state, one load, N staged pcalls.
	{
		_, rss, out := runTimed([]string{*embedBin, "reuse", tmp, strconv.Itoa(sc.warmN)})
		c := parseChildOut(out)
		byName["clua-reuse"].warm = cell{wall: c.med, rss: rss, ret: c.ret, heap: c.heap}
	}
	// clua-fresh warm only (cold is process-identical to clua-reuse
	// cold — one run in a fresh process), capped at freshCap.
	{
		n := sc.warmN
		if n > freshCap {
			n = freshCap
		}
		_, rss, out := runTimed([]string{*embedBin, "fresh", tmp, strconv.Itoa(n)})
		c := parseChildOut(out)
		byName["clua-fresh"].warm = cell{wall: c.med, rss: rss, ret: c.ret, heap: c.heap}
	}
	// glua-cold: daemon restart — no disk tier exists here, so every
	// timed cold rep is the real parse+compile+first-run shape.
	{
		var walls []time.Duration
		var rsses []int64
		var last childOut
		for r := 0; r < sc.coldReps; r++ {
			w, rss, out := runTimed(childArgv("glua-cold", "cold", 1))
			walls = append(walls, w)
			rsses = append(rsses, rss)
			last = parseChildOut(out)
		}
		byName["glua-cold"].cold = cell{wall: medDur(walls), first: last.med,
			compile: last.compile, rss: medInt(rsses), ret: last.ret, gomem: last.gomem}
	}
	// glua-fresh warm: fresh VM per run, capped at freshCap.
	{
		n := sc.warmN
		if n > freshCap {
			n = freshCap
		}
		_, rss, out := runTimed(childArgv("glua-fresh", "warm", n))
		c := parseChildOut(out)
		byName["glua-fresh"].warm = cell{wall: c.med, rss: rss, ret: c.ret, gomem: c.gomem, compile: c.compile}
	}
	// glua-pool warm: the daemon steady state (priming run untimed
	// inside the child).
	{
		_, rss, out := runTimed(childArgv("glua-pool", "warm", sc.warmN))
		c := parseChildOut(out)
		byName["glua-pool"].warm = cell{wall: c.med, rss: rss, ret: c.ret, gomem: c.gomem, compile: c.compile}
	}
	// glua-used diagnostic: accounted-allocation growth on a pinned VM
	// (proxy for the pooled VM — see the child). Attached to glua-pool.
	{
		_, _, out := runTimed(childArgv("glua-used", "warm", sc.warmN))
		c := parseChildOut(out)
		byName["glua-pool"].warm.used1 = c.used1
		byName["glua-pool"].warm.usedn = c.usedn
	}
	return legs
}

// normRet compares return values across engines: newlines are escaped
// by both harnesses, so fold both spellings into single-space-joined
// tokens.
func normRet(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `\n`, " ")), " ")
}

// emitMarkdown prints paste-ready tables for the doc.
func emitMarkdown(rows []row) {
	pick := func(r row, eng string) *leg {
		for i := range r.legs {
			if r.legs[i].engine == eng {
				return &r.legs[i]
			}
		}
		return nil
	}
	table := func(title string, engines []string, val func(r row, l *leg) string) {
		fmt.Printf("\n## %s\n\n", title)
		fmt.Printf("| script |")
		for _, e := range engines {
			fmt.Printf(" %s |", e)
		}
		fmt.Printf("\n|---|%s\n", strings.Repeat("---|", len(engines)))
		for _, r := range rows {
			fmt.Printf("| %s |", strings.TrimSuffix(r.name, ".lua"))
			for _, e := range engines {
				if l := pick(r, e); l != nil {
					fmt.Printf(" %s |", val(r, l))
				} else {
					fmt.Printf(" — |")
				}
			}
			fmt.Println()
		}
	}

	table("Cold wall (one-shot process, median)",
		[]string{"clua-reuse", "glua-cold"},
		func(_ row, l *leg) string { return durOr(l.cold.wall) })
	table("Cold first run (in-process, excludes the /usr/bin/time floor)",
		[]string{"clua-reuse", "glua-cold"},
		func(_ row, l *leg) string { return durOr(l.cold.first) })
	table("Cold script load/compile (in-process)",
		[]string{"clua-reuse", "glua-cold"},
		func(_ row, l *leg) string { return durOr(l.cold.compile) })
	table("Warm wall (per run, steady state, median)",
		[]string{"clua-reuse", "clua-fresh", "glua-fresh", "glua-pool"},
		func(_ row, l *leg) string { return durOr(l.warm.wall) })
	table("Warm wall × native reuse",
		[]string{"clua-fresh", "glua-fresh", "glua-pool"},
		func(r row, l *leg) string {
			ref := pick(r, "clua-reuse").warm.wall
			if l.warm.wall == 0 || ref == 0 {
				return "—"
			}
			return fmt.Sprintf("%.1f×", float64(l.warm.wall)/float64(ref))
		})
	table("Cold maxRSS",
		[]string{"clua-reuse", "glua-cold"},
		func(_ row, l *leg) string { return rssOr(l.cold.rss) })
	table("Warm maxRSS (loop process)",
		[]string{"clua-reuse", "clua-fresh", "glua-fresh", "glua-pool"},
		func(_ row, l *leg) string { return rssOr(l.warm.rss) })

	// memory detail: live C heap vs Go heap live vs accounted totals
	fmt.Printf("\n## Memory detail (post-loop)\n\n")
	fmt.Printf("| script | clua lua-heap (live) | glua go-heap live | glua accounted USED 1→N (monotone gauge) |\n|---|---|---|---|\n")
	for _, r := range rows {
		cr := pick(r, "clua-reuse").warm
		gp := pick(r, "glua-pool").warm
		live := "—"
		if f := strings.Fields(gp.gomem); len(f) == 3 {
			live = fmtRSS(atoi64(f[2]))
		}
		used := "—"
		if gp.used1 > 0 {
			used = fmt.Sprintf("%s → %s", fmtRSS(gp.used1), fmtRSS(gp.usedn))
		}
		heap := "—"
		if cr.heap != "" {
			heap = cr.heap + "KB"
		}
		fmt.Printf("| %s | %s | %s | %s |\n", strings.TrimSuffix(r.name, ".lua"), heap, live, used)
	}
}

func main() {
	flag.Parse()
	if *childFlag != "" {
		childMain()
		return
	}

	if _, err := os.Stat(filepath.Join(*scriptsDir, "e_arg.lua")); err != nil {
		must(fmt.Errorf("cannot read %s — run embedcmp from the repo root: %v", *scriptsDir, err))
	}
	if _, err := os.Stat(*embedBin); err != nil {
		must(fmt.Errorf("native embedbench missing at %s — run cmd/embedcmp/build-embedbench55.sh first: %v", *embedBin, err))
	}
	self, err := os.Executable()
	must(err)

	fmt.Printf("# embedcmp — embedded-EVAL: native C Lua 5.5.1 (C API) vs gopher-lua host/ | %s | GOMAXPROCS=%d | %s\n",
		runtime.Version(), runtime.GOMAXPROCS(0), time.Now().Format("2006-01-02 15:04"))
	fmt.Printf("# embedbench=%s staged=%d keys/%d argv children GODEBUG=asyncpreemptoff=1\n",
		*embedBin, len(stagedKeys), len(stagedArgv))

	tmpdir, err := os.MkdirTemp("", "embedcmp")
	must(err)
	defer os.RemoveAll(tmpdir)

	var rows []row
	for _, sc := range scripts {
		stem := strings.TrimSuffix(sc.name, ".lua")
		if *only != "" && !strings.Contains(stem, *only) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(*scriptsDir, sc.name))
		must(err)
		tmp := filepath.Join(tmpdir, sc.name)
		must(os.WriteFile(tmp, src, 0o644)) // byte-identical on every leg; NO prelude

		legs := runLegs(sc, tmp, self)
		rows = append(rows, row{name: sc.name, legs: legs})

		fmt.Printf("\n=== %s (%d bytes)\n", sc.name, len(src))
		fmt.Printf("  %-12s %8s %9s   %8s %9s\n", "engine", "cold", "maxRSS", "warm", "maxRSS")
		rets := map[string]string{}
		for _, lg := range legs {
			fmt.Printf("  %-12s %8s %9s   %8s %9s",
				lg.engine, durOr(lg.cold.wall), rssOr(lg.cold.rss), durOr(lg.warm.wall), rssOr(lg.warm.rss))
			if lg.cold.first > 0 {
				fmt.Printf("  first %s  load/compile %s", fmtDur(lg.cold.first), fmtDur(lg.cold.compile))
			}
			if lg.warm.heap != "" {
				fmt.Printf("  lua-heap %sKB", lg.warm.heap)
			}
			if f := strings.Fields(lg.warm.gomem); len(f) == 3 {
				fmt.Printf("  goheap heapsys=%s sys=%s live=%s", fmtRSS(atoi64(f[0])), fmtRSS(atoi64(f[1])), fmtRSS(atoi64(f[2])))
			}
			if lg.warm.used1 > 0 {
				fmt.Printf("  used %s→%s", fmtRSS(lg.warm.used1), fmtRSS(lg.warm.usedn))
			}
			fmt.Println()
			switch {
			case lg.warm.ret != "":
				rets[lg.engine] = lg.warm.ret
			case lg.cold.ret != "":
				rets[lg.engine] = lg.cold.ret
			}
		}
		ref := normRet(rets["clua-reuse"])
		for _, e := range []string{"clua-fresh", "glua-cold", "glua-fresh", "glua-pool"} {
			if normRet(rets[e]) != ref {
				fmt.Printf("  RET MISMATCH %s: %q vs clua-reuse %q\n", e, rets[e], rets["clua-reuse"])
			}
		}
	}
	emitMarkdown(rows)
	fmt.Println()
}

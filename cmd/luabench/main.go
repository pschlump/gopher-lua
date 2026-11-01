// Command luabench measures the interpreter and the production EVAL
// postures for high-frequency scripts — the P0 measurement rig (Option-A
// plan §5), ported from glasm's cmd/luabench (same scripts, same
// benchIt cadence, so glasm's recorded numbers are a usable historical
// reference). Interp-only: this repo IS the native engine, so the wasm
// and wazero-prod legs of the glasm harness are gone and the postures
// are:
//
//	interp TOTAL (fork)   — fresh LState + KEYS/ARGV staging + DoString + Close
//	cold: fresh VM/run    — e.Run(): a fresh VM per run (no VM cache)
//	warm: pooled VM rerun — one VM, vm.Run() reruns (the daemon steady state)
//	vmcache: engine cache  — e.Run against the WithVMCacheSize pool (primed)
//
// Scripts return values (no print) so the interp and host legs can be
// output-checked against each other. h_hostfn exercises the hostfn
// seam via bench.call, registered identically on the interp and host
// legs (an LState function vs host.RegisterGlobal).
//
// Flags:
//
//	-benchstat    also emit one go-benchmark line per (script, posture) —
//	              run the binary repeatedly, appending to one file per
//	              tree; benchstat old.txt new.txt compares
//	-cpuprofile D write per-(script, posture) CPU profiles into D — timing
//	              numbers are NOT comparable while profiling
//	-only SUBSTR  restrict to scripts whose name contains SUBSTR
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	lua "github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/host"
)

//go:embed scripts/*.lua
var scriptFS embed.FS

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "luabench: fatal:", err)
		os.Exit(1)
	}
}

type stats struct {
	n              int
	min, med, mean time.Duration
}

func benchIt(fn func(), maxIters int) stats {
	for i := 0; i < 2; i++ {
		fn()
	}
	runtime.GC()
	t0 := time.Now()
	fn()
	probe := time.Since(t0)
	n := int(1200*time.Millisecond/max(probe, time.Microsecond)) + 1
	if n < 3 {
		n = 3
	}
	if n > maxIters {
		n = maxIters
	}
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		runtime.GC()
		t0 = time.Now()
		fn()
		ds = append(ds, time.Since(t0))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	sum := time.Duration(0)
	for _, d := range ds {
		sum += d
	}
	return stats{n: n, min: ds[0], med: ds[len(ds)/2], mean: sum / time.Duration(n)}
}

func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%7.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%7.2fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%7.2fs ", float64(d)/float64(time.Second))
	}
}

// benchFn is the h_hostfn host function: string down, integer back —
// the redis.call shape in miniature. The same computation is registered
// on the interpreter leg (an LState function, staged as LInteger — the
// N5 integer-staging contract) and the host legs (host.RegisterGlobal
// returning host.Int), so both legs return identical results.
func benchFn(key string) int64 { return int64(len(key)) }

// interpOnce: fresh state + KEYS/ARGV staging (what host/ stages per
// run) + bench.call when needed + DoString + Close, collecting return
// values.
func interpOnce(src []byte, needsBench bool) string {
	L := lua.NewState()
	L.SetGlobal("KEYS", L.NewTable())
	L.SetGlobal("ARGV", L.NewTable())
	if needsBench {
		t := L.NewTable()
		t.RawSetString("call", L.NewFunction(func(L *lua.LState) int {
			key := L.CheckString(1)
			L.Push(lua.LInteger(benchFn(key)))
			return 1
		}))
		L.SetGlobal("bench", t)
	}
	must(L.DoString(string(src)))
	vals := make([]string, 0, L.GetTop())
	for i := 1; i <= L.GetTop(); i++ {
		vals = append(vals, L.Get(i).String())
	}
	L.Close()
	return strings.Join(vals, "\n")
}

func hostResultString(res host.Result) string {
	vals := make([]string, 0, len(res.Values))
	for _, v := range res.Values {
		vals = append(vals, v.String())
	}
	return strings.Join(vals, "\n")
}

type scriptDef struct {
	name    string // under scripts/
	maxCold int
	needsB  bool // registers bench.call (interp + host legs)
}

// The glasm seven (U7c EVAL shapes) plus the U9 call-path lanes: h_fib
// is the call-path worst case, h_nsieve the table-heavy lane, h_callret
// isolates the per-call seam without recursion, h_tco rides the staged
// tailcall — exactly the lanes P1 (register windows) and P2 (table
// engine) must move.
var scripts = []scriptDef{
	{"h_tiny.lua", 40, false},
	{"h_str.lua", 40, false},
	{"h_strlong.lua", 4, false},
	{"h_numloop.lua", 8, false},
	{"h_tablechurn.lua", 20, false},
	{"h_concat.lua", 20, false},
	{"h_hostfn.lua", 40, true},
	{"h_fib.lua", 2, false},
	{"h_nsieve.lua", 4, false},
	{"h_callret.lua", 8, false},
	{"h_tco.lua", 8, false},
}

var (
	benchstat = flag.Bool("benchstat", false, "emit go-benchmark lines for benchstat")
	cpuprof   = flag.String("cpuprofile", "", "per-(script, posture) CPU profiles into this dir (timings not comparable)")
	only      = flag.String("only", "", "restrict to scripts whose name contains this substring")
)

// measure runs benchIt under an optional CPU profile and prints the row.
func measure(label, stem string, fn func(), maxIters int) stats {
	if *cpuprof != "" {
		os.MkdirAll(*cpuprof, 0o755)
		f, err := os.Create(fmt.Sprintf("%s/%s-%s.pprof", *cpuprof, stem, label))
		must(err)
		must(pprof.StartCPUProfile(f))
		st := benchIt(fn, maxIters)
		pprof.StopCPUProfile()
		must(f.Close())
		return st
	}
	return benchIt(fn, maxIters)
}

func emitBench(stem, posture string, st stats) {
	if !*benchstat {
		return
	}
	fmt.Printf("BenchmarkLuabench/%s/%s-%d  %d  %d ns/op\n",
		stem, posture, runtime.GOMAXPROCS(0), st.n, st.mean.Nanoseconds())
}

// regBenchCall registers the bench.call host function (h_hostfn lane).
func regBenchCall(e *host.Engine, needsB bool) {
	if !needsB {
		return
	}
	must(e.RegisterGlobal("bench", "call", func(vm *host.VM, args []host.Value) ([]host.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("bench.call wants 1 argument, got %d", len(args))
		}
		return []host.Value{host.Int(benchFn(args[0].String()))}, nil
	}))
}

func main() {
	flag.Parse()
	ctx := context.Background()
	fmt.Printf("# luabench — %s | native engine (this repo) | GOMAXPROCS=%d | %s\n",
		runtime.Version(), runtime.GOMAXPROCS(0), time.Now().Format("2006-01-02 15:04"))
	if *cpuprof != "" {
		fmt.Println("# (CPU profiling on — timing numbers in this run are NOT comparable)")
	}

	for _, sc := range scripts {
		stem := strings.TrimSuffix(sc.name, ".lua")
		if *only != "" && !strings.Contains(stem, *only) {
			continue
		}
		src, err := scriptFS.ReadFile("scripts/" + sc.name)
		must(err)

		e := host.NewEngine()
		regBenchCall(e, sc.needsB)
		s, err := e.Compile(src, sc.name)
		must(err)

		// output check: interp vs host
		want := interpOnce(src, sc.needsB)
		res, err := e.Run(ctx, s, host.RunOptions{})
		must(err)
		got := hostResultString(res)
		check := "OK"
		if want != got {
			check = fmt.Sprintf("MISMATCH! interp=%q host=%q", want, got)
		}

		fmt.Printf("\n=== %s  (%d bytes)  outputs: %s\n", sc.name, len(src), check)
		fmt.Printf("%-26s %5s  %9s  %9s  %9s\n", "posture", "n", "min", "median", "mean")

		st := measure("interp", stem, func() { interpOnce(src, sc.needsB) }, 400)
		fmt.Printf("%-26s %5d  %s  %s  %s\n", "interp TOTAL (fork)", st.n, fmtDur(st.min), fmtDur(st.med), fmtDur(st.mean))
		emitBench(stem, "interp", st)

		st = measure("cold", stem, func() {
			_, err := e.Run(ctx, s, host.RunOptions{})
			must(err)
		}, sc.maxCold)
		fmt.Printf("%-26s %5d  %s  %s  %s\n", "cold: fresh VM/run", st.n, fmtDur(st.min), fmtDur(st.med), fmtDur(st.mean))
		emitBench(stem, "cold", st)

		// pooled: one VM bound to this script, rerun in place
		vm, err := e.NewVM()
		must(err)
		st = measure("warm", stem, func() {
			_, err := vm.Run(ctx, s, host.RunOptions{})
			must(err)
		}, 400)
		fmt.Printf("%-26s %5d  %s  %s  %s\n", "warm: pooled VM rerun", st.n, fmtDur(st.min), fmtDur(st.med), fmtDur(st.mean))
		emitBench(stem, "warm", st)
		vm.Close()

		// engine VM-cache (WithVMCacheSize): e.Run reruns against a
		// pooled warm VM. One priming run pays the first VM; the
		// measured iterations are pool hits — the daemon's steady-state
		// EVAL shape.
		ep := host.NewEngine(host.WithVMCacheSize(8))
		regBenchCall(ep, sc.needsB)
		sp, err := ep.Compile(src, sc.name)
		must(err)
		_, err = ep.Run(ctx, sp, host.RunOptions{})
		must(err)
		st = measure("vmcache", stem, func() {
			_, err := ep.Run(ctx, sp, host.RunOptions{})
			must(err)
		}, 400)
		fmt.Printf("%-26s %5d  %s  %s  %s\n", "vmcache: engine cache", st.n, fmtDur(st.min), fmtDur(st.med), fmtDur(st.mean))
		emitBench(stem, "vmcache", st)
		ep.Close()
		e.Close()
	}
	fmt.Println()
}

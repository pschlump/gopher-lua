package lua

// Golden-corpus runner (N3a, docs/Option-A-Plan.md §4): executes one
// corpus case in a fresh LState and produces the canonical event log.
// The conventions are glasm's testdiff Interp engine's, verbatim —
// PRINT/ERROR/RESULTS/GLOBALS lines, the deterministic os shims, and the
// sorted-table serialization (testdiff/interp.go + normalize.go; the
// gate history is docs/glasm-history/) — so the checked-in goldens are a
// standing byte-exact regression law with no C and no wasm dependency.
// Any change to the rules below invalidates every golden and must be
// deliberate (mirroring normalize.go's contract comment).

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type goldenCase struct {
	Name   string
	Dir    string
	Source []byte
}

// loadGoldenCorpus mirrors testdiff.LoadCorpus: top-level *.lua files of
// the corpus dir (subdirectories are library/support material), sorted by
// name, each run with the corpus dir as working directory.
func loadGoldenCorpus(dir string) ([]goldenCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []goldenCase
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		cases = append(cases, goldenCase{Name: e.Name(), Dir: abs, Source: src})
	}
	return cases, nil
}

// goldenRun executes one case and returns its event log. The per-case
// deadline is defensive only (the hang-shaped skip rows are excluded from
// the corpus); a case that trips it fails the gate with a deadline error
// line instead of wedging the suite.
func goldenRun(c goldenCase, suiteFlags bool) []string {
	var log []string
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	defer os.Chdir(wd)

	L := NewState(Options{
		RegistrySize:        1024 * 20,
		CallStackSize:       1024,
		IncludeGoStackTrace: true,
	})
	defer L.Close()
	// The per-case context is a HANG GUARD (closure.lua-class hangs
	// convert to an error, not a stuck suite), not a semantic gate —
	// 60s holds on a dev machine but a 2-vCPU shared CI runner runs the
	// heavyweight cases (calls.lua's binary-chunk leg) 5–10× slower, so
	// the timeout is env-tunable and CI sets it generously.
	caseTimeout := 60 * time.Second
	if v := os.Getenv("GOLDEN_CASE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			caseTimeout = time.Duration(n) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
	defer cancel()
	L.SetContext(ctx)

	goldenShim(L, emit)
	if suiteFlags {
		// all.lua's "user tests" posture: _port/_soft/_nomsg set as
		// globals BEFORE the chunk (testdiff's SuiteFlags — line numbers
		// match the C driver's prelude-chunk posture)
		L.SetGlobal("_port", LTrue)
		L.SetGlobal("_soft", LTrue)
		L.SetGlobal("_nomsg", LTrue)
	}
	// arg = { [0] = chunkname } — the standalone-interpreter surface the
	// engine contract seeds (the compat `arg` LOCAL is separate).
	argt := L.NewTable()
	argt.RawSetInt(0, LString(c.Name))
	L.SetGlobal("arg", argt)
	defer func() {
		matches, _ := filepath.Glob(filepath.Join(c.Dir, "testdiff.tmp.*"))
		for _, m := range matches {
			os.Remove(m)
		}
	}()

	fn, err := L.Load(strings.NewReader(string(c.Source)), "@"+c.Name)
	if err != nil {
		emit("ERROR", ValueRepr(LString(err.Error()), 0, map[*LTable]bool{}))
		emit("GLOBALS", normalizeVersionGlobal(goldenGlobals(L)))
		return log
	}
	resBase := L.GetTop()
	L.Push(fn)
	if err := L.PCall(0, MultRet, nil); err != nil {
		// The uncaught error value: PCall returns it as a Go error whose
		// message is the Lua error's string form (with position prefix).
		emit("ERROR", ValueRepr(LString(err.Error()), 0, map[*LTable]bool{}))
	} else {
		// U9a2: chunk returns enter the differential surface — the count
		// is its own field so trailing-nil and truncation drift diffs even
		// when the rendered values agree.
		n := L.GetTop() - resBase
		parts := make([]string, 0, n)
		for i := 1; i <= n; i++ {
			parts = append(parts, ValueRepr(L.Get(resBase+i), 0, map[*LTable]bool{}))
		}
		out := strconv.Itoa(n)
		for _, p := range parts {
			out += "\t" + p
		}
		emit("RESULTS", out)
	}
	emit("GLOBALS", normalizeVersionGlobal(goldenGlobals(L)))
	return log
}

// normalizeVersionGlobal folds the identity global's value to a
// placeholder: PackageVersion changes on every release (Makefile -X /
// package.go literal), and an unpinned value would rewrite every golden's
// GLOBALS line on each bump.
func normalizeVersionGlobal(s string) string {
	return strings.Replace(s,
		`"_GOPHER_LUA_VERSION"="`+PackageName+" "+PackageVersion+`"`,
		`"_GOPHER_LUA_VERSION"="<version>"`, 1)
}

func goldenGlobals(L *LState) string {
	return serializeTable(L.GetGlobal("_G").(*LTable), 0, map[*LTable]bool{})
}

// goldenShim replaces every nondeterministic surface with a deterministic
// equivalent and hooks print into the event log (testdiff's installShim;
// math.random is NOT shimmed — the mathlib speaks stock xoshiro256** over
// the makeseed=0 determinism pin).
func goldenShim(L *LState, emit func(event, payload string)) {
	// print → event log
	printFn := func(L *LState) int {
		top := L.GetTop()
		parts := make([]string, 0, top)
		for i := 1; i <= top; i++ {
			parts = append(parts, PrintArg(L.Get(i)))
		}
		emit("PRINT", strings.Join(parts, "\t"))
		return 0
	}
	L.SetGlobal("print", L.NewFunction(printFn))

	// os.getenv/setenv: pure map, seeded the same everywhere
	env := map[string]string{"PATH": "/bin:/usr/bin"}
	getenv := func(L *LState) int {
		v, ok := env[L.CheckString(1)]
		if !ok {
			L.Push(LNil)
		} else {
			L.Push(LString(v))
		}
		return 1
	}
	setenv := func(L *LState) int {
		env[L.CheckString(1)] = L.CheckString(2)
		L.Push(LTrue)
		return 1
	}
	L.SetField(L.GetGlobal("os"), "getenv", L.NewFunction(getenv))
	L.SetField(L.GetGlobal("os"), "setenv", L.NewFunction(setenv))

	// os.execute: constant exit status (no shelling out in a gate)
	execStub := func(L *LState) int {
		L.Push(LNumber(1))
		return 1
	}
	L.SetField(L.GetGlobal("os"), "execute", L.NewFunction(execStub))

	// os.time/clock/date: the pinned-instant semantics (no-arg time = the
	// pinned epoch, table form does real UTC calendar arithmetic, date
	// formats the fixed 2000-01-01 00:00:00 UTC instant — Saturday).
	constStub := func(v float64) func(*LState) int {
		return func(L *LState) int {
			L.Push(LNumber(v))
			return 1
		}
	}
	osTime := func(L *LState) int {
		if L.GetTop() > 0 && L.Get(1).Type() == LTTable {
			t := L.Get(1).(*LTable)
			geti := func(name string, def int) int {
				if v := L.GetField(t, name); v.Type() == LTNumber {
					return int(v.(LNumber))
				}
				return def
			}
			year := geti("year", 0)
			month := geti("month", 1)
			day := geti("day", 1)
			hour := geti("hour", 12)
			min := geti("min", 0)
			sec := geti("sec", 0)
			t.RawSetString("hour", LInteger(hour))
			t.RawSetString("min", LInteger(min))
			t.RawSetString("sec", LInteger(sec))
			t.RawSetString("isdst", LBool(false))
			days := daysFromCivil(year, month, day)
			L.Push(LNumber(days*86400 + int64(hour)*3600 + int64(min)*60 + int64(sec)))
			return 1
		}
		L.Push(LNumber(946684800)) // 2000-01-01 00:00:00 UTC
		return 1
	}
	L.SetField(L.GetGlobal("os"), "time", L.NewFunction(osTime))
	L.SetField(L.GetGlobal("os"), "clock", L.NewFunction(constStub(0)))
	L.SetField(L.GetGlobal("os"), "date", L.NewFunction(goldenDateShim))

	// os.tmpname: unique per call (files that rename tmpname() A to
	// tmpname() B must not rename onto itself), deterministic per run;
	// leftovers are removed after the run.
	tmpCounter := 0
	L.SetField(L.GetGlobal("os"), "tmpname", L.NewFunction(func(L *LState) int {
		tmpCounter++
		L.Push(LString(fmt.Sprintf("testdiff.tmp.%d", tmpCounter)))
		return 1
	}))
}

// daysFromCivil: days since 1970-01-01 from a civil date (Howard
// Hinnant's algorithm).
func daysFromCivil(y, m, d int) int64 {
	yy := int64(y)
	if m <= 2 {
		yy--
	}
	era := yy / 400
	if yy < 0 && yy%400 != 0 {
		era-- // floor division for negatives
	}
	yoe := yy - era*400
	var mp int
	if m > 2 {
		mp = m - 3
	} else {
		mp = m + 9
	}
	doy := (153*int64(mp)+2)/5 + int64(d) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// goldenDateShim: os.date over the pinned 2000-01-01 00:00:00 UTC instant
// — "*t" gives the fixed table; string forms walk a strftime subset with
// fixed outputs (the time argument is ignored).
func goldenDateShim(L *LState) int {
	f := "%c"
	if L.GetTop() > 0 && L.Get(1).Type() == LTString {
		f = L.Get(1).String()
	}
	f = strings.TrimPrefix(f, "!") // UTC == local at the pinned instant
	if f == "*t" {
		t := L.NewTable()
		t.RawSetString("year", LInteger(2000))
		t.RawSetString("month", LInteger(1))
		t.RawSetString("day", LInteger(1))
		t.RawSetString("hour", LInteger(0))
		t.RawSetString("min", LInteger(0))
		t.RawSetString("sec", LInteger(0))
		t.RawSetString("wday", LInteger(7))
		t.RawSetString("yday", LInteger(1))
		t.RawSetString("isdst", LBool(false))
		L.Push(t)
		return 1
	}
	fixed := map[byte]string{
		'Y': "2000", 'y': "00", 'm': "01", 'd': "01", 'H': "00", 'M': "00",
		'S': "00", 'j': "001", 'w': "6", 'W': "00", 'U': "00", 'p': "AM",
		'A': "Saturday", 'a': "Sat", 'B': "January", 'b': "Jan", 'h': "Jan",
		'c': "Sat Jan  1 00:00:00 2000", 'x': "01/01/00", 'X': "00:00:00",
	}
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		if f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		if v, ok := fixed[f[i]]; ok {
			b.WriteString(v)
		} else {
			b.WriteByte('%')
			b.WriteByte(f[i])
		}
	}
	L.Push(LString(b.String()))
	return 1
}

/* canonical value formatting (testdiff/normalize.go, verbatim) {{{ */

const (
	maxDepth   = 8   // table serialization depth cap
	maxEntries = 512 // entries per serialized table
)

// NumRepr formats a Lua number canonically: %.14g with C-style inf/nan.
func NumRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', 14, 64)
}

// PrintArg normalizes one print() argument using Lua print semantics:
// strings print raw, other values use their tostring form, tables never
// leak addresses. (Blindness of print-diffing: "1" and 1 print the same;
// corpus asserts cover those distinctions.)
func PrintArg(v LValue) string {
	switch x := v.(type) {
	case LString:
		return string(x)
	case LNumber:
		return NumRepr(float64(x))
	case LInteger:
		// integers render lossless; equals NumRepr below 1e15, correct
		// above — where the f64 detour printed 14-sig-fig 'g' forms
		return strconv.FormatInt(int64(x), 10)
	case *LNilType:
		return "nil"
	case LBool:
		if bool(x) {
			return "true"
		}
		return "false"
	case *LTable:
		return "<table>"
	case *LFunction:
		return "<function>"
	case *LUserData:
		return "<userdata>"
	case *LState:
		return "<thread>"
	case LChannel:
		return "<channel>"
	}
	return fmt.Sprintf("<%s>", v.Type().String())
}

// ValueRepr is the full serialization form used for globals and error
// values: strings quoted, tables recursively serialized with sorted keys.
func ValueRepr(v LValue, depth int, seen map[*LTable]bool) string {
	switch x := v.(type) {
	case LString:
		return strconv.Quote(string(x))
	case LNumber:
		return NumRepr(float64(x))
	case LInteger:
		return strconv.FormatInt(int64(x), 10)
	case *LNilType:
		return "nil"
	case LBool:
		if bool(x) {
			return "true"
		}
		return "false"
	case *LTable:
		return serializeTable(x, depth, seen)
	case *LFunction:
		return "<function>"
	case *LUserData:
		return "<userdata>"
	case *LState:
		return "<thread>"
	case LChannel:
		return "<channel>"
	}
	return fmt.Sprintf("<%s>", v.Type().String())
}

// serializeTable renders a table deterministically:
//   - entries sorted by normalized key text (byte order)
//   - cycles rendered as <cycle> (DFS marking), depth capped at maxDepth
//   - at most maxEntries entries, then a plain <...> marker
func serializeTable(t *LTable, depth int, seen map[*LTable]bool) string {
	if seen[t] {
		return "<cycle>"
	}
	if depth >= maxDepth {
		return "<deep>"
	}
	seen[t] = true
	defer delete(seen, t)

	type kv struct{ k, v string }
	var entries []kv
	truncated := false
	t.ForEach(func(k, val LValue) {
		if len(entries) >= maxEntries {
			truncated = true
			return
		}
		entries = append(entries, kv{ValueRepr(k, depth+1, seen), ValueRepr(val, depth+1, seen)})
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].k < entries[j].k })

	out := "{"
	for i, e := range entries {
		if i > 0 {
			out += ", "
		}
		out += e.k + "=" + e.v
	}
	if truncated {
		if len(entries) > 0 {
			out += ", "
		}
		out += "<...>"
	}
	return out + "}"
}

/* }}} */

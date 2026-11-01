package host

// N1 gates: the run protocol, host functions, caps, deadline, kill,
// determinism, and the A9 concurrency contract — the last under -race,
// which is the CI requirement. Shapes port from glasm's host_test.go;
// expectations that were wasm-seam residuals (staging-buffer caps, the
// row-85b error prefix) are re-pinned to the native contract.

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	e := NewEngine(opts...)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func mustCompile(t *testing.T, e *Engine, src string) *Script {
	t.Helper()
	s, err := e.Compile([]byte(src), "=test")
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return s
}

func runScript(t *testing.T, e *Engine, src string, opt RunOptions) (Result, error) {
	t.Helper()
	s := mustCompile(t, e, src)
	return e.Run(context.Background(), s, opt)
}

// engine returns the VM's engine (test helper for the reentrancy probe).
func (vm *VM) engine() *Engine { return vm.e }

// --- basics: compile, run, results, KEYS/ARGV ------------------------------

func TestEvalBasics(t *testing.T) {
	e := mustEngine(t)
	cases := []struct {
		src  string
		want []Value
	}{
		{"return 1+1", []Value{Int(2)}},
		{"return 'hello'", []Value{String("hello")}},
		{"return 3.5", []Value{Number(3.5)}},
		{"return true", []Value{Bool(true)}},
		{"return nil", []Value{Nil()}},
		{"return", nil},
		{"return 1, 'two', true, nil", []Value{Int(1), String("two"), Bool(true), Nil()}},
		{"return KEYS[1]..ARGV[1]", []Value{String("ab")}},
		{"return #KEYS, #ARGV", []Value{Int(1), Int(2)}},
		{"return {1, 2, {3, 'four'}}", []Value{Table(
			KV{Key: Int(1), Val: Int(1)},
			KV{Key: Int(2), Val: Int(2)},
			KV{Key: Int(3), Val: Table(
				KV{Key: Int(1), Val: Int(3)},
				KV{Key: Int(2), Val: String("four")},
			)},
		)}},
		{"local function f(x) return x*2 end return f(21)", []Value{Int(42)}},
		{"local function f(n) if n < 2 then return n end return f(n-1) + f(n-2) end return f(10)", []Value{Int(55)}},
		{"local function f(...) return ... end return f(42)", []Value{Int(42)}},
		{"local function f(x) return x*2 end return f(1), f(2)", []Value{Int(2), Int(4)}},
		{"local function f(x) return x*2 end return tostring(f(21))", []Value{String("42")}},
		{"local function f() end return f()", nil},
		{"return (function(x) return x+1 end)(41)", []Value{Int(42)}},
	}
	for _, c := range cases {
		res, err := runScript(t, e, c.src, RunOptions{Keys: []string{"a"}, Argv: []string{"b", "c"}})
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if len(res.Values) != len(c.want) {
			t.Errorf("%q: got %d values, want %d", c.src, len(res.Values), len(c.want))
			continue
		}
		for i := range c.want {
			if !res.Values[i].Equal(c.want[i]) {
				t.Errorf("%q: value %d: got %#v, want %#v", c.src, i, res.Values[i], c.want[i])
			}
		}
	}
}

// binary-safety of KEYS/ARGV staging (control bytes, quotes, UTF-8-ish
// bytes, embedded NULs — the strings cross as strings, no quoting seam)
func TestEvalBinaryArgs(t *testing.T) {
	e := mustEngine(t)
	res, err := runScript(t, e, `return KEYS[1]..ARGV[1]`, RunOptions{
		Keys: []string{"a\x00\xff\"b\\"},
		Argv: []string{"\n\t\r\x01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Values[0].Str
	want := "a\x00\xff\"b\\\n\t\r\x01"
	if got != want {
		t.Fatalf("binary round-trip: got %q, want %q", got, want)
	}
}

func TestCompileErrors(t *testing.T) {
	e := mustEngine(t)
	if _, err := e.Compile([]byte("return (("), "=bad"); err == nil {
		t.Fatal("syntax error not reported")
	}
	// compile errors carry the frontend's message verbatim
	if _, err := e.Compile([]byte("return (("), "=bad"); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("syntax error message: %v", err)
	}
}

func TestScriptRuntimeError(t *testing.T) {
	e := mustEngine(t)
	_, err := runScript(t, e, `error("boom")`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.ErrValue.String(), "boom") {
		t.Fatalf("error value %q missing boom", se.ErrValue.String())
	}
	if se.Line != 1 {
		t.Fatalf("raise line: got %d, want 1 (%q)", se.Line, se.ErrValue.String())
	}
	// pcall catches; ok=false. Stock semantics: error("inner", 0) raises
	// the bare message (glasm's "=test:1: inner" was a wasm-seam residual,
	// row 85b — the interpreter is byte-exact stock here).
	res, err := runScript(t, e, `local ok, e = pcall(function() error("inner", 0) end) return ok, e`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].BoolVal() || res.Values[1].Str != "inner" {
		t.Fatalf("pcall round-trip: %#v", res.Values)
	}
}

// --- host functions (the redis.call seam) ----------------------------------

func registerEcho(t *testing.T, e *Engine) {
	t.Helper()
	err := e.RegisterGlobal("redis", "call", func(vm *VM, args []Value) ([]Value, error) {
		return args, nil // echo
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHostFunctionRoundTrip(t *testing.T) {
	e := mustEngine(t)
	registerEcho(t, e)
	res, err := runScript(t, e,
		`return redis.call('SET', KEYS[1], 42, true)`, RunOptions{Keys: []string{"k"}})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Values
	want := []Value{String("SET"), String("k"), Int(42), Bool(true)}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("arg %d: got %#v want %#v", i, got[i], want[i])
		}
	}
}

func TestHostFunctionReturns(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "getall", func(vm *VM, args []Value) ([]Value, error) {
		return []Value{
			Table(KV{Key: String("ok"), Val: String("fine")}),                     // status-reply shape
			Table(KV{Key: Int(1), Val: String("a")}, KV{Key: Int(2), Val: Nil()}), // array with hole
			Nil(), Number(1.25), Bool(false),
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := runScript(t, e, `
		local t, arr = redis.getall()
		return t.ok, arr[1], arr[2], #arr, 1.25, false
	`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].Str != "fine" || res.Values[1].Str != "a" || res.Values[2].Kind != KindNil {
		t.Fatalf("table return: %#v", res.Values)
	}
	// arr[2] = nil is an ABSENT key in Lua — the table has one entry
	if !res.Values[3].Equal(Int(1)) {
		t.Fatalf("array length: %#v", res.Values[3])
	}
}

func TestHostFunctionErrors(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "call", func(vm *VM, args []Value) ([]Value, error) {
		return nil, errFake("ERR wrong number of arguments for 'get' command")
	}); err != nil {
		t.Fatal(err)
	}
	// redis.call semantics: the error aborts the script, text verbatim
	// (bare lua_error — no position prefix, the rt_sea_raise_nopos
	// contract carried over natively)
	_, err := runScript(t, e, `redis.call('GET')`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if se.ErrValue.Str != "ERR wrong number of arguments for 'get' command" {
		t.Fatalf("verbatim error text: %q", se.ErrValue.Str)
	}
	// pcall catches it as a string value
	res, err := runScript(t, e, `local ok, e = pcall(redis.call, 'GET') return ok, e`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].BoolVal() || res.Values[1].Str != "ERR wrong number of arguments for 'get' command" {
		t.Fatalf("pcall of failing hostfn: %#v", res.Values)
	}
}

// ValueError raises a non-string error value crossing verbatim.
func TestHostFunctionValueError(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "fail", func(vm *VM, args []Value) ([]Value, error) {
		return nil, &ValueError{V: Table(KV{Key: String("err"), Val: String("ERR table error")})}
	}); err != nil {
		t.Fatal(err)
	}
	res, err := runScript(t, e,
		`local ok, e = pcall(redis.fail) return ok, type(e), e.err`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].BoolVal() || res.Values[1].Str != "table" || res.Values[2].Str != "ERR table error" {
		t.Fatalf("ValueError round-trip: %#v", res.Values)
	}
	// uncaught: the ScriptError carries the table value itself
	_, err = runScript(t, e, `redis.fail()`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if se.ErrValue.Kind != KindTable || len(se.ErrValue.Pairs) != 1 {
		t.Fatalf("uncaught ValueError value: %#v", se.ErrValue)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestHostFunctionPanicContained(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "boom", func(vm *VM, args []Value) ([]Value, error) {
		panic("kaboom")
	}); err != nil {
		t.Fatal(err)
	}
	_, err := runScript(t, e, `redis.boom()`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.ErrValue.String(), "host function panic: kaboom") {
		t.Fatalf("panic containment: %q", se.ErrValue.String())
	}
}

// Trampolines execute on the lock-holding goroutine (A9): the VM mutex
// must be held for the whole trampoline.
func TestTrampolinesHoldImageLock(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "call", func(vm *VM, args []Value) ([]Value, error) {
		if vm.mu.TryLock() {
			vm.mu.Unlock()
			return nil, errFake("FATAL: VM lock NOT held during hostfn trampoline")
		}
		return []Value{String("ok")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := runScript(t, e, `return redis.call('PING')`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].Str != "ok" {
		t.Fatalf("lock assertion fired: %#v", res.Values)
	}
}

// A HostFunc re-entering its own VM must get ErrReentrant as a clean
// script error, not a deadlock.
func TestHostFunctionReentrancyRefused(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "nest", func(vm *VM, args []Value) ([]Value, error) {
		s, err := vm.engine().Compile([]byte("return 7"), "=nested")
		if err != nil {
			return nil, err
		}
		res, err := vm.Run(context.Background(), s, RunOptions{})
		if err != nil {
			return nil, err
		}
		return res.Values, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := runScript(t, e, `return redis.nest()`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), ErrReentrant.Error()) {
		t.Fatalf("reentrancy refusal: %q", se.Error())
	}
}

// --- chaos host: the seam caps are clean script errors, never panics --------

func TestChaosHost(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "chaos", func(vm *VM, args []Value) ([]Value, error) {
		switch args[0].Str {
		case "many":
			vals := make([]Value, 5000) // over the 4096 frame cap
			for i := range vals {
				vals[i] = Nil()
			}
			return vals, nil
		case "deep":
			v := Nil()
			for i := 0; i < 100; i++ { // over the depth-8 expansion cap
				v = Table(KV{Key: Int(1), Val: v})
			}
			return []Value{v}, nil
		case "cycle":
			return []Value{args[1]}, nil // echoes its (cyclic) second argument back
		case "wide":
			pairs := make([]KV, 600) // over the 512-entry table cap
			for i := range pairs {
				pairs[i] = KV{Key: Int(int64(i + 1)), Val: Int(int64(i + 1))}
			}
			return []Value{Table(pairs...)}, nil
		case "big":
			return []Value{String(strings.Repeat("x", 300 << 10))}, nil // no staging buffer natively
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// clean refusals (pcall-catchable, engine unharmed): too many
	// results, a cyclic table echoed back, an over-cap table, and a
	// 100-deep host-built tree (the guest-decode depth guard, restated)
	for _, src := range []string{
		`return redis.chaos('many')`,
		`local t = {} t[1] = t return redis.chaos('cycle', t)`,
		`return redis.chaos('wide')`,
		`return redis.chaos('deep')`,
	} {
		_, err := runScript(t, e, src, RunOptions{})
		if _, ok := err.(*ScriptError); !ok {
			t.Errorf("%q: want clean ScriptError, got %T %v", src, err, err)
		}
	}
	// 'big': no wire staging cap exists — 300 KB crosses whole
	res, err := runScript(t, e, `return #redis.chaos('big')`, RunOptions{})
	if err != nil {
		t.Fatalf("big string: %v", err)
	}
	if !res.Values[0].Equal(Int(300 << 10)) {
		t.Fatalf("big string length: %#v", res.Values[0])
	}
}

// The argument-side expansion caps: depth-8/deep, 512+More, cycle —
// observed from inside the host function.
func TestArgExpansionCaps(t *testing.T) {
	e := mustEngine(t)
	var got []Value
	if err := e.RegisterGlobal("redis", "probe", func(vm *VM, args []Value) ([]Value, error) {
		got = args
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// deep: the expansion walks 8 levels; the depth-8 node crosses as
	// the KindDeep sentinel (the C encoder's per-node depth rule)
	if _, err := runScript(t, e,
		`local v = 1 for i=1,20 do v = {v} end redis.probe(v)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got[0].Kind != KindTable {
		t.Fatalf("deep arg root: %#v", got[0])
	}
	v := got[0]
	for i := 0; i < 8; i++ {
		if v.Kind != KindTable || len(v.Pairs) != 1 {
			t.Fatalf("chain level %d: %#v", i, v)
		}
		v = v.Pairs[0].Val
	}
	if v.Kind != KindDeep {
		t.Fatalf("depth-8 node: %#v", v)
	}
	// cycle: the reference lands as a KindCycle child (detection is
	// path-based, like the encoder's ancestor walk)
	if _, err := runScript(t, e,
		`local t = {} t.self = t redis.probe(t)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if got[0].Kind != KindTable || len(got[0].Pairs) != 1 || got[0].Pairs[0].Val.Kind != KindCycle {
		t.Fatalf("cyclic arg: %#v", got[0])
	}
	// wide: 512 pairs + More
	if _, err := runScript(t, e,
		`local t = {} for i=1,600 do t[i]=i end redis.probe(t)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(got[0].Pairs) != 512 || !got[0].More {
		t.Fatalf("wide arg: %d pairs, more=%v", len(got[0].Pairs), got[0].More)
	}
	// expansion order is lua_next order: array part first
	if !got[0].Pairs[0].Key.Equal(Int(1)) || !got[0].Pairs[1].Key.Equal(Int(2)) {
		t.Fatalf("iteration order: %#v", got[0].Pairs[:2])
	}
}

// --- deadline ----------------------------------------------------------------

// No host-GC quiescing here (glasm's noHostGC): the native loop is
// pure-Go and preemptible at every instruction — the row-99/100
// STW-wedge class cannot occur, and the deadline tests prove it under
// the ordinary scheduler.
func TestDeadlineKill(t *testing.T) {
	e := mustEngine(t)
	s := mustCompile(t, e, `while true do end`)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	start := time.Now()
	_, err = vm.Run(context.Background(), s, RunOptions{Deadline: 250 * time.Millisecond})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("deadline did not fire promptly (%v)", time.Since(start))
	}
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	// the bare contract the daemon keys on (RunOptions.Deadline, mapError):
	// no position, exactly this text — for the timer, Kill, and caller-ctx
	// cancel alike
	if se.ErrValue.Kind != KindString || se.ErrValue.Str != "context deadline exceeded" || se.Line != 0 {
		t.Fatalf("deadline contract: want (String \"context deadline exceeded\", 0), got (%v, %d)", se.ErrValue, se.Line)
	}
	// the VM is reusable: the run context died with the run, and the
	// same script can be killed again cleanly
	_, err = vm.Run(context.Background(), s, RunOptions{Deadline: 250 * time.Millisecond})
	se2, ok := err.(*ScriptError)
	if !ok || se2.ErrValue.Str != "context deadline exceeded" || se2.Line != 0 {
		t.Fatalf("second run after kill: %T %v — VM wedged", err, err)
	}
}

// The second half of the deadline contract: a bounded script under a
// generous deadline completes normally, and ctx cancellation raises the
// same clean kill.
func TestDeadlineNormalAndCancel(t *testing.T) {
	e := mustEngine(t)
	if _, err := runScript(t, e, `local s=0 for i=1,100000 do s=s+i end return s`,
		RunOptions{Deadline: 30 * time.Second}); err != nil {
		t.Fatalf("bounded script failed: %v", err)
	}
	s := mustCompile(t, e, `while true do end`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := e.Run(ctx, s, RunOptions{})
	if se, ok := err.(*ScriptError); ok {
		// the caller-ctx cancel rides the same normalized contract as the
		// timer and Kill (mapError): bare "context deadline exceeded", no
		// position — the manager distinguishes the cases via its own flags
		if se.ErrValue.Kind != KindString || se.ErrValue.Str != "context deadline exceeded" || se.Line != 0 {
			t.Fatalf("ctx cancel kill (script error): want the bare deadline contract, got (%v, %d)", se.ErrValue, se.Line)
		}
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx cancel kill: %T %v", err, err)
	}
}

// Kill is the SCRIPT KILL mechanism: it ends an in-flight run from
// another goroutine, without any deadline having been set.
func TestKill(t *testing.T) {
	e := mustEngine(t)
	s := mustCompile(t, e, `while true do end`)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	res := make(chan error, 1)
	go func() {
		_, rerr := vm.Run(context.Background(), s, RunOptions{})
		res <- rerr
	}()
	time.Sleep(150 * time.Millisecond)
	vm.Kill()
	select {
	case err := <-res:
		se, ok := err.(*ScriptError)
		// Kill rides the normalized deadline contract (mapError): bare
		// "context deadline exceeded", Line 0 — indistinguishable from the
		// timer at this layer by design; the manager's own flag tells them
		// apart
		if !ok || se.ErrValue.Kind != KindString || se.ErrValue.Str != "context deadline exceeded" || se.Line != 0 {
			t.Fatalf("kill: %T %v", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not end the run")
	}
}

// --- determinism ---------------------------------------------------------------

func TestDeterminism(t *testing.T) {
	e := mustEngine(t)
	src := `local t = {} for i = 1, 16 do t[i] = math.random(1, 1000000) end return t`
	var first *Result
	for i := 0; i < 5; i++ {
		res, err := runScript(t, e, src, RunOptions{Seed: 99})
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			r := res
			first = &r
			continue
		}
		for j := range first.Values[0].Pairs {
			// math.random yields integers — they cross as KindInteger
			a, b := first.Values[0].Pairs[j].Val, res.Values[0].Pairs[j].Val
			if a.Int != b.Int {
				t.Fatalf("run %d value %d diverged: %v vs %v", i, j, a.Int, b.Int)
			}
		}
	}
	// a different seed must (virtually certainly) move the stream
	res2, err := runScript(t, e, src, RunOptions{Seed: 100})
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for j := range res2.Values[0].Pairs {
		if first.Values[0].Pairs[j].Val.Int != res2.Values[0].Pairs[j].Val.Int {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seed produced identical stream")
	}
}

// The unseeded posture: fresh VMs start at the build-pinned open seed,
// so unseeded runs are reproducible across VMs (the luai_makeseed()=0
// posture); Seed=0 keeps an existing stream running on the same VM.
func TestDefaultSeedDeterminism(t *testing.T) {
	e := mustEngine(t)
	src := `local a, b = math.random(), math.random(1, 1000) return a, b`
	r1, err := runScript(t, e, src, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := runScript(t, e, src, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Values[0].Equal(r2.Values[0]) || !r1.Values[1].Equal(r2.Values[1]) {
		t.Fatalf("unseeded fresh VMs diverged: %#v vs %#v", r1.Values, r2.Values)
	}
	// same VM, no reseed: the stream continues (values move)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	s := mustCompile(t, e, src)
	a, err := vm.Run(context.Background(), s, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := vm.Run(context.Background(), s, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Values[0].Equal(b.Values[0]) {
		t.Fatal("same-VM stream restarted without a seed")
	}
}

// --- concurrency (A9) — the -race CI requirement -------------------------------

func TestConcurrentFreshVMs(t *testing.T) {
	e := mustEngine(t)
	const W, N = 8, 15 // goroutines × runs
	var wg sync.WaitGroup
	errs := make(chan error, W*N)
	for w := 0; w < W; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s, err := e.Compile([]byte(strings.Repeat("local x = 1 ", 50)+
				`return KEYS[1]..':'..ARGV[1]..':'..#KEYS`), "=w")
			if err != nil {
				errs <- err
				return
			}
			for i := 0; i < N; i++ {
				res, err := e.Run(context.Background(), s, RunOptions{
					Keys: []string{"k" + string(rune('a'+w))},
					Argv: []string{string(rune('A' + w))},
				})
				if err != nil {
					errs <- err
					return
				}
				if got := res.Values[0].Str; got != "k"+string(rune('a'+w))+":"+string(rune('A'+w))+":1" {
					errs <- errFake("bad result " + got)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSharedVMSerialization(t *testing.T) {
	e := mustEngine(t)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	s := mustCompile(t, e, `x = (x or 0) + 1 return x`)
	const N = 16
	var wg sync.WaitGroup
	results := make([]int64, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := vm.Run(context.Background(), s, RunOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			// the counter is integer arithmetic end-to-end: KindInteger
			results[i] = res.Values[0].Int
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, r := range results {
		if seen[r] {
			t.Fatalf("duplicate counter value %d — runs interleaved", r)
		}
		seen[r] = true
	}
	for i := int64(1); i <= N; i++ {
		if !seen[i] {
			t.Fatalf("missing counter value %d (got %v)", i, results)
		}
	}
}

// The glasm one-script-per-image law is deleted (no per-image proto
// registry exists): one VM runs any scripts, serially.
func TestNoOneScriptLaw(t *testing.T) {
	e := mustEngine(t)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	a := mustCompile(t, e, `return 1`)
	b := mustCompile(t, e, `return 2`)
	if _, err := vm.Run(context.Background(), a, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := vm.Run(context.Background(), b, RunOptions{})
	if err != nil || !res.Values[0].Equal(Int(2)) {
		t.Fatalf("second script on one VM: %v %#v", err, res.Values)
	}
	// and the first still runs (serially, shared state)
	res, err = vm.Run(context.Background(), a, RunOptions{})
	if err != nil || !res.Values[0].Equal(Int(1)) {
		t.Fatalf("re-run: %v %#v", err, res.Values)
	}
}

// --- registry freeze + RegisterValue ------------------------------------------

func TestRegisterValueAndFreeze(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterValue("redis", "LOG_WARNING", Number(4)); err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterValue("redis", "N", Int(7)); err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterValue("redis", "S", String("v")); err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterValue("redis", "BAD", Table()); err == nil {
		t.Fatal("RegisterValue accepted a table")
	}
	if err := e.RegisterValue("redis", "NAN", Number(math.NaN())); err == nil {
		t.Fatal("RegisterValue accepted a non-finite number")
	}
	if err := e.RegisterValue("redis table", "X", Number(1)); err == nil {
		t.Fatal("RegisterValue accepted a non-identifier table name")
	}
	res, err := runScript(t, e, `return redis.LOG_WARNING, redis.N, redis.S`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Values[0].Equal(Number(4)) || !res.Values[1].Equal(Int(7)) || !res.Values[2].Equal(String("v")) {
		t.Fatalf("registered values: %#v", res.Values)
	}
	// freeze: once a VM exists, the registry is immutable
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	if err := e.RegisterGlobal("redis", "late", func(vm *VM, a []Value) ([]Value, error) { return nil, nil }); err == nil {
		t.Fatal("RegisterGlobal after NewVM accepted")
	}
	if err := e.RegisterValue("redis", "late", Number(1)); err == nil {
		t.Fatal("RegisterValue after NewVM accepted")
	}
	// duplicates refused
	if err := e.RegisterValue("redis", "N", Int(8)); err == nil {
		t.Fatal("duplicate RegisterValue accepted")
	}
	if err := mustEngine(t).RegisterGlobal("a", "", func(vm *VM, a []Value) ([]Value, error) { return nil, nil }); err == nil {
		t.Fatal("empty name accepted")
	}
}

// --- print event sink ------------------------------------------------------------

func TestEventSink(t *testing.T) {
	var mu sync.Mutex
	var got [][]Value
	e := mustEngine(t, WithEventSink(func(vm *VM, args []Value) {
		mu.Lock()
		got = append(got, args)
		mu.Unlock()
	}))
	if _, err := runScript(t, e, `print('hello', 42, true, nil)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || len(got[0]) != 4 {
		t.Fatalf("print events: %#v", got)
	}
	if got[0][0].Str != "hello" || !got[0][1].Equal(Int(42)) || !got[0][2].BoolVal() ||
		got[0][3].Kind != KindNil {
		t.Fatalf("print args: %#v", got[0])
	}
}

// Tables cross the print sink as bare markers — print never expands
// tables (glasm's g_print posture; natively the marker decodes instead
// of the whole frame dropping).
func TestEventSinkTableMarker(t *testing.T) {
	var mu sync.Mutex
	var got [][]Value
	e := mustEngine(t, WithEventSink(func(vm *VM, args []Value) {
		mu.Lock()
		got = append(got, args)
		mu.Unlock()
	}))
	if _, err := runScript(t, e, `local t = {1,2} print('a', t, print)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || len(got[0]) != 3 {
		t.Fatalf("print events: %#v", got)
	}
	if got[0][0].Str != "a" || got[0][1].Kind != KindTable || len(got[0][1].Pairs) != 0 {
		t.Fatalf("table marker: %#v", got[0])
	}
	if got[0][2].Kind != KindFunction {
		t.Fatalf("function marker: %#v", got[0])
	}
}

// --- close semantics ------------------------------------------------------------

func TestCloseSemantics(t *testing.T) {
	e := mustEngine(t)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Close(); err != nil {
		t.Fatal(err)
	}
	s := mustCompile(t, e, `return 1`)
	if _, err := vm.Run(context.Background(), s, RunOptions{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Run after VM close: %v", err)
	}
	// live VMs keep working after engine close (they own their states)
	vm2, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm2.Close()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := vm2.Run(context.Background(), s, RunOptions{})
	if err != nil || !res.Values[0].Equal(Int(1)) {
		t.Fatalf("live VM after engine close: %v %#v", err, res.Values)
	}
	if _, err := e.NewVM(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("NewVM after engine close: %v", err)
	}
}

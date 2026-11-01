package host

// N2 sandbox tests: globals protection (the glasm-blob byte-text
// contract, probed against Redis 7.2.7), stdlib selection, and the
// coroutine spawn cap.

import (
	"strings"
	"testing"
	"time"

	lua "github.com/pschlump/gopher-lua"
)

func runProtected(t *testing.T, e *Engine, src string) (Result, error) {
	t.Helper()
	s := mustCompile(t, e, src)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatalf("NewVM: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	return vm.Run(t.Context(), s, RunOptions{Keys: []string{"k1"}, Argv: []string{"a1"}})
}

func wantScriptError(t *testing.T, src string, res Result, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Errorf("%q: got values %v, want error %q", src, res.Values, wantMsg)
		return
	}
	se, ok := err.(*ScriptError)
	if !ok {
		t.Errorf("%q: got %T %v, want *ScriptError", src, err, err)
		return
	}
	if got := se.ErrValue.String(); got != wantMsg {
		t.Errorf("%q:\n got %q\nwant %q", src, got, wantMsg)
	}
	if se.Line != 1 {
		t.Errorf("%q: line = %d, want 1", src, se.Line)
	}
}

func TestGlobalsProtectionErrors(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	cases := []struct{ src, want string }{
		{"return undefined_global",
			"test:1: Script attempted to access nonexistent global variable 'undefined_global'"},
		{"g = 5 return g", "test:1: Attempt to modify a readonly table"},
		{"redis = nil return 1", "test:1: Attempt to modify a readonly table"},
		{"string.foo = 1 return 1", "test:1: Attempt to modify a readonly table"},
		{"_G.pairs = nil return 1", "test:1: Attempt to modify a readonly table"},
		{"return _G[nil]",
			"test:1: Second argument to luaProtectedTableError must be a string or number"},
		// rawset goes through baseRawSet, whose guard raises BARE (level
		// 0) — N5/Redis parity: the bytecode set paths carry "chunk:N:"
		// but Redis's rawset builtin adds no position; the raise line
		// still resolves (raise-time traceback) for the on-@ suffix.
		{"rawset(_G, 'g3', 1) return 1", "Attempt to modify a readonly table"},
	}
	for _, c := range cases {
		res, err := runProtected(t, e, c.src)
		wantScriptError(t, c.src, res, err, c.want)
	}
}

func TestGlobalsProtectionAllows(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	cases := []struct {
		src  string
		want Value
	}{
		{"return rawget(_G, 'undefined_global') == nil", Bool(true)},
		{"local t = {} rawset(t, 'k', 1) return t.k", Int(1)},
		{"KEYS[1] = 'x' return KEYS[1]", String("x")},
		{"ARGV[1] = 'y' return ARGV[1]", String("y")},
		{"local t = setmetatable({}, {__index=function() return 7 end}) return t.x", Int(7)},
		{"for k in pairs(_G) do if k=='nosuch' then return 1 end end return 0", Int(0)},
		{"return getmetatable(_G) ~= nil", Bool(true)},
	}
	for _, c := range cases {
		res, err := runProtected(t, e, c.src)
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if len(res.Values) != 1 || !res.Values[0].Equal(c.want) {
			t.Errorf("%q: got %v, want %v", c.src, res.Values, c.want)
		}
	}
}

// the caught message carries the position prefix (Redis parity), and
// the run that follows an errored script is unaffected
func TestGlobalsProtectionPcall(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	res, err := runProtected(t, e,
		"local ok,e = pcall(function() return nosuchglobal end) return {ok, e}")
	if err != nil {
		t.Fatalf("pcall of guard raise must not escape: %v", err)
	}
	msg := res.Values[0].Pairs[1].Val.Str
	want := "test:1: Script attempted to access nonexistent global variable 'nosuchglobal'"
	if msg != want {
		t.Errorf("caught message:\n got %q\nwant %q", msg, want)
	}
	res, err = runProtected(t, e,
		"local ok,e = pcall(function() g = 5 end) return {ok, e}")
	if err != nil {
		t.Fatalf("pcall of readonly raise must not escape: %v", err)
	}
	if msg = res.Values[0].Pairs[1].Val.Str; msg != "test:1: Attempt to modify a readonly table" {
		t.Errorf("caught message: %q", msg)
	}
}

// default unchanged: without the option, scripts create/read globals freely
func TestGlobalsProtectionOffByDefault(t *testing.T) {
	e := mustEngine(t)
	res, err := runProtected(t, e, "g = 5 return g + #KEYS")
	if err != nil {
		t.Fatalf("protection off: %v", err)
	}
	if !res.Values[0].Equal(Int(6)) {
		t.Errorf("got %v, want 6", res.Values)
	}
	res, err = runProtected(t, e, "return undefined_global == nil")
	if err != nil {
		t.Fatalf("protection off: %v", err)
	}
	if !res.Values[0].Equal(Bool(true)) {
		t.Errorf("got %v, want true", res.Values)
	}
}

// staging still works through the readonly window: constants registered
// before VM creation are staged before protection, KEYS/ARGV inside it
func TestGlobalsProtectionStaging(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	if err := e.RegisterValue("redis", "ANSWER", Int(42)); err != nil {
		t.Fatalf("RegisterValue: %v", err)
	}
	res, err := runProtected(t, e, "return redis.ANSWER + #KEYS")
	if err != nil {
		t.Fatalf("staging under protection: %v", err)
	}
	if !res.Values[0].Equal(Int(43)) {
		t.Errorf("got %v, want 43", res.Values)
	}
	res, err = runProtected(t, e, "return #KEYS..'/'..#ARGV")
	if err != nil {
		t.Fatalf("KEYS/ARGV staging under protection: %v", err)
	}
	if !res.Values[0].Equal(String("1/1")) {
		t.Errorf("got %v, want 1/1", res.Values)
	}
}

// a pooled protected VM re-stages KEYS/ARGV across runs through the raw
// seam — the second run sees fresh tables, still guest-writable
func TestGlobalsProtectionPooledRerun(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true), WithVMCacheSize(1))
	s := mustCompile(t, e, "KEYS[1] = 'z' return KEYS[1]")
	res, err := e.Run(t.Context(), s, RunOptions{Keys: []string{"k"}})
	if err != nil {
		t.Fatalf("pooled run 1: %v", err)
	}
	if !res.Values[0].Equal(String("z")) {
		t.Fatalf("run 1: %v", res.Values)
	}
	res, err = e.Run(t.Context(), s, RunOptions{Keys: []string{"k"}})
	if err != nil {
		t.Fatalf("pooled run 2 (raw-seam restage): %v", err)
	}
	if !res.Values[0].Equal(String("z")) {
		t.Fatalf("run 2: %v", res.Values)
	}
}

// --- stdlib selection ---------------------------------------------------------

func TestStdLibsDefaultSafe(t *testing.T) {
	e := mustEngine(t)
	res, err := runProtected(t, e, `return io == nil and os == nil and debug == nil and package == nil and channel == nil`)
	if err != nil {
		t.Fatalf("safe default: %v", err)
	}
	if !res.Values[0].Equal(Bool(true)) {
		t.Errorf("ambient libraries present under SafeStdLibs: %v", res.Values)
	}
}

func TestStdLibsBaseLoadersNiled(t *testing.T) {
	e := mustEngine(t)
	res, err := runProtected(t, e, `return require == nil and loadfile == nil and dofile == nil and load == nil and warn == nil`)
	if err != nil {
		t.Fatalf("base loaders: %v", err)
	}
	if !res.Values[0].Equal(Bool(true)) {
		t.Errorf("base loaders not nil'd: %v", res.Values)
	}
}

func TestStdLibsAll(t *testing.T) {
	e := mustEngine(t, WithStdLibs(AllStdLibs...))
	res, err := runProtected(t, e, `return type(io.read) == 'function' and type(os.time) == 'function' and type(debug.traceback) == 'function'`)
	if err != nil {
		t.Fatalf("all libs: %v", err)
	}
	if !res.Values[0].Equal(Bool(true)) {
		t.Errorf("full surface missing: %v", res.Values)
	}
}

func TestStdLibsExplicit(t *testing.T) {
	e := mustEngine(t, WithStdLibs(StdLib{"", lua.OpenBase}))
	res, err := runProtected(t, e, `return string == nil and select('#', 1, 2) == 2`)
	if err != nil {
		t.Fatalf("explicit minimal: %v", err)
	}
	if !res.Values[0].Equal(Bool(true)) {
		t.Errorf("minimal surface wrong: %v", res.Values)
	}
}

// --- coroutine cap -----------------------------------------------------------

func TestMaxCoroutines(t *testing.T) {
	e := mustEngine(t, WithMaxCoroutines(2))
	res, err := runProtected(t, e, `
local ok1, co1 = pcall(coroutine.create, function() coroutine.yield() end)
local ok2, co2 = pcall(coroutine.create, function() coroutine.yield() end)
local ok3, err = pcall(coroutine.create, function() end)
if not (ok1 and ok2) then return "create under cap failed" end
if not ok3 then return err end
return "no cap"`)
	if err != nil {
		t.Fatalf("coroutine cap run: %v", err)
	}
	got := res.Values[0].String()
	if !strings.Contains(got, "too many coroutines") {
		t.Fatalf("third create: got %q, want 'too many coroutines'", got)
	}
}

// closing a coroutine releases its slot against the cap
func TestMaxCoroutinesCloseReleases(t *testing.T) {
	e := mustEngine(t, WithMaxCoroutines(1))
	res, err := runProtected(t, e, `
local co = coroutine.create(function() coroutine.yield() end)
coroutine.resume(co)
coroutine.close(co)
local ok, co2 = pcall(coroutine.create, function() end)
if not ok then return "still capped: " .. tostring(co2) end
return "released"`)
	if err != nil {
		t.Fatalf("release run: %v", err)
	}
	if got := res.Values[0].String(); got != "released" {
		t.Fatalf("slot not released: %q", got)
	}
}

// unbudgeted/uncapped default: stock behavior (guarded by the golden
// corpus; here just that many coroutines still work)
func TestCoroutinesUncapped(t *testing.T) {
	e := mustEngine(t)
	_, err := runProtected(t, e, `
local t = {}
for i = 1, 8 do
  t[i] = coroutine.create(function() coroutine.yield(i) end)
end
for i = 1, 8 do assert(select(2, coroutine.resume(t[i])) == i) end`)
	if err != nil {
		t.Fatalf("uncapped coroutines: %v", err)
	}
}

// the budget spins (glasm host's three probes): each must surface a
// pcall-catchable "not enough memory", never a wedge — the Deadline is
// the backstop, the seams are the mechanism.
func TestMemoryBudgetConcat(t *testing.T) {
	e := mustEngine(t, WithMemoryBudgetBytes(4<<20))
	_, err := runScript(t, e, `local s = 'x' while true do s = s .. s end`,
		RunOptions{Deadline: 60 * time.Second})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "not enough memory") {
		t.Fatalf("oom text: %q", se.Error())
	}
}

func TestMemoryBudgetGmatch(t *testing.T) {
	e := mustEngine(t, WithMemoryBudgetBytes(8<<20))
	_, err := runScript(t, e, `
local s = string.rep("alpha ", 1000)
while true do
  for w in string.gmatch(s, "(%a+)") do end
  s = s .. s
end`, RunOptions{Deadline: 60 * time.Second})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "not enough memory") {
		t.Fatalf("oom text: %q", se.Error())
	}
}

func TestMemoryBudgetTconcat(t *testing.T) {
	e := mustEngine(t, WithMemoryBudgetBytes(4<<20))
	_, err := runScript(t, e, `
local t = {}
for i = 1, 200 do t[i] = string.rep("x", 100) end
while true do t[#t + 1] = table.concat(t) end`, RunOptions{Deadline: 60 * time.Second})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "not enough memory") {
		t.Fatalf("oom text: %q", se.Error())
	}
}

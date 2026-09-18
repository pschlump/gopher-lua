package testdiff

// The M5d error-suite generator: ~100 one-error scripts, one divergence
// family each (note/m5-to-m6 §6 M5d). Run once via
// `go test ./testdiff -run TestGenM5ErrCorpus`; the files land in
// _wasm-err-tests/ and are diffed byte-exact (message heads) by
// TestWasmErrorSuite (interp vs wasm) — the suite IS the M5d spike:
// run it before patching, patch per DIVERGE, ledger the rest.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestGenM5ErrCorpus(t *testing.T) {
	dir := "../_wasm-err-tests"
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var cases []string
	add := func(name, src string) {
		cases = append(cases, src)
		if err := os.WriteFile(filepath.Join(dir, name+".lua"), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// ---- arithmetic on non-numbers (no metamethods) ----
	// gopher: "cannot perform <op> operation between <t1> and <t2>"
	n := 0
	pre := map[string]string{
		"nil":   "local x\n",
		"bool":  "local b = true\n",
		"table": "local t = {}\n",
		"str":   "local s = 'nope'\n",
	}
	for _, v := range []string{"nil", "bool", "table"} {
		for _, op := range []string{"+", "-", "*", "/", "%", "^"} {
			add(fmt.Sprintf("ar%02d", n), fmt.Sprintf("%sprint(%s %s 1)\n", pre[v], v, op))
			n++
		}
	}
	add("ar10", pre["str"]+"print(s + 1)\n")
	add("ar11", "local t = {}\nprint(1 + t)\n")
	add("ar12", "local b = true\nprint(-b)\n") // UNM on boolean
	add("ar13", "local t = {}\nprint(-t)\n")
	add("ar14", "local s = 'q'\nprint(#s == 0 and -s or -s)\n") // unm string coercion fails
	add("ar15", "print('x' * 'y')\n")                           // string-string arith

	// ---- concat (gopher: "cannot perform concat operation between <t1> and <t2>") ----
	add("cc00", "local x\nprint(x .. 'a')\n")
	add("cc01", "local x\nprint('a' .. x)\n")
	add("cc02", "local t = {}\nprint(t .. 'a')\n")
	add("cc03", "local b = true\nprint('a' .. b)\n")
	add("cc04", "local x\nprint('a' .. 1 .. x .. 2)\n")
	add("cc05", "local t1, t2 = {}, {}\nprint(t1 .. t2)\n")
	add("cc06", "local x\nprint(1 .. x)\n")
	add("cc07", "local b = false\nprint(b .. b)\n")

	// ---- index (gopher: "attempt to index a non-table object(<t>) with key '<k>'") ----
	add("ix00", "local x\nprint(x.foo)\n")
	add("ix01", "local x\nlocal k = 'foo'\nprint(x[k])\n")
	add("ix02", "local x\nprint(x[1])\n")
	add("ix03", "local n = 5\nprint(n.foo)\n")
	add("ix04", "local n = 5\nprint(n[true])\n")
	add("ix05", "local b = true\nprint(b.foo)\n")
	add("ix06", "local f = print\nprint(f.field)\n") // function index is legal (nil) — kept as a control
	add("ix07", "local x\nx.foo = 1\n")              // set
	add("ix08", "local x\nx[3] = 1\n")
	add("ix09", "local n = 2.5\nn.foo = 1\n")
	add("ix10", "local s = 'str'\ns.foo = 1\n") // string set errors
	add("ix11", "local b = nil\nprint(b.a.b)\n")

	// ---- comparison (gopher: "attempt to compare <t1> with <t2>") ----
	add("cp00", "local x\nprint(x < 1)\n")
	add("cp01", "print(1 < nil)\n")
	add("cp02", "print('a' < 1)\n")
	add("cp03", "print(1 < 'a')\n")
	add("cp04", "print(true < false)\n")
	add("cp05", "local t = {}\nprint(t < t)\n")
	add("cp06", "print(nil <= nil)\n")
	add("cp07", "print({} < {})\n")
	add("cp08", "local x\nprint(1 <= x)\n")
	add("cp09", "print(false < 1)\n")

	// ---- numeric-for limits (gopher: "for statement init/limit/step must be a number") ----
	add("fo00", "for i = 'x', 3 do end\n")
	add("fo01", "for i = 1, 'x' do end\n")
	add("fo02", "for i = 1, 3, 'x' do end\n")
	add("fo03", "for i = {}, 3 do end\n")
	add("fo04", "for i = 1, true do end\n")
	add("fo05", "for i = nil, 3, 2 do end\n")

	// ---- call non-function (gopher: "attempt to call a non-function object") ----
	add("cl00", "local x\nx()\n")
	add("cl01", "local n = 5\nn()\n")
	add("cl02", "local s = 'f'\ns()\n")
	add("cl03", "local b = true\nb()\n")
	add("cl04", "local t = {}\nt()\n")
	add("cl05", "local t = {}\nt.f()\n")

	// ---- error() levels × value kinds ----
	add("e000", "error('m')\n")
	add("e001", "error('m', 0)\n")
	add("e002", "error('m', 1)\n")
	add("e003", "local function g() error('m', 2) end\nlocal function f() g() end\nf()\n")
	add("e004", "local function g() error('m', 1) end\nlocal function f() g() end\nf()\n")
	add("e005", "local function g() error('m', 3) end\nlocal function f() g() end\nlocal function h() f() end\nh()\n")
	add("e006", "error(nil)\n")
	add("e007", "error()\n")
	add("e008", "local ok, e = pcall(error, {})\nprint(ok, type(e))\n") // table values carry addresses — in-script pcall + type
	add("e009", "error(42)\n")
	add("e010", "print(pcall(error, 'wrapped'))\n")
	add("e011", "local function g() error('lvl2', 2) end\nlocal function f() g() end\nprint(pcall(f))\n")
	add("e012", "error('a\\nb')\n") // embedded newline stays in the payload

	// ---- assert ----
	add("as00", "assert(false)\n")
	add("as01", "assert(nil)\n")
	add("as02", "assert(false, 'custom')\n")
	add("as03", "print(pcall(assert, false))\n")
	add("as04", "assert(nil, 42)\n") // non-string message value

	// ---- pcall of pcall, nested catches ----
	add("pc00", "print(pcall(function() print(pcall(function() error('inner') end)) error('outer') end))\n")
	add("pc01", "local ok, e = pcall(function() error('x') end)\nprint(ok, e)\n")
	add("pc02", "print(pcall(pcall, error, 'py'))\n")

	// ---- error inside metamethod ----
	add("mm00", "local mt = {__index = function(t, k) error('index boom') end}\nlocal t = setmetatable({}, mt)\nprint(t.foo)\n")
	add("mm01", "local mt = {__add = function(a, b) error('add boom') end}\nlocal t = setmetatable({}, mt)\nprint(t + 1)\n")
	add("mm02", "local mt = {__index = function(t, k) error('idx') end}\nlocal t = setmetatable({}, mt)\nprint(pcall(function() return t.foo end))\n")
	add("mm03", "local mt = {__lt = function(a, b) error('lt boom') end}\nlocal t = setmetatable({}, mt)\nprint(t < t)\n")
	add("mm04", "local mt = {__concat = function(a, b) error('cc boom') end}\nlocal t = setmetatable({}, mt)\nprint(t .. 'x')\n")

	// ---- metamethod exactly once (counters) ----
	add("mo00", "local n = 0\nlocal mt = {__index = function(t, k) n = n + 1 return n end}\nlocal t = setmetatable({}, mt)\nt.a t.a t.a\nprint(n)\n")
	add("mo01", "local n = 0\nlocal mt = {__add = function(a, b) n = n + 1 return n end}\nlocal t = setmetatable({}, mt)\nlocal _ = t + 1\nlocal _ = t + 1\nprint(n)\n")

	// ---- length / misc type errors ----
	add("mi00", "local x\nprint(#x)\n")
	add("mi01", "local b = true\nprint(#b)\n")

	// ---- stack overflow (depth is ledgered; assert the catch shape) ----
	add("so00", "local function f() return 1 + f() end\nprint(pcall(f))\n")
	add("so01", "local function f() local t = {} t[1] = f() return t end\nprint(pcall(f))\n")

	// ---- error through gsub callback (sort stays row 10) ----
	add("gb00", "print(pcall(function() return ('x'):gsub('x', function() error('gsub boom') end) end))\n")

	t.Logf("wrote %d cases to %s", len(cases), dir)
}

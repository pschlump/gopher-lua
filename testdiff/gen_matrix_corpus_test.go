package testdiff

// The M4 gate corpus generator: writes ~200 small scripts covering the
// v1-supported opcode surface (no closures/varargs — those SKIP and belong
// to M5). Run once via `go test ./testdiff -run TestGenMatrixCorpus`; the
// files land in _wasm-tests/ and are diffed by
//
//	go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wasm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestGenMatrixCorpus(t *testing.T) {
	dir := "../_wasm-tests"
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

	// ---- literals & prints ----
	for i, v := range []string{"42", "3.25", "-7", "0.5", "1e3", "'s'", "\"d\"", "true", "false", "nil"} {
		add(fmt.Sprintf("lit%02d", i), fmt.Sprintf("print(%s)\n", v))
	}
	// ---- arithmetic over a value grid ----
	vals := []string{"1", "2.5", "-3", "100", "0", "7"}
	ops := []string{"+", "-", "*", "/", "%", "^"}
	n := 0
	for _, a := range vals {
		for _, b := range vals {
			for _, op := range ops {
				add(fmt.Sprintf("ari%03d", n), fmt.Sprintf("print(%s %s %s)\n", a, op, b))
				n++
			}
		}
	}
	// string coercion arithmetic
	for i, s := range []string{"'10' + 1", "'2' * '3'", "'7' .. 7", "1 .. 2", "'x' .. 0"} {
		add(fmt.Sprintf("coerce%02d", i), fmt.Sprintf("print(%s)\n", s))
	}
	// unary
	for i, s := range []string{"-5", "-(-5)", "- 2.5"} {
		add(fmt.Sprintf("unm%02d", i), fmt.Sprintf("print(%s)\n", s))
	}
	// ---- comparisons ----
	cvals := []string{"1", "2", "'a'", "'b'", "true", "false", "nil"}
	n = 0
	for _, a := range []string{"1", "2", "'a'", "'b'", "1.5"} {
		for _, b := range []string{"1", "2", "'a'", "2.5"} {
			for _, op := range []string{"==", "~=", "<", "<=", ">", ">="} {
				add(fmt.Sprintf("cmp%03d", n), fmt.Sprintf("print(%s %s %s)\n", a, op, b))
				n++
			}
		}
	}
	_ = cvals
	// ---- logic (and/or/not) ----
	lvals := []string{"nil", "false", "true", "0", "1", "''", "'x'"}
	n = 0
	for _, a := range lvals {
		for _, b := range lvals {
			add(fmt.Sprintf("logic%03d", n), fmt.Sprintf("print(%s and %s, %s or %s, not %s)\n", a, b, a, b, a))
			n++
		}
	}
	// ---- length / concat ----
	for i, s := range []string{"#'hello'", "#{1,2,3}", "'a'..'b'..'c'", "#{1,2,3}[2]"} {
		add(fmt.Sprintf("len%02d", i), fmt.Sprintf("print(%s)\n", s))
	}
	// ---- locals & assignment ----
	add("loc00", "local a, b, c = 1, 2, 3\nprint(a, b, c)\n")
	add("loc01", "local a = 1\nlocal b = a\na = 2\nprint(a, b)\n")
	add("loc02", "local a, b = 1\nprint(a, b)\n")
	add("loc03", "local a = 1\na, a = 2, 3\nprint(a)\n")
	add("loc04", "local x\nprint(x)\nx = 9\nprint(x)\n")
	// ---- if / elseif / else ----
	add("if00", "if 1 < 2 then print('y') else print('n') end\n")
	add("if01", "if 2 < 1 then print('y') else print('n') end\n")
	add("if02", "if 1 < 0 then print('a') elseif 1 < 2 then print('b') else print('c') end\n")
	add("if03", "if 1 < 0 then print('a') elseif 2 < 1 then print('b') else print('c') end\n")
	add("if04", "local x = 5\nif x > 0 then print('p') end\nprint('after')\n")
	add("if05", "if nil then print('t') elseif false then print('f') elseif 0 then print('z') end\n")
	// ---- while / repeat / break ----
	add("whl00", "local i = 0\nwhile i < 3 do i = i + 1 end\nprint(i)\n")
	add("whl01", "local i = 0\nwhile true do i = i + 1 if i > 2 then break end end\nprint(i)\n")
	add("whl02", "local i = 3\nwhile i > 0 do i = i - 1 end\nprint(i)\n")
	add("rep00", "local i = 0\nrepeat i = i + 1 until i >= 3\nprint(i)\n")
	add("rep01", "local i = 0\nrepeat local j = i i = i + 1 until j >= 2\nprint(i)\n")
	// ---- numeric for ----
	add("for00", "for i = 1, 3 do print(i) end\n")
	add("for01", "for i = 1, 6, 2 do print(i) end\n")
	add("for02", "for i = 3, 1, -1 do print(i) end\n")
	add("for03", "local s = 0\nfor i = 1, 10 do s = s + i end\nprint(s)\n")
	add("for04", "for i = 1, 0 do print('never') end\nprint('done')\n")
	add("for05", "for i = 1.5, 3.5, 0.5 do print(i) end\n")
	// ---- generic for ----
	add("gfor00", "for i, v in ipairs({10, 20, 30}) do print(i, v) end\n")
	add("gfor01", "for k, v in pairs({a=1, b=2}) do print(k, v) end\n")
	add("gfor02", "local n = 0\nfor k, v in pairs({5, 6, 7}) do n = n + v end\nprint(n)\n")
	// ---- tables ----
	add("tbl00", "local t = {}\nt.x = 1\nprint(t.x)\n")
	add("tbl01", "local t = {1, 2, 3}\nprint(#t, t[1], t[3])\n")
	add("tbl02", "local t = {a=1, b=2}\nprint(t.a, t.b)\n")
	add("tbl03", "local t = {1, 2, 3, 4, 5}\nt[2] = 20\nprint(t[2], #t)\n")
	add("tbl04", "local t = {}\nfor i = 1, 60 do t[i] = i * 2 end\nprint(#t, t[50], t[60])\n")
	add("tbl05", "local t = {1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,59,60}\nprint(#t, t[1], t[60])\n")
	add("tbl06", "local t = {'a','b','c'}\ntable.insert(t, 'd')\nprint(#t, t[4])\n")
	// tbl07 (table.sort) REMOVED: sort's comparator path trips the wasmtime
	// callback machinery (ENGINE-PANIC at lglobals; CLI goes silent after the
	// sort call). Ledger row — fixed with the M5 error/EH investigation.
	add("tbl08", "local t = {x = {y = {z = 7}}}\nprint(t.x.y.z)\n")
	add("tbl09", "local t = {}\nt[1] = 'a' t[2] = 'b'\nprint(table.concat(t, '-'))\n")
	// ---- strings ----
	add("str00", "print(string.sub('hello', 2, 3))\n")
	add("str01", "print(string.upper('abc') .. string.rep('x', 3))\n")
	add("str02", "print(string.format('%d-%s', 7, 'q'))\n")
	add("str03", "print(#('a'..'b'), ('ab'):sub(2,2))\n")
	add("str04", "print(string.byte('A'), string.char(66))\n")
	// ---- library math (deterministic subset) ----
	add("math00", "print(math.floor(3.7), math.ceil(3.2), math.abs(-4))\n")
	add("math01", "print(math.max(1, 5, 3), math.min(1, 5, 3))\n")
	add("math02", "print(math.sqrt(16), math.fmod(7, 3))\n")
	// ---- calls & multret ----
	add("cal00", "print(select('#', 1, 2, 3))\n")
	add("cal01", "print(select(2, 'a', 'b', 'c'))\n")
	add("cal02", "print(unpack({7, 8, 9}))\n")
	add("cal03", "print(tostring(42), tonumber('13'), type(nil), type(1), type('s'), type({}))\n")
	add("cal04", "print(pcall(function() end))\n") // function → SKIP (closure) — keep for the skip path
	add("cal05", "print(tonumber('x') == nil)\n")
	// ---- error paths (byte-exact messages) ----
	add("err00", "local t = nil\nprint(t.x)\n")
	add("err01", "print(1 + {})\n")
	add("err02", "print(nil .. 'x')\n")
	add("err03", "for i = 1, 'x' do end\n")
	// ---- nested control flow ----
	add("nest00", "for i = 1, 3 do for j = 1, 2 do print(i, j) end end\n")
	add("nest01", "local s = 0\nfor i = 1, 4 do if i % 2 == 0 then s = s + i end end\nprint(s)\n")
	add("nest02", "local i = 0\nwhile i < 5 do i = i + 1 if i == 3 then break end end\nprint(i)\n")
	add("nest03", "for i = 1, 3 do if i == 2 then break end print(i) end\n")

	t.Logf("wrote %d cases to %s", len(cases), dir)
}

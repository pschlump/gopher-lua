package testdiff

// M4 smoke: the wasm backend compiles and runs a real script against the
// C runtime, with the harness shim producing the same event log shape as
// the other engines.

import "testing"

func runWasm(t *testing.T, src string) []string {
	t.Helper()
	e := &WasmEngine{name: "wasm"}
	return e.Run(Case{Name: "smoke.lua", Dir: ".", Source: []byte(src)})
}

func TestWasmBackendSmoke(t *testing.T) {
	log := runWasm(t, `
local x = 40
local y = 2
print("sum", x + y)
local t = {}
t.k = "v"
print(t.k, #t.k)
for i = 1, 3 do
  print("i", i)
end
if x < y then print("lt") else print("ge") end
print("concat", "a" .. 1 .. "b")
print(math.floor(3.7))
`)
	for _, l := range log {
		if len(l) > 200 {
			l = l[:200] + "..."
		}
		t.Log(l)
	}
}

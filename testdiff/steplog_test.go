package testdiff

import "testing"

func TestStepLog(t *testing.T) {
	e := &WasmEngine{name: "s"}
	log := e.Run(Case{Name: "b.lua", Dir: ".", Source: []byte("local y = x\n")})
	for _, l := range log {
		if len(l) > 100 {
			l = l[:100]
		}
		t.Log(l)
	}
}

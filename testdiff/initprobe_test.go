package testdiff

import "testing"

func TestInitProbe(t *testing.T) {
	e := &WasmEngine{name: "p"}
	log := e.Run(Case{Name: "p.lua", Dir: ".", Source: []byte("print('hi')")})
	for _, l := range log {
		if len(l) > 700 {
			l = l[:700]
		}
		t.Log(l)
	}
}

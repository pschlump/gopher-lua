package testdiff

import "testing"

func TestMini3(t *testing.T) {
	e := &WasmEngine{name: "m3"}
	log := e.Run(Case{Name: "m3.lua", Dir: ".", Source: []byte("print('a' .. 'b')\n")})
	for _, l := range log {
		if len(l) > 90 {
			l = l[:90]
		}
		t.Log(l)
	}
}

package testdiff

import "testing"

func TestMini2(t *testing.T) {
	for _, src := range []string{
		"local t = {}\nt.k = 'v'\nprint(t.k)\n",
		"print('a' .. 'b')\n",
	} {
		e := &WasmEngine{name: "m2"}
		log := e.Run(Case{Name: "m2.lua", Dir: ".", Source: []byte(src)})
		for _, l := range log {
			if len(l) > 80 {
				l = l[:80]
			}
			if l[:5] == "PRINT" || l[:5] == "ERROR" || l[:6] == "ENGINE" {
				t.Logf("%-30q %s", src, l)
			}
		}
	}
}

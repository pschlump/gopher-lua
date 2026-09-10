package testdiff

import (
	"strings"
	"testing"
)

func TestZZ(t *testing.T) {
	e := &WasmEngine{name: "z"}
	log := e.Run(Case{Name: "z.lua", Dir: ".", Source: []byte("zz = 5\n")})
	for _, l := range log {
		if strings.HasPrefix(l, "GLOBALS\t") {
			if i := strings.Index(l, `"zz"=`); i >= 0 {
				end := i + 20
				if end > len(l) {
					end = len(l)
				}
				t.Log("zz =", l[i:end])
			} else {
				t.Log("zz NOT in globals")
			}
		}
		if strings.HasPrefix(l, "ERROR\t") {
			t.Log(l)
		}
	}
}

package testdiff

import (
	"strings"
	"testing"
)

func TestMiniScripts(t *testing.T) {
	for _, src := range []string{
		"zz = print\n", "print(42)\n",
		"local x = 40\nprint(x)\n",
		"print(40 + 2)\n",
		"local x = 40\nlocal y = 2\nprint(x + y)\n",
		"local t = {}\nt.k = 'v'\nprint(t.k)\n",
		"for i = 1, 3 do print('i', i) end\n",
		"print('a' .. 'b')\n",
	} {
		e := &WasmEngine{name: "m"}
		log := e.Run(Case{Name: "m.lua", Dir: ".", Source: []byte(src)})
		out := ""
		for _, l := range log {
			if i := strings.Index(l, "zz="); i >= 0 {
				l = "GLOBALS zz=" + l[i+3:i+16]
			}
			if len(l) > 60 {
				l = l[:60]
			}
			if len(out) > 0 {
				out += " | "
			}
			out += l
		}
		t.Logf("%-40q => %s", src, out)
	}
}

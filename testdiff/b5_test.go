package testdiff

import (
	"os"
	"testing"
)

func TestBisect5(t *testing.T) {
	for _, src := range []string{
		"",               // empty chunk
		"local x = 1\n",  // LOADK only
		"local y = x\n",  // GETGLOBAL (nil global)
		"zz = 5\n",       // SETGLOBAL
		"local t = {}\n", // NEWTABLE
		"print('hi')\n",  // GETGLOBAL + CALL (C func)
	} {
		dir := "."
		if os.Getenv("ABSDIR") != "" {
			dir = t.TempDir()
		}
		e := &WasmEngine{name: "b", NoopHosts: os.Getenv("NOOPHOST") != ""}
		log := e.Run(Case{Name: "b.lua", Dir: dir, Source: []byte(src)})
		status := "ok"
		for _, l := range log {
			if len(l) > 90 {
				l = l[:90]
			}
			if l[:7] == "ENGINE-" || l[:6] == "PRINT\t" || l[:6] == "ERROR\t" {
				status = l
			}
		}
		t.Logf("%-24q %s", src, status)
	}
}

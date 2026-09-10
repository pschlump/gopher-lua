package testdiff

import (
	"testing"
	"time"
)

// For-loop bisection: each script runs in its own engine with a watchdog so
// one hanging variant doesn't block the others.
func TestForMini(t *testing.T) {
	scripts := []string{
		"for i = 1, 3 do end\n",
		"for i = 1, 3 do local x = i end\n",
		"for i = 1, 3 do print(i) end\n",
		"for i = 1, 3 do print('i', i) end\n",
	}
	for _, src := range scripts {
		done := make(chan string, 1)
		go func(src string) {
			e := &WasmEngine{name: "m"}
			log := e.Run(Case{Name: "m.lua", Dir: ".", Source: []byte(src)})
			out := ""
			n := 0
			for _, l := range log {
				if len(l) > 50 {
					l = l[:50]
				}
				if n++; n > 14 {
					if n == 15 {
						out += " | ..."
					}
					continue
				}
				if len(out) > 0 {
					out += " | "
				}
				out += l
			}
			done <- out
		}(src)
		select {
		case out := <-done:
			t.Logf("%-38q => %s", src, out)
		case <-time.After(15 * time.Second):
			t.Logf("%-38q => HANG (15s)", src)
		}
	}
}

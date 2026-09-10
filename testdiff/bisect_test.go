package testdiff

import (
	"testing"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
)

func tryCompile(t *testing.T, src string) error {
	bin, err := CompileSource([]byte(src), "b.lua")
	if err != nil {
		return err
	}
	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true)
	eng := wt.NewEngineWithConfig(cfg)
	_, err2 := wt.NewModule(eng, bin)
	return err2
}

func TestBisectBackend(t *testing.T) {
	parts := []string{
		`local x = 40
print("v", x)`,
		`local t = {}
t.k = "v"
print(t.k)`,
		`for i = 1, 3 do print("i", i) end`,
		`local a = 1
if a < 2 then print("lt") else print("ge") end`,
		`print("c", "a" .. 1 .. "b")`,
		`print(math.floor(3.7))`,
		`local u = #{10, 20, 30}
print(u)`,
	}
	src := ""
	for i, p := range parts {
		src += p + "\n"
		err := tryCompile(t, src)
		t.Logf("after part %d (%q): %v", i, p[:14], err)
		if err != nil {
			return
		}
	}
}

package testdiff

import "testing"

func TestCLuaSmoke(t *testing.T) {
	e := NewCLua("clua-smoke")
	log := e.Run(Case{
		Name:   "smoke.lua",
		Dir:    ".",
		Source: []byte("local t = {x=1, y='two'}\nprint('hello', 1+2, true, t)\nzz = 42\n"),
	})
	for _, l := range log {
		if len(l) > 160 {
			l = l[:160] + "..."
		}
		t.Log(l)
	}
	if len(log) != 2 {
		t.Fatalf("expected PRINT + GLOBALS events, got %d", len(log))
	}
	if log[0] != "PRINT\thello\t3\ttrue\t<table>" {
		t.Errorf("PRINT mismatch: %q", log[0])
	}
}

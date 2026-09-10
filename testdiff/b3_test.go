package testdiff

import "testing"

func TestBisect3(t *testing.T) {
	err := tryCompile(t, "local t = {}\nt.k = \"v\"\n")
	t.Log(err)
}

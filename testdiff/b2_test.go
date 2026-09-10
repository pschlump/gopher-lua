package testdiff

import "testing"

func TestBisect2(t *testing.T) {
	for _, src := range []string{
		`local t = {}`,
		`local t = {}
t.k = "v"`,
		`local t = {10, 20}`,
	} {
		err := tryCompile(t, src)
		t.Logf("src=%q err=%v", src, err)
	}
}

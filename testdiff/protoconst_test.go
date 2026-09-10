package testdiff

import (
	"testing"

	"github.com/pschlump/gopher-lua"
	"strings"

	"github.com/pschlump/gopher-lua/parse"
)

func TestProtoConsts(t *testing.T) {
	for _, src := range []string{"local x = 40\nprint(x)\n", "print(42)\n"} {
		chunk, err := parse.Parse(strings.NewReader(src), "k.lua")
		if err != nil {
			t.Fatal(err)
		}
		proto, err := lua.Compile(chunk, "k.lua")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("src=%q", src)
		for i, c := range proto.Constants {
			t.Logf("  Constants[%d] = %#v", i, c)
		}
		for i, s := range proto.StringConstants() {
			t.Logf("  stringConstants[%d] = %q", i, s)
		}
	}
}

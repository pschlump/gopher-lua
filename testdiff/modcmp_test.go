package testdiff

import (
	"crypto/md5"
	"testing"
)

func TestModuleCompare(t *testing.T) {
	src := "local y = x\n"
	// probe-style compile
	a, err := CompileSource([]byte(src), "b.lua")
	if err != nil {
		t.Fatal(err)
	}
	// engine-style compile (same function, but exercise twice for
	// determinism)
	b1, _ := CompileSource([]byte(src), "b.lua")
	b2, _ := CompileSource([]byte(src), "b.lua")
	t.Logf("probe=%x engine1=%x engine2=%x", md5.Sum(a), md5.Sum(b1), md5.Sum(b2))
}

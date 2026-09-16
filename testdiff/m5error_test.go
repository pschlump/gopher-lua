package testdiff

// M5d gate: the byte-exact error suite — interp vs wasm over
// _wasm-err-tests, message heads compared exactly by DiffLogs (traceback
// tails already stripped there). The suite is the spike: every failure
// is either a dialect patch (runtime message families) or a ledger row.

import (
	"os"
	"path/filepath"
	"testing"
)

// ledgeredErrSkips: suite cases whose divergence has a ruling (each skip
// cites its row in docs/Lua-Wasm-Divergence-Ledger.md).
var ledgeredErrSkips = map[string]string{
	// The depth guard fires inside the adapter with no rt line, and the
	// throw's 150-level unwind doesn't reach pcall cleanly (so01 traps).
	// Depth divergence is ruled in row 20; the clean unwind lands with M6.
	"so00.lua": "stack-overflow depth/unwind — ledger row 20, M6",
	"so01.lua": "stack-overflow depth/unwind — ledger row 20, M6",
	// The staged core raise carries no position prefix on the value the
	// script catches; interp and stock C both prefix it. Ruled in row 28.
	"pc10.lua": "pcall-caught core raises lose the position prefix — ledger row 28",
}

func TestWasmErrorSuite(t *testing.T) {
	dir := "../_wasm-err-tests"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no error suite corpus: %v", err)
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".lua" {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Skip("error suite corpus empty — run TestGenM5ErrCorpus")
	}
	skipped := 0
	for _, name := range names {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if why, ok := ledgeredErrSkips[name]; ok {
			skipped++
			t.Logf("skip %s (%s)", name, why)
			continue
		}
		c := Case{Name: name, Dir: dir, Source: src}
		a := NewInterp("interp").Run(c)
		b := (&WasmEngine{name: "wasm", SkipUnsupported: true}).Run(c)
		for _, l := range b {
			if len(l) > 9 && l[:9] == "SKIP-UNSU" {
				t.Errorf("%s: %s", name, l)
			}
		}
		if d := DiffLogs(a, b); d != "" {
			t.Errorf("%s:\n%s", name, d)
		}
	}
	t.Logf("%d comparable, %d ledgered skips", len(names)-skipped, skipped)
}

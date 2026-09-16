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
	// (empty — rows 20's unclean unwind and 28's missing prefix were
	// fixed with the call_body rebasing in M6; so00/so01/pc10 pass)
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

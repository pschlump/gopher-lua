package testdiff

// M5e gates: the backend over the full corpora, differentially against
// the interpreter oracle. _wasm-tests is the opcode matrix; _glua-tests
// is the fork's own conformance suite. Every skip cites its divergence
// ledger row (docs/Lua-Wasm-Divergence-Ledger.md; the shared maps live
// in skips.go so cmd/testdiff applies the same ones).

import (
	"testing"
)

// wasmGluaFull runs interp vs wasm over a corpus and fails on any
// unruled divergence.
func wasmGluaFull(t *testing.T, dir string, skips map[string]string) {
	t.Helper()
	cases, err := LoadCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus %s: %v", dir, err)
	}
	cases, skipped := FilterSkips(cases, skips)
	engines := []Engine{NewInterp("interp"), &WasmEngine{name: "wasm", SkipUnsupported: true}}
	results := RunCorpus(cases, engines)

	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("%s:\n%s", r.Name, d)
		}
	}
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge (see ledgered skip map)", bad, len(cases))
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
}

// TestWasmGluaFull: the M5 milestone gate — full _glua-tests, interp vs
// wasm, 0 unledgered DIVERGE.
func TestWasmGluaFull(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	wasmGluaFull(t, "../_glua-tests", gluaWasmSkips)
}

// TestWasmFullMatrix: the regenerated opcode matrix stays green.
func TestWasmFullMatrix(t *testing.T) {
	wasmGluaFull(t, "../_wasm-tests", map[string]string{
		// (empty — row 10u's table.sort skips were removed with the M6
		// call_body stack-rebasing fix; tbl07/cb00/tsort01 pass)
	})
}

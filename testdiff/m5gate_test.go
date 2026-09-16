package testdiff

// M5e gates: the backend over the full corpora, differentially against
// the interpreter oracle. _wasm-tests is the opcode matrix; _glua-tests
// is the fork's own conformance suite. Every skip cites its divergence
// ledger row (docs/Lua-Wasm-Divergence-Ledger.md).

import (
	"testing"
)

// gluaWasmSkips: _glua-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md).
var gluaWasmSkips = map[string]string{
	"coroutine.lua": "coroutines out of scope for the Redis subset — row 22",
	"issues.lua":    "yield across C boundary (coroutines) — row 22",
	"db.lua":        "debug.getinfo introspection over wasm frames — row 23",
	"goto.lua":      "loadstring-mixed C-interpreted chunks — row 24",
	"vm.lua":        "loadstring + gopher parser-error wording (register overflow) — row 24",
	"math.lua":      "library-error wording/behavior (math.max arity text) — row 25",
	"strings.lua":   "library-error wording/behavior (string.dump unsupported in the fork) — row 25",
}

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

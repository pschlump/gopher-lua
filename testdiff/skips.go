package testdiff

import "path/filepath"

// CorpusSkips is the single source of truth for ledgered corpus skips,
// shared by the gate tests and cmd/testdiff (M6b): a skip map that lived
// only on one side would let the CLI leg and the go-test leg disagree
// about what is excluded. Every skip must cite its row in
// docs/Lua-Wasm-Divergence-Ledger.md (§8.9: a divergence without a row is
// a bug; FilterSkips panics on a reasonless skip).
func CorpusSkips(dir string) map[string]string {
	switch filepath.Base(dir) {
	case "_glua-tests":
		return gluaWasmSkips
	case "_wasm-tests":
		return wasmTestsSkips
	default:
		// _wasm-err-tests carries no skips: the M6 call_body rebasing
		// cleared rows 20/28 (stack-overflow unwind, pcall prefix).
		return nil
	}
}

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

// wasmTestsSkips: _wasm-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md). Rows 41/42 were
// fixed (branchPartner for the NOP'd jump partner; LNumber2I/ConstIndex
// -0.0 sign) — whlc00-03 and nz00-01 were unskipped with the fix.
var wasmTestsSkips = map[string]string{}

// ledgeredErrSkips: _wasm-err-tests cases whose divergence has a ruling
// (each skip cites its row in docs/Lua-Wasm-Divergence-Ledger.md).
var ledgeredErrSkips = map[string]string{
	// (empty — row 20's unclean unwind and 28's missing prefix were
	// fixed with the call_body rebasing in M6; so00/so01/pc10 pass)
}

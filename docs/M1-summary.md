⏺ M1 complete — gate green. Summary:

  What was built

  ┌───────────────────────────┬─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
  │           Piece           │                                                                              Contents                                                                               │
  ├───────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ testdiff/testdiff.go      │ Engine interface, corpus loader, skip-with-mandatory-reason filter, sequential runner, log differ with context reporting                                            │
  ├───────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ testdiff/normalize.go     │ The cross-engine contract: canonical %.14g numbers with C-style inf/nan, quoted strings, DFS cycle-safe table serialization with sorted keys, no addresses ever     │
  ├───────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ testdiff/interp.go        │ Interpreter oracle — fresh LState per case, event log (PRINT / ERROR / final GLOBALS serialization), and the deterministic shim: print hook, pure-map               │
  │                           │ os.getenv/setenv, constant os.execute/time/clock/date, fixed os.tmpname, engine-owned math.random/randomseed                                                        │
  ├───────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ testdiff/selfdiff_test.go │ The M1 gate                                                                                                                                                         │
  ├───────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
  │ cmd/testdiff/main.go      │ CI/manual CLI — -corpus, -engines interp,interp, -v, exit 1 on diff                                                                                                 │
  └───────────────────────────┴─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘

  Gate results

  - _glua-tests: 10/10, 0 diffs (the M1 gate proper)
  - _lua5.1-tests: 24/24, 0 diffs, zero skips — and this corpus is larger than what script_test.go runs (it executes all 24 files including verybig, sort, gc, nextvar), so the oracle covers more than
    the existing suite
  - Runs per-commit automatically (CI's go test ./...); the heavy lua5.1 corpus (~63s, dominated by verybig.lua compiling a 120k-line chunk per engine) is -short-skippable for local iteration
  - Verified CI's go-inline regeneration produces a zero-diff tree — edits mirrored in _vm.go/_state.go are exact

  Two real findings (self-diff earned its keep immediately)

  1. math.randomseed is a no-op on Go ≥ 1.24 (mathlib.go:202 calls rand.Seed, which is disabled by default via randseednop) — and the unseeded math.random stream is process-random, unlike C Lua's
     deterministic start. Caught concretely by sort.lua, whose 30,000-element sort comparison counts differed between two interpreter runs in the same process. The harness shims it with an
     engine-owned rand.New(rand.NewSource(42)); the proper fork fix (per-state RNG in mathlib) is ledgered.
  2. os.tmpname() paths are run-unique and leak into error messages (verybig.lua). Shimmed to a fixed name with engine-side cleanup.

  Both are recorded in the new docs/Lua-Wasm-Divergence-Ledger.md with the §8.9 rule attached (a divergence without a row is a bug; a row without a test is a bug), plus the documented print-blindness
  note and the os.* shim ruling.

  One process lesson now saved to memory: CI regenerates vm.go/state.go from _vm.go/_state.go before testing, so any future edit to the generated files must be mirrored in the sources — this
  session's regeneration check confirmed the discipline holds.

  Nothing committed yet, as before. Next per the plan: M2 — compile stock C Lua 5.1 whole to wasm (wasi-sdk/clang toolchain, shims for print/os.*, run _lua5.1-tests inside wazero, seed the C-column
  of the differential matrix). It needs the wasi-sdk toolchain installed on this machine — say the word and I'll set it up.

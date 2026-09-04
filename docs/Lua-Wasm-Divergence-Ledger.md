# Lua→Wasm Divergence Ledger

Companion to `docs/Lua-Wasm-Design-and-Test-Plan.md` §8.9.

**Rule: a divergence without a ledger row is a bug; a row without a test is a bug.**

Where the engines disagree (or any engine disagrees with C Lua 5.1, the
semantic authority), the ruling lives here. Rows record: what diverges,
why, the ruling, and the covering test or shim.

| # | What | Where | Why / ruling | Covered by |
|---|---|---|---|---|
| 1 | `math.randomseed` is a no-op on Go ≥ 1.24 | `mathlib.go:201-204` (`rand.Seed`) | Go 1.24 made `rand.Seed` a no-op by default (`GODEBUG randseednop`), so seeding does not reproducibly reseed the global source. Additionally the unseeded `math.random` stream is process-random, unlike C Lua 5.1 which starts deterministically (`srand(1)`-equivalent). **Ruling:** fork bug; fix is a per-`LState` `rand.New(rand.NewSource(...))` in mathlib. Until fixed, the testdiff shim replaces `math.random`/`math.randomseed` with an engine-owned deterministic source (required of every engine — the C oracle included). | `testdiff/interp.go` shim; caught by `sort.lua` in `TestSelfDiffLua51Tests` |
| 2 | `os.tmpname()` returns a process-unique path | `oslib.go` | Temp paths embed `/var/folders/.../T/<pid-ish>` and differ run to run; `verybig.lua`'s error message embeds the path. **Ruling:** harness shims `os.tmpname` to a fixed relative name, removed after the run; every engine must provide the same shim. | `testdiff/interp.go` shim; caught by `verybig.lua` |
| 3 | (design note) print-diffing is blind to string/number distinction | harness | `print(1)` and `print("1")` produce identical log lines (Lua print semantics: strings raw). Corpus asserts and GLOBALS serialization (strings quoted) cover the distinction in practice. **Ruling:** accepted blindness, documented. | `testdiff/normalize.go` `PrintArg` comment |
| 4 | `os.time`/`os.clock`/`os.date`/`os.execute`/`os.getenv`/`os.setenv` are environment-dependent | `oslib.go` | Wall clock, exit codes, and the process environment vary across runs and machines. **Ruling:** harness shims all six to constants/pure-map equivalents with Lua-conforming shapes; real semantics are tested by the C oracle's own suite, not by the diff. | `testdiff/interp.go` shim |
| 5 | `os.time`/`os.date` shim semantics differ between engines | testdiff shims | interp pins both to constants (no calendar arithmetic); clua implements real UTC calendar conversion over the pinned 2000-01-01 instant (files.lua's time-arithmetic asserts need it). **Ruling:** align interp's shim to the clua semantics before M5 cross-engine diffs. | `testdiff/interp.go` vs `runtime/luawasm.c` g_gettime/g_getdate |
| 6 | Lua 5.1.5 `tonumber(s, base)` wraps negatives on ILP32 | `runtime/lua51/src/lbaselib.c` | `strtoul`/`unsigned long` is 32-bit on wasm32: `tonumber('-10', 36)` → 4294967260. Patched to `strtoll` (LP64 behavior). | `TestCLuaConformance` math.lua |

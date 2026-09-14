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

## M4 rows (2026-09-14 — backend v1)

| # | Divergence | Where | Notes |
|---|---|---|---|
| 7 | GLOBALS content differs between the interp oracle and the C/wasm engines by construction (gopher-lua adds `_GOPHER_LUA_VERSION`, `channel`, `table.foreach`, different `package`/`io` surface; `math.huge` prints `inf` vs `1.7976931348623e+308`) | engine library sets | Excluded from cross-engine diffs (`DiffLogs` drops GLOBALS lines). The globals surface aligns with the host module (M7). |
| 8 | Interp's uncaught-error payloads carry Go-side `stack traceback:` tails the C engines cannot produce | `interp.go` PCall error rendering | Stripped in `DiffLogs`; M5's byte-exact error gate compares message heads (chunk:line + message body). |
| 9 | Error-message wording: gopher-lua `"attempt to index a non-table object(nil) with key 'x'"` vs C 5.1 `"attempt to index a nil value"` (same class for arith/concat errors) | ldebug.c vs `_vm.go` | The M2 oracle contract keeps the stock C texts (they pass `_lua5.1-tests`); the M5 byte-exact suite decides the canonical wording per message family. `_wasm-tests` err00–err02 skip on this row. |
| 10 | `table.sort` on the wasm backend trips the wasm callback machinery (engine: wasmtime `index out of range` panic at the post-run globals dump; CLI: no output after the sort call, clean exit) | ltablib sort under rt_call | Root cause pending the M5 error/EH investigation (suspect: the sort path unwinding through EH-unaware script frames). `_wasm-tests/tbl07` skips on this row. |
| 11 | Register cells are not GC roots: any `luaC_checkGC` inside library calls (e.g. table.sort stack churn) can collect tables referenced only from cells | M3 ABI obligation, v1 backend | v1 mitigation: the engine stops the GC per run (`collectgarbage('stop')`); the M6 arena lifecycle (arena-reset instead of GC for stateless runs) is the durable fix. |
| 12 | OP_CLOSURE/OP_VARARG/upvalue opcodes rejected by backend v1 | `luawasm/emit.go` | Clean compile error; the harness surfaces SKIP-UNSUPPORTED. Closures ABI (Proto-struct emission, rt_newclosure/find_upval/close_upvals) is the first M5 deliverable. |

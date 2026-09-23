# bench-vs-orig — fork vs baseline performance comparison

Standalone Go module (nested under `note/`, so the parent repo's `./...`
tooling ignores it). Imports **both** Lua engines into one process:

- `github.com/pschlump/gopher-lua` → this fork, via `replace => ../..`
- `github.com/pschlump/gopher-lua-orig` → pristine baseline, via
  `replace => ../../../gopher-lua-orig` (adjust if the baseline lives elsewhere)

Measurements per script (adaptive iteration count, ≥2 warmups, `runtime.GC()`
before each timed iteration; median reported):

| row | meaning |
|---|---|
| `orig/new parse+compile` | `parse.Parse` + `lua.Compile` only |
| `orig/new run (interp)` | precompiled proto, fresh `NewState`, `PCall`, `Close` |
| `orig/new TOTAL (CLI)` | `NewState` + `DoString` + `Close` (what `glua script.lua` does minus process spawn) |
| `wasm compile` | `testdiff.CompileSource` (frontend + wasm emit) |
| `wasm run (VM start+run)` | cold start: `testdiff.NewRunEngine` + `Run` — rt-blob + script instantiate, `lnewstate`, stdlib open, `lua_main`, teardown |

Every script is first run once on all three legs and the outputs compared
(`outputs: OK` line) — numbers are only meaningful when the check passes.

`scripts/` — `s0_nop` (pure startup), `s1_hello`, `s2_string`, `s3_table`
(short set); `l1_fib` (fib(30), call-heavy), `l2_nsieve` (2^18..2^20 sieve,
table/arith-heavy), `l3_stringwork` (~150KB text build + scan, string-heavy)
(long set).

## Run

```sh
cd note/bench-vs-orig
go build -o luabench .

./luabench scripts/s0_nop.lua scripts/s1_hello.lua scripts/s2_string.lua scripts/s3_table.lua | tee out-short-wasmtime.txt
GLUA_WASM_ENGINE=wazero ./luabench scripts/s0_nop.lua scripts/s1_hello.lua scripts/s2_string.lua scripts/s3_table.lua | tee out-short-wazero.txt

./luabench scripts/l1_fib.lua scripts/l2_nsieve.lua scripts/l3_stringwork.lua | tee out-long-wasmtime.txt
GLUA_WASM_ENGINE=wazero ./luabench scripts/l1_fib.lua scripts/l2_nsieve.lua scripts/l3_stringwork.lua | tee out-long-wazero.txt
```

Notes on reading the wasm numbers:

- `wasmtime` leg shares one lazy wasmtime `Engine` across runs (JIT of the rt
  blob happens once, during warmup) — it is the differential *oracle* host.
- `wazero` (the production engine) creates a fresh `wazero.Runtime` per run,
  so its `wasm run` row re-instantiates the rt blob every time — that is the
  real per-run production cost.
- The wasm engine stops Lua GC for the run (v1 ABI posture); interp legs run
  with the normal Go GC. Alloc-heavy scripts (nsieve) therefore do GC work on
  the interp side only.
- `RESULTS.md` holds the consolidated 2026-09-23 numbers and reading notes;
  the `out-*.txt` files are the raw runs they came from. The `luabench`
  binary itself is a throwaway artifact (`go build -o luabench .`).

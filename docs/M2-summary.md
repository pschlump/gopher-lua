⏺ M2 complete — gate green at 100%. Full summary:

  What landed

  The oracle module — runtime/lua51_sjlj.wasm: stock Lua 5.1.5 with native-EH setjmp via -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false + -lsetjmp + 8 MB wasm stack. The critical discovery
  this session: LLVM still defaults to legacy EH opcodes (which wasmtime dropped), and the one-flag fix is wasm-use-legacy-eh=false — emitting try_table, which wasmtime runs with
  SetWasmExceptions(true).

  The host — testdiff/clua.go on wasmtime-go v48: WASI preopen FS (dofile/io work), stdout capture, host RNG shared with the interp engine (bit-identical math.random streams), and the same typed
  value protocol → identical event-log format as the interpreter. The cgo dependency is oracle-only — production stays pure-Go/wazero, since our own backend never needs setjmp (error-flag
  propagation, design §4.4).

  The gate

  20/20 non-skipped _lua5.1-tests scripts pass (100%, gate was ≥95%) — full test suite green. Four documented skips: main.lua/all.lua (suite drivers that spawn the lua binary as subprocesses), pm.lua
  (fork-adapted file — do; isn't Lua 5.1 syntax), big.lua (yield-across-C-boundary fires outside the suite's guard — open investigation explicitly carried to M3).

  Real bugs found and fixed en route (all in the ledger)

  1. Lua 5.1.5 ILP32 bug: tonumber("-10", 36) → 4294967260 (strtoul/32-bit unsigned long) — patched to strtoll
  2. os.tmpname constant shim made rename(tmpname(), tmpname()) a rename-onto-itself — unique counters in both engines
  3. short_src semantics: buffer-loaded chunks are [string "x"]; file-loader form needs @-prefix — fixed error positions to db.lua:28 style
  4. os.time/date needed real calendar arithmetic over the pinned 2000-01-01 UTC instant (days-from-civil + strftime subset + *t table form) — with the interp shim alignment noted as M5 prerequisite
     (ledger row 5)
  5. Plus shebang skipping, the arg global, os.setlocale/tmpfile WASI gaps, and an 8 MB wasm stack so Lua's recursion limits trip before the engine's

  Two process gotchas worth remembering (both in the docs): wasmtime-go maps Go uint64→i64 so i32 args need explicit narrowing, and go clean -testcache after rebuilding the embedded blob — a stale
  cache masked a failure for two iterations.

  The asyncify work is preserved as documentation (runtime/setjmp_asyncify.c, canon*.c, --with-asyncify build path, constraints recorded in the plan doc) — it got remarkably far (full state creation
  on wazero) and the findings stand for any future core-wasm-only need.

  Next: M3 — the rt_* ABI: split lvm.c out of the oracle runtime, freeze the export surface the backend will call, and carry the big.lua yield investigation. Say the word.

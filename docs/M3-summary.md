⏺ M3's session increment is checkpointed. Here's where things stand:

  What M3 settled today — the architecture question

  The design doc had glossed over the hardest M3 question: script.wasm and runtime.wasm are separate modules, and wasm modules don't share memory by default — every pointer in the ABI design would
  have been meaningless across the boundary. The experiment (wasm/sharedmem_test.go, now a green gate) proved the answer works in wazero:

  1. One shared address space: the script module imports the runtime's exported memory. Verified end-to-end — emitted data segments land in shared memory and are readable by the C module, emitted
     stores are visible to C, and C functions (rt_*) operate on pointers written by emitted code.
  2. Downward-only calls — and the consequence is now ABI law: the runtime never calls into script code. When C must run Lua (function __index, sort comparators, pcall into a compiled function), the
     runtime's own lvm interprets the proto — script protos are real Proto structs in shared memory. Correctness never depends on the compiled path; compiled code is pure acceleration. Cross-module
     dispatch becomes an M8 perf concern, not a correctness one.
  3. The emitter grew ImportMemory (with the fix that it belongs in the import section — a classic wasm ordering trap, now regression-tested).

  The frozen ABI — first batch

  - runtime/rt_abi.h — the contract: LUA_RT_ABI = 1, status codes (RT_OK/RT_ERR), the Lua 5.1 tag constants including the collectable bit (they're frozen by Lua 5.1 being final), arith op codes
    aligned to the OP_* numbers the backend already compiles from, and the error protocol — no setjmp crosses the ABI: errors are staged, copied out as bytes, sticky until rt_err_clear, and the
    emitted code propagates RT_ERR up its frame chain.
  - runtime/rt_abi.c — first batch implemented over stock internals: version, rt_set_state, value constructors, rt_intern, rt_newtable, and protected rt_gettable/rt_settable (via
    luaD_rawrunprotected, with oracle-format script:LINE: error prefixes). Linked into lua51_sjlj.wasm with 12 exports — the M2 conformance gate still passes, and the static asserts pin the 16-byte
    TValue layout the whole ABI rests on.

  What remains for M3 completion (recorded in the plan doc)

  The rest of the surface (arith, len, eq/lt/le, concat, closures/upvalues, call fallback, forprep), the native unit-test harness with ASan/UBSan, the emitted-side seam smoke against the real runtime
  (today's proof used a stub C module), the big.lua yield investigation carried from M2, and the formal freeze review. No signature or semantic changes without bumping LUA_RT_ABI.

  Everything is in the plan doc's M3 status note and project memory. Worth noting: this was the last milestone gate that could change the ABI's shape — everything from M4 onward is filling in a
  proven structure. Next session picks up the remaining rt_* surface and the native tests.


---


⏺ M3 complete. The session's final state:

  What's now in place

  The full rt_* ABI — runtime/rt_abi.h (the frozen LUA_RT_ABI=1 contract) and runtime/rt_abi.c (24 exports compiled into
  lua51_sjlj.wasm): value construction, intern, newtable, protected gettable/settable, all seven arith ops (with string coercion and
  metamethods), len, eq/lt/le, concat, rt_call — the universal call fallback where the runtime's lvm interprets Lua closures and C
  functions run directly — forprep, error raise, frame allocation, and the complete error-staging protocol with script:LINE: prefixes
  applied in the buffer.

  Three independent gates, all green:
  - runtime/tests/run.sh — native unit tests PASS on plain, ASan, and UBSan (the plan's L1/L2 runtime gate)
  - TestRTSeamSmoke — an emitted module drives the real ABI against the real C runtime through shared memory (the M3 milestone
    validation)
  - M1 self-diff, M2 conformance (100%), M0 emitter gates — all unaffected; full go test ./... green

  The debugging haul (now ABI documentation)

  Getting the seam green surfaced six real contract-level lessons, each recorded in the plan doc: luaV_tonumber returns a value
  pointer, not a boolean; luaV_concat leaves the result in the first window slot and you adjust top -= n-1, not n; boolean TValues
  aren't collectable; ABI cells must be genuine contiguous arrays (two locals aren't adjacent — this one masqueraded as a GC bug for
  a while); callables need a GC-visible reference while their cell is live — a standing backend obligation; and LUA_CORE gates
  luai_num*.

  Also recorded in memory: the two meta-traps that repeatedly cost time — go test caching env-var changes, and stdout buffering
  making print ordering lie (fprintf(stderr) for truthful sequencing).

  Carried to M4

  The closures/upvalues ABI (with the Proto-struct layout freeze for emitted protos — the last piece of the ABI), setlist, and the
  big.lua yield investigation. M4 itself — the backend, FunctionProto → wasm — now starts with a proven seam on both sides: an
  emitter that can express what's needed, and a runtime ABI that's tested three ways.

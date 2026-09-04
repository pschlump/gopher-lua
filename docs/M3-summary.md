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

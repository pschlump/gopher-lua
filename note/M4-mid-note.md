⏺ Written: note/m4-to-m5-plan.md — here's the narrative restored:

  Where the project stands

  Done and gated: M0 (wasm emitter, measured on wazero), M1 (differential harness — self-diff 0 across both corpora), M2 (stock C Lua 5.1
  at 100% of _lua5.1-tests on wasmtime via new-EH setjmp), M3 (the frozen rt_* ABI — native tests pass on plain/ASan/UBSan, and an
  emitted module drives the real ABI through shared memory).

  M4 is mid-bring-up. The backend exists and compiles real scripts: flattened control flow, every opcode lowered through the ABI with
  line numbers as immediates, inline f64 fast paths for arithmetic, calls routed through rt_call so the runtime's interpreter executes
  callees (correctness never depends on the compiled path). Eleven bring-up bugs fixed along the way — import arities, stack-discipline
  errors, an off-by-one in the dispatch br, operand decoding, a bad intern pointer, a missing rt_set_state in the engine.

  The one bug in flight (paused exactly here): the engine traps with wasm trap: uninitialized element — a call_indirect into a null table
  slot — on the first script that touches a global, while the seam test calling the same rt_getglobal for an existing global works fine.
  The prime suspects are the engine's WASI configuration (the seam has none) or a metamethod C-function whose pointer never got
  registered in wasm's 196-slot indirect function table. Also pending: one rebuild of the runtime blob to pick up the
  Lua-5.1-raw-tag-constants fix I just made in rt_abi.h.

  Still owed for M4: fix that trap, green smoke test, cmd/luawasmc/cmd/luawasm-run — your save-the-.wasm-to-file request, which slots in
  naturally as the compile CLI — and the ~200-case differential subset gate.

  The note also carries the M5 preview (closures ABI + Proto-struct emission, VARARG, byte-exact error messages, full corpus), and the
  standing debug meta-lessons (go clean -testcache, stderr-not-stdout for truthful ordering) so those stop costing us time.

  Project memory is updated with a pointer to the note, so any future session picks up from exactly this cliff. Say the word when you
  want me to push on the trap.


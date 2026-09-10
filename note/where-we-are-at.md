⏺ Summary of where we are — this session moved M4 from "dead trap" to "one bug away from executing scripts correctly."                                                 ────────────────────────────────────────────────────────────────────────────────────────
                                                                                                                                                                       New file not yet staged.
  The trap is solved — and it wasn't exotic                                                                                                                            Run `git add :/luawasm/emit.go` to see line counts.

  The uninitialized element trap that dominated the last session was the engine losing its rt_set_state call during an earlier edit. With curL = NULL, every           ────────────────────────────────────────────────────────────────────────────────────────
  NULL-dereference landed in wasm's mapped page 0, read garbage, and eventually jumped through a wild call_indirect. Restored the call → all script-content bisect     luawasm/ops.go (untracked)
  cases run (empty, locals, GETGLOBAL, SETGLOBAL, NEWTABLE, for-loops, concat — no traps).                                                                             ────────────────────────────────────────────────────────────────────────────────────────
                                                                                                                                                                       New file not yet staged.
  Two more real bugs found and fixed after that                                                                                                                        Run `git add :/luawasm/ops.go` to see line counts.

  1. luawasm_init local indices off by one — the step parameter occupies local 0, so lS/lK/lC had to be 1/2/3. With the wrong indices, the init gate read the cursor   ────────────────────────────────────────────────────────────────────────────────────────
     instead of the step and no constants were interned — which is why every constant-using script saw nils. Fixed; the kcells now dump perfectly (40.0 as tag 3,      note/deadlock-prevention.md (untracked)
     interned strings deduping to shared TStrings).                                                                                                                    ────────────────────────────────────────────────────────────────────────────────────────
  2. rt_setglobal double-popped — lua_setglobal is lua_setfield, which pops the value itself; my body decremented top again. Fixed; host-side setglobal→getglobal      New file not yet staged.
     round-trips correctly (verified zz2 = 7 lands as tag 3).                                                                                                          Run `git add :/note/deadlock-prevention.md` to see line counts.

  The one open bug (where the session ended)                                                                                                                           ────────────────────────────────────────────────────────────────────────────────────────
                                                                                                                                                                       note/M4-mid-note.md (untracked)
  Emitted zz = 5 runs, calls rt_setglobal exactly once with the correct "zz" key cell (proven via the rt_sgcalls/rt_sgname counter exports I added to the runtime),    ────────────────────────────────────────────────────────────────────────────────────────
  returns RT_OK, lua_main returns 0 — and getglobal("zz") afterward is still nil. The host-side identical call works. So the difference lives in the calling context:  New file not yet staged.
  something about the Lua state or stack when the call comes from wasm frames inside lua_main vs from the host. That's the first thing to dig into next session —      Run `git add :/note/M4-mid-note.md` to see line counts.
  prime suspect is the stack discipline around luaD_rawrunprotected when L->top isn't at a call-frame boundary (api_checknelems/luaV_settable behaving differently
  with an unexpected top).                                                                                                                                             ────────────────────────────────────────────────────────────────────────────────────────
                                                                                                                                                                       note/m4-to-m5-plan.md (untracked)
  State of the tree                                                                                                                                                    ────────────────────────────────────────────────────────────────────────────────────────
                                                                                                                                                                       New file not yet staged.
  - Debug artifacts to keep for now: testdiff/trapiso_test.go (the environment-isolation harness), zz2_test.go (the current repro + kcell dump), mini_test.go,         Run `git add :/note/m4-to-m5-plan.md` to see line counts.
    engcopy_test.go. Older triage tests (b2–b5, dump, initprobe, steplog, zz, kcell, modcmp, protoconst) are deletable at cleanup.
  - Runtime has temporary debug exports (rt_sgcalls, rt_sgname) — remove at M4 completion.                                                                             ────────────────────────────────────────────────────────────────────────────────────────
  - Memory and note/m4-to-m5-plan.md are updated with all of this (the note's §5 "open trap" section is now stale — the trap is resolved; §6 steps 1–4 done).          runtime/build.sh
                                                                                                                                                                       ────────────────────────────────────────────────────────────────────────────────────────
  Still owed for M4 after this bug: TestWasmBackendSmoke green, cmd/luawasmc (save .wasm to file) + cmd/luawasm-run, the ~200-case differential subset gate, and        34    -Wl,--export=rt_err_pending -Wl,--export=rt_err_clear -Wl,--export=rt_err_stage_c
  triage-test cleanup.                                                                                                                                                      opy \
                                                                                                                                                                        35    -Wl,--export=rt_arith -Wl,--export=rt_len -Wl,--export=rt_eq -Wl,--export=rt_lt \
  Ready to resume on the setglobal-from-wasm bug whenever you are.

# M4 → M5 Plan & Status — UPDATE 2026-09-10 (session 4: bring-up bugs RESOLVED, CLIs built, emitCall fix IN FLIGHT)

## READ FIRST — exact tree state at this checkpoint

Nothing committed (as always). `luawasm/ops.go` contains an **IN-FLIGHT, UNCOMPILED emitCall rewrite with one KNOWN BUG** (details below) — tests will NOT be green until the one-line fix lands. Everything else in this section was verified green on the last full run (`go test ./testdiff/` → ok, 66.9s, which includes the M1 + M2 corpus gates).

## Verified fixes this session (full suite green before the in-flight edit)

1. **CONCAT lower was missing its `Call(rt_concat)`** — pushed 4 args then `checkStatus()` consumed the *line number* (1) as the status → `return 1` with no rt_ call, no failfn, no staged error (the "status=1, empty error" mystery). One-line fix in emit.go; `print('a'..'b')` → PRINT ab.
2. **for-loop infinite spin: `forprep_body` read the conversion from the UNFILLED temp** — `luaV_tonumber` returns the converted value as a POINTER (the cell itself for numbers; the temp only for strings); the body did `nvalue(&nv)` on the unfilled temp → on wasm that's 0.0 → coerced {1,3,1} into {0,0,0} → step=0 → `0>=0` loops forever. Exactly the M3 lesson; fixed to `nvalue(x)` through the returned pointer (rt_abi.c). Diagnosed via fuel instrumentation + an rt_forprep input-capture diag: fpin=1,3,1 while the final cells read 0.0.
3. **FORLOOP never set R(A+3)'s TAG** (ops.go) — stored the f64 value but the tag stayed nil from the entry fill → `print(i)` would see nil. Added the tag store (`I32Const(3).I32Store8(8)`).
4. rt_laststatus was missing from build.sh's export list (diag read "missing export"); zz_test had a stale slice-bounds panic (clamped).

Result: mini matrix 8/8 (`zz=print`, `print(42)`, locals, `40+2`, `x+y`, `t.k` tables, `for i=1,3 do print('i',i) end` → PRINT i 1/i 2/i 3, `'a'..'b'` → PRINT ab), TestWasmBackendSmoke, seam (TestRTSeamSmoke + TestRTDirectABI), M1, M2 — all green.

## CLIs built (Philip's save-to-file request — functionally complete, one bug found by their demo)

- **cmd/luawasmc** — `.lua` → `.wasm`; flags `-o` (output path, default input basename), `-name` (chunkname, default input path). Verified: 132-byte demo source → 3245-byte module.
- **cmd/luawasm-run** — loads a saved artifact (`WasmEngine{Precompiled}`), PRINT/STDOUT payloads → stdout, ERROR/ENGINE-ERROR/SKIP-UNSUPPORTED → stderr + exit 1, `-v` prints the full event log. Chunkname is `@<basename>` for error parity.
- Round-trip demo verified working for: table construction `t[#t+1]='item-'..i`, `#t`, numeric for, concat, setglobal.

## IN FLIGHT: emitCall results off-by-one (the ipairs bug) — NEXT ACTION

**Symptom (found by the CLI demo):** `for i, v in ipairs(t) do` → `"bad argument #1 to '?' (table expected, got function)"`. `ipairs(t)` returns (next, t, nil); rt_call wrote results over the arg cells at &R(A+1), but OP_CALL semantics put results at R(A).. — everything one cell too high, so TFORLOOP called `next(next, …)`. `print(42)` masked this for weeks: want=0 → no results written.

**Fix design (LANDED in ops.go `emitCall`, has one known bug):**
- callee `R(A)` → scratch cell 0;
- args shifted down one cell: `R(A+i) ← R(A+1+i)` ascending (B>0: static copies; B==0: dynamic loop over nargs);
- `rt_call(scratch0, &R(A), nargs, want, line)` → results land exactly at R(A) for static AND multret; tailcall/top-adjust paths unchanged and now correct.

**KNOWN BUG IN THE LANDED EDIT — fix before anything else:** the B==0 dynamic shift uses `lT1` as its loop index (ops.go ~line 156), but `lT1` holds `want`, which is still passed to rt_call after the loop → breaks `f(g())` shapes (B==0 && C>0). **Fix: use `lSt` as the shift index instead** (free until the rt_call result lands there) and change `dynCellAddrAlt` to index via `lSt`; `lT0` (nargs) stays the loop bound. Then: `go build ./...`, rerun the demo (ipairs must print), mini matrix + smoke + full testdiff.

## Ordered plan to close M4

0. **Land the lT1→lSt fix** (above); rebuild; demo + mini + smoke + full suite green.
1. **TFORLOOP audit:** `emitTForloop` passes funcell=&R(A), argcells=&R(A+1) (rt_call semantics → results at R(A+1)) then moves C cells to R(A+3). Verify against vm.go OP_TFORLOOP; the fixed demo exercises it immediately.
2. **Wire "wasm" into cmd/testdiff's `-engines` switch** (currently interp/clua only). Check normalize.go handles the engine's STEP lines (interp emits none — comparison layer must filter them or the gate false-fails).
3. **Differential gate:** ~200-case subset, interp vs `WasmEngine{SkipUnsupported:true}`, goal 100% log match on the compilable subset; SKIP-UNSUPPORTED expected on closures/varargs (M5 work).
4. **Cleanup:** remove ALL diag exports from rt_abi.c + build.sh (rt_sgcalls/sgname/sgvaltag, rt_ggcalls/ggname/ggresult, rt_ccalls/carg, rt_gtcalls/gt, rt_lastfn/laststatus/failfn, rt_fpcalls/fpin) and their C bodies; delete triage tests (zz, zz2, b2–b5, dump, initprobe, steplog, modcmp, protoconst, engcopy, bisect, forfuel, formini; keep trapiso harness + mini matrix + wasmbackend smoke + rtseam; calldiag's kcell/frame dump is generally useful — fold a trimmed version into mini or keep). After blob rebuild: `touch testdiff/clua.go` + `go clean -testcache` (embed staleness!).
5. **Docs:** M4 status block in docs/Lua-Wasm-Design-and-Test-Plan.md §9; ledger rows for anything DIVERGE surfaced by step 3.
6. **Carried (post-M4):** interp os.time/date shim alignment (ledger row 5) before M5; big.lua yield investigation.

## Meta-lessons (new this session)

(a) `//go:embed lua51_sjlj.wasm` — `go clean -testcache` alone may not re-embed a rebuilt blob; `touch testdiff/clua.go` forces it. (b) wasmtime fuel: set BEFORE instantiation (ctors consume); fuel is store-wide — refill before post-trap diag reads; v48 API = `cfg.SetConsumeFuel(true)` + `store.SetFuel/GetFuel`. (c) The luaV_tonumber pointer-return lesson bit AGAIN (forprep_body) — and the NATIVE M3 tests still passed because benign stack garbage hid it → native ABI tests must assert exact coerced values, not just RT_OK. (d) The CLI demo found in minutes a bug the mini matrix couldn't see (multi-result calls) — run a richer script through luawasmc/luawasm-run whenever emitCall changes.

---

## Session 3 checkpoint (2026-09-09, pre-/exit — historical)

**RESOLVED this session (three root-cause fixes):**
1. The "uninitialized element" trap = engine lost its `rt_set_state` call (curL=NULL). Restored.
2. `luawasm_init` intern pointer used the cursor AFTER writeStrBytes advanced it — interned garbage/empty strings for every constant. Fix: `ptr = lS + cursor - len(s)` (backend.go, both constant loops). Result: `zz = 5` now lands and reads back (tag 3).
3. `emitCall` passed `argcells = &R(A)` instead of `&R(A+1)` — the callee itself became arg0 (`print(42)` printed `<function>`). Fixed in ops.go.

**OPEN BUG (resume here):** `print(42)` → `PRINT nil`. Facts:
- Bytecode: pc0 GETGLOBAL R0=print (kscell Bx=0), pc1 LOADK R1←K[1]=42, pc2 CALL A=0 B=2 C=1.
- Disassembly of emitted lua_main is CORRECT end-to-end: LOADK copies kcells+16 (the 42) into frame+16 (verified i64.store at both offsets); CALL pushes funcell=frame+0, argcells=frame+16, nargs=1, want=0, line=1 (rt_call at 0x3f5).
- Runtime diag (rt_ccalls/rt_carg exports): rt_call fired once; funcell tag=6 (function ✓); **arg0 tag=0 (NIL)** — the cell at frame+16 is nil at call time despite LOADK having just written 42 there.
- So the mystery: LOADK writes frame+16=42; nothing between it and the CALL writes frame+16; yet rt_call reads nil. Remaining suspects: (a) kcell(1) empty at runtime for THIS module (the value-loop writes may still be mis-sequenced for mixed string/number Constants — note print(42) has Constants=["print",42], and the dump that verified cell[1]=tag3 was from the zz=5 module, NOT this one); (b) the frame buffer and kcells alias/overlap (rt_frame_alloc malloc reuse?); (c) the init step-gate emits the two loops in an order where the second clobbers the first.
- **FIRST NEXT STEP: extend testdiff/calldiag_test.go to ALSO dump kcells[0..3] tags right after luawasm_init for the print(42) module** (the machinery exists in zz2_test.go/kcell_test.go — trapEnvNamedBin(binOverride) + UnsafeData read of gKCells). If cell[1] is nil → init value-loop bug; if cell[1]=3 → the LOADK/store path or frame aliasing.

**Mini-matrix current results:** `zz = print` ok(zz set, value unseen); `print(42)`/`print(x)`/arith → PRINT nil (the open bug); `local t={}; t.k='v'; print(t.k)` → ERROR "attempt to index a nil value" line 3 (t or the key cell nil — likely same root cause); for-loop → RUNS but iterates ~36x for range 3 then stack-overflow trap (FORLOOP back-edge/block mapping off — separate bug, not yet investigated); `print('a'..'b')` completes silently, no PRINT (same arg-nil bug + concat produced something callable?).

**Runtime debug exports added (REMOVE at M4 completion):** rt_sgcalls/rt_sgname/rt_sgvaltag, rt_ggcalls/rt_ggname/rt_ggresult, rt_ccalls/rt_carg. Debug tests in testdiff/ (mostly deletable at cleanup): trapiso, zz, zz2, kcell, calldiag, mini, engcopy, b2-b5, dump, initprobe, steplog, modcmp, protoconst, bisect.

**Remaining M4 after the bug:** smoke green, FORLOOP iteration-count bug, cmd/luawasmc (save .wasm to file — Philip's explicit request) + cmd/luawasm-run, ~200-case differential subset gate, cleanup, M4 status block in the plan doc + ledger.

# M4 → M5 Plan & Status

**Written:** 2026-09-08 (session paused mid-debug for this note)
**Purpose:** restore the narrative — the original milestone plan, where everything stands, what is in flight at this exact moment, and what remains.

---

## 1. The project in one paragraph

We are building a **Lua 5.1 → WebAssembly compiler** inside this gopher-lua fork (`pschlump/gopher-lua`), to be the scripting engine of your Go implementation of Redis. Design: reuse the existing frontend (lexer/parser/`compile.go` → `FunctionProto` bytecode) unchanged; a new wasm backend emits per-script wasm modules; a **C Lua 5.1 runtime** (vendored, lightly ported) is compiled once to `runtime/lua51_sjlj.wasm` and provides tables/strings/GC/metamethods through a frozen `rt_*` ABI. The script module and runtime module **share one linear memory**; calls are downward-only (script → runtime), and the runtime's own `lvm` interpreter is the universal fallback — correctness never depends on the compiled path. Full specs: `docs/Lua-Wasm-Design-and-Test-Plan.md` (engineering) and `docs/Performance-Improvement-Plan.md` (Option E context). Divergences: `docs/Lua-Wasm-Divergence-Ledger.md`.

## 2. The original milestone plan and where each stands

| M | Deliverable (gate) | Status |
|---|---|---|
| **M0** | wasm emitter spike; execute on wazero; wasm2wat validates | ✅ **Done 2026-09-03.** `wasm/` package. Measured: wazero instantiate ≈7.4µs, call ≈21ns, stack ≥2M frames. |
| **M1** | Differential harness; self-diff = 0 diffs across corpora | ✅ **Done 2026-09-03.** `testdiff/` + `cmd/testdiff`. `_glua-tests` 10/10, `_lua5.1-tests` 24/24, zero skips. Every engine must install the harness shim (print hook, deterministic `os.*`, engine-owned `math.random`). |
| **M2** | Stock C Lua 5.1 in wasm passes ≥95% of `_lua5.1-tests` | ✅ **Done 2026-09-03.** `runtime/lua51_sjlj.wasm` built with `-mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false` (new-EH setjmp) + `-lsetjmp` + 8MB stack, hosted on **wasmtime-go v48** (`SetWasmExceptions(true)`). **20/20 non-skipped pass (100%)**; 4 documented skips (main/all = subprocess drivers; pm.lua = fork-adapted syntax; big.lua = yield-boundary investigation, still open). Port fixes: `strtoll` for ILP32 tonumber, `@`-chunknames, shebang skip, `arg` global, deterministic `os.time/date` with real calendar arithmetic. |
| **M3** | `rt_*` ABI implemented, frozen, native tests + ASan/UBSan + seam smoke | ✅ **Done 2026-09-06.** `runtime/rt_abi.h` (contract) + `runtime/rt_abi.c` (24 exports in the runtime module). Gates: `runtime/tests/run.sh` native PASS ×3 (plain/ASan/UBSan); `testdiff/rtseam_test.go` emitted-module-drives-real-ABI PASS. Architecture proven: shared memory via import, downward-only calls, lvm fallback. |
| **M4** | Backend v1: pure-inline + arith/compare fast paths + CALL/RETURN, flattened control flow; corpus subset ~200 cases 100% match | 🔶 **In progress — this note.** Details below. |
| **M5** | Backend v1 complete: all 41 opcodes incl. closures/upvalues/vararg; `_glua-tests` + curated Redis corpus 0 unledgered DIVERGE; byte-exact error messages | ⬜ Not started. |
| **M6** | Hardening: arena/GC watermark, stack limits, deadline, determinism, isolation | ⬜ Not started. |
| **M7** | Redis integration: host module, EVAL/EVALSHA/SCRIPT FLUSH, caps | ⬜ Not started. |
| **M8** | Performance: v2 structured control flow, inline caches; ≥2× interpreter on numeric corpus | ⬜ Not started. |

Also done outside milestones: fork rebrand to `github.com/pschlump/gopher-lua` (go 1.27.0, deps updated, 18 vet printf bugs fixed); emitter gained `ImportMemory`/`ExportGlobal`; `FunctionProto.StringConstants()` accessor.

## 3. M4 — what is built

**The backend (`luawasm/` package):**
- `backend.go` — `Compile(proto, chunkName) → wasm module`: imports the runtime's memory + 21 `rt_*` functions; `luawasm_init` interns all constants into heap-allocated cells (no data segments — bytes written with i64 stores, so no address collisions with the runtime) and installs the chunk name; `lua_main(frame)→status` is the entry. Init currently has a debug `step` parameter (0=allocs only, 1=+chunkname, 2=+interns).
- `emit.go` — the function emitter: registers as TValue cells at `frame+16*k`, local tracking of `top` (interpreter registry semantics), basic-block partition, **flattened control flow** (loop + br_table), all opcode lowerings with `line` args (design A4: line numbers as immediates for error parity).
- `ops.go` — the tricky lowers: arith (inline f64 fast path for ADD/SUB/MUL/DIV + ABI fallback), comparisons via ABI, calls through `rt_call` (lvm executes the callee — the M3 architecture law), returns (static + dynamic multret copy), FORPREP/FORLOOP (inline f64), TFORLOOP, SETLIST.
- Unsupported in v1 (clean compile error): OP_CLOSURE, OP_VARARG, upvalue ops — these are M5 work (need the Proto-struct emission + closures ABI).

**The engine (`testdiff/wasmengine.go`):** `CompileSource(source, name)` (frontend + backend) and `WasmEngine` (Engine interface) — compiles and runs on wasmtime against the runtime module, decodes PRINT/GLOBALS events via the same typed protocol as the other engines, staged-error extraction, ABI-version check (v2). Plus `SkipUnsupported` mode for corpus runs.

**Triage tests currently in `testdiff/`** (debug scaffolding, to be folded into the real gate or deleted at M4 completion): `wasmbackend_test.go` (smoke), `bisect_test.go`/`b2..b5` (script-content bisection), `dump_test.go` (writes `/tmp/smoke.wasm` for objdump), `initprobe_test.go`, `engprobe_test.go` (host-side ABI sequence replication).

**Still missing for the M4 gate:** `cmd/luawasmc` (compile `.lua` → save `.wasm` to file — **you explicitly requested save-to-file; agreed it is the natural final step, planned as the compile CLI**) and `cmd/luawasm-run` (execute a saved artifact); the differential corpus subset gate (~200 cases, interp vs wasm backend); opcode-matrix rows.

## 4. Bugs fixed this session (the M4 bring-up haul)

Each of these was found by module-validation failures or runtime traps, bisected by script content:

1. `rt_set_chunkname` declared with 3 params, C takes 2 → import arity mismatch.
2. `Drop()` before `checkStatus()` everywhere — consumed the status `LocalSet` needed (validation: "values remaining on stack").
3. Dispatch-loop `br` depth off by one (`N-k` → hits the function's implicit result label; must be `N-1-k`).
4. `rt_mknil` import declared (i32,i32), takes (i32).
5. SETTABLEKS operand decode: C is an RK operand (constant flag bit) — was decoded as a register.
6. Missing `line` immediates on GETTABLE/GETTABLEKS/SETTABLE/SELF/LEN/FORPREP calls.
7. `rt_intern` string pointer computed as cursor alone instead of `sbuf+cursor` (memory fault).
8. Engine never called `rt_set_state` after `lnewstate` (NULL `curL` crash).
9. wasmtime-go `Val` accessors (`gv.I32()`), i32 arg narrowing, void-export handling.
10. ABI bumped to **v2** (additive: `rt_getglobal`, `rt_setglobal`, `rt_set_chunkname`, `lglobals` export; seam test updated to check v2).
11. **Lua 5.1 tag convention (diagnosed, fix written, rebuild pending):** Lua 5.1 stores **raw type numbers** in `TValue.tt` — no 5.2-style collectable bit (`iscollectable` is `tt >= LUA_TSTRING`, not a bit test). `rt_abi.h` constants wrongly had `|64`; corrected to raw 0–8. Harmless to C code (which uses Lua macros) but the seam test now returns tag 6 for a function as expected.

## 5. Where we are RIGHT NOW (the paused debug)

**State of the gates:** seam test green (arith/table/len/getglobal-returns-tag/err checks pass — note `check_getglobal` was temporarily changed to *return the raw tag* instead of asserting; restore the assertion). All seven bisect constructs **validate** as wasm modules. The engine runs `luawasm_init` successfully at every step level.

**The one open bug:** the engine traps on the first script that touches a global. Bisect by content:

| script | engine result |
|---|---|
| `` (empty) | ok |
| `local x = 1` | ok |
| `local y = x` (GETGLOBAL of a **missing** global) | **trap** |
| `zz = 5` (SETGLOBAL) | **trap** |
| `local t = {}` (NEWTABLE!) | **trap** |
| `print('hi')` | **trap** |

Facts established:
- The trap is `wasm trap: uninitialized element` (a `call_indirect` into a null table slot) inside the C runtime, reached via `lua_main → rt_getglobal(83) → rt_run(63) → luaD_rawrunprotected(271) → protect_trampoline(64) → getglobal_body(84) → [273 → 272 → 369]`. Function 369 loads a `lua_CFunction` from a closure and `call_indirect`s it.
- The **seam test calls the same `rt_getglobal` for an EXISTING global (`print`) and succeeds** (returns tag 6). So `rt_getglobal` itself works; the difference is engine-vs-seam environment, and/or existing-vs-missing key path reaching a metamethod lookup.
- **Environment deltas engine vs seam:** (a) engine configures WASI (`PreopenDir(c.Dir,"/")` + `SetStdoutFile`) — seam has none; (b) engine instantiates the script module *between* creating the runtime instance and calling `lnewstate`; (c) engine calls `luawasm_init` on the script instance before `lua_main`.
- Suspicion (unverified): the globals table in the engine's state has a metatable (or another `__index` function) whose C function pointer slot is null in wasm's `__indirect_function_table` — possibly a WASI-config-dependent library-init path (`luaL_openlibs` with WASI) installing a C function whose address wasm-ld never registered. The table has exactly 196 slots; "uninitialized element" means the slot exists but is null.

**Also pending:** `rt_abi.h` tag-constant fix requires `sh runtime/build.sh` + `cp` to `testdiff/` (build.sh does the copy) — **not yet rebuilt** since that edit.

## 6. Next steps (in order) to finish M4

1. **Rebuild the runtime blob** (`sh runtime/build.sh`) — picks up the rt_abi.h tag fix; rerun seam + M1/M2 gates.
2. **Isolate the trap:** copy `rtSetup` (seam env) into a probe that adds the engine's deltas one at a time — (a) `store.SetWasi(...)` with the preopen/stdout, (b) script-module instantiation, (c) `luawasm_init` — calling `rt_getglobal` for a *missing* global after each. The step that turns success into the trap names the cause.
3. Likely fix categories: register the missing C function in the indirect table (check `-Wl,--export-table` sizing / `--pass-...`); or stop installing whatever metatable the WASI path adds; or avoid the metamethod path for globals lookups in `getglobal_body` (raw `luaH_get` + explicit metatable walk, mirroring `luaV_gettable` but staying in C-table land).
4. Restore `check_getglobal` to assert `tag == RT_TFUNCTION (6)`.
5. Get `TestWasmBackendSmoke` green (arith, tables, forloop, if/else, concat, calls).
6. **Build the CLIs** — your save-to-file request: `cmd/luawasmc` (reads `.lua`, `CompileSource`, writes `.wasm`; `-o` flag) and `cmd/luawasm-run` (loads a saved `.wasm` into `WasmEngine.Precompiled`, executes with the harness shim). This makes compiled scripts durable artifacts — cacheable by SHA, the Redis model.
7. **Differential gate:** `WasmEngine{SkipUnsupported:true}` vs interp on a hand-picked ~200-case subset (no closures/varargs yet — those scripts SKIP-UNSUPPORTED); goal 100% log match on the compilable subset.
8. Fold the triage tests into a real gate (`TestWasmBackendSmoke` + corpus test), delete the b2-b5/dump/initprobe/engprobe scaffolding.
9. Update `docs/Lua-Wasm-Design-and-Test-Plan.md` §9 with the M4 status block + ledger rows for anything divergent discovered.

## 7. M5 preview (so the shape isn't lost)

- **Closures ABI (ABI v3):** `rt_newclosure(cell, protoIdx, upvalueDescs…)`, `rt_find_upval`, `rt_close_upvals`; requires emitting **Proto structs** into shared memory in the C runtime's exact layout (the "Proto-struct layout freeze") so the runtime's lvm can interpret script functions called as *values* (e.g. via pcall, metamethods, sort comparators) while compiled→compiled calls stay fast.
- **VARARG** lowering (frame shuffle per the interpreter's `state.go:1192-1240` layout).
- **Error-message byte-exact suite** (~100 scripts) — the chunkname+line machinery (A4) exists; message bodies must match gopher-lua's texts.
- **Full `_glua-tests` + curated Redis corpus** differential: 0 unledgered DIVERGE.
- Carried investigations: big.lua yield-across-C-boundary (from M2); interp shim alignment for os.time/date (ledger row 5).

## 8. Standing debug meta-lessons (cost us real time — keep visible)

- `go clean -testcache` after **any** env-var change or blob rebuild — the cache silently replays stale results (bit us twice).
- stdout is buffered, stderr is not — use `fprintf(stderr, …)` for truthful ordering; printf-based "it didn't print" conclusions were wrong twice.
- wasmtime-go: Go `uint64` args map to wasm i64 — narrow to int32; `Val` results need `.I32()`; `Drop()` only when the callee actually returns a value.
- The wasm-ld indirect function table ("uninitialized element") is implicated in the current trap — remember it exists and is sized exactly to address-taken functions.

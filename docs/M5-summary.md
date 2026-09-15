# M5 Summary — backend v1 complete

**Date:** 2026-09-15 · **Commits:** `f1a8958` (A1) → `baf5062` (A2) → `c293dc4` (A3) →
`4b6ae26` (A4) → `d717d38` (M5b) → `b7dc044` (M5c) → `4d00591` (M5d) → M5e
· **Plan:** `note/m5-to-m6-detailed-plan.md` (all seams verified against source there)

**Deliverable:** every opcode in the fork's set lowers; errors are byte-exact
against the interpreter oracle; the full `_glua-tests` gate runs. One commit per
green sub-milestone, gate evidence in each message.

## Gate snapshot

| Gate | Result |
|---|---|
| `_wasm-tests` opcode matrix | 510/510 comparable, 0 diffs, 2 skips (sort — row 10) |
| `_wasm-err-tests` byte-exact suite | 91/93 comparable, 0 diffs, 2 skips (stack-overflow depth — row 20, M6) |
| `_glua-tests` full gate | 3/3 comparable, 0 diffs, 7 ledgered skips (rows 22–25) |
| native rt_* tests | PASS ×3 (plain / ASan / UBSan, incl. the upvalue-registry suite) |
| `go test ./...` | all green |

## What M5 added, per sub-milestone

- **A1 — ABI v3 skeleton** (LUA_RT_ABI 2→3): `Proto.wasm_idx`; the
  `precall_wasm` adapter in ldo.c (CallInfo + frame push + params/vararg split
  + dispatch through `host.wasm_dispatch`; every adapter level rawrunprotects
  and restores cursor/CallInfo/base before re-raising — a longjmp never
  crosses the Go boundary); the wasm-proto registry; the chunked bump frame
  stack; the rt-OWNED open-upvalue registry (`correctstack` re-basing makes
  `L->openupval` unusable on wasm frame addresses).
- **A2 — reentrancy spike**: the C runtime dispatches a registered proto
  through the Go host back into the script module on one store, nested. The
  fallbacks (two-instance scheme / engine dispatch loop) were not needed.
- **A3 — all protos compile**: `(frame, cl, nargs, want) → nret` per proto,
  `lua_dispatch` br_table + the restage loop, thin `lua_main`, one global
  constant pool, `-1` error convention (unambiguous with "1 result").
- **A4 — closures/upvalues/CLOSE + the callback matrix**: OP_CLOSURE →
  `rt_newclosure` with capture descriptors from the pseudo-instruction scan
  (the fork encodes captures only there — `_vm.go:793-803`); gsub/pcall/
  `__index` callbacks pass through the adapter.
- **M5b — varargs + the compat `arg` table**; all 41 opcodes emit.
- **M5c — the tailcall trampoline**: staged tailcalls re-dispatch at the same
  adapter level via the `-2` sentinel — O(1) wasm stack and O(1) frames;
  10⁶-deep flat recursion ≈ 0.7s. C functions and `__call`'d objects decline
  staging and take the rt_call fallback (parity with `_vm.go:587-650`).
- **M5d — the gopher dialect** (`rt_set_dialect(1)`, wasm engine only; the
  clua oracle keeps stock C texts): all core error families byte-exact; the
  activation line-stack (one entry per live rt_run) drives `error()` levels
  with gopher's frame arithmetic; non-string uncaught errors render from the
  staged TValue; ledger row 5 (os.time/date) resolved on the interp side.
- **M5e — full gates**: `__len` metamethod honored; wasm-frame guards in the
  debug machinery; the `_glua-tests` gate with the reasoned skip map.

## ABI v3 (LUA_RT_ABI = 3)

Additive over v2: `rt_wasm_proto`, `rt_wasm_upval`, `rt_wasm_count`,
`rt_newclosure`, `rt_getupval`/`rt_setupval`, `rt_close_upvals`,
`rt_compat_arg`, `rt_clidx`, `rt_err_value_ptr`/`rt_err_stage_value`,
`rt_tail_stage`/`rt_tail_clidx`/`rt_tail_nargs`/`rt_tail_funcell`/
`rt_tail_restage`, `rt_set_dialect`. New host import: `host.wasm_dispatch`.
Internal: `rt_frame_push`/`rt_push_frame_from`, `rt_frame_restore`,
`rt_pcall_caught`, `rt_wasm_enter/leave` (RTW_MAX_DEPTH=150),
`rt_wasm_where`, `rt_gindex_error`.

Status convention: `nret ≥ 0` results at `frame+0..`; `-1` error (exact TValue
staged); `-2` tailcall sentinel; `-3` host-refused.

## Vendored-tree patch list (`runtime/lua51/src/`)

| File | Patch |
|---|---|
| `lobject.h` | `Proto.wasm_idx` |
| `lfunc.c` | `luaF_newproto` init `wasm_idx = -1` |
| `ldo.c` | precall hook → `precall_wasm` adapter; `luaD_pcall` recovery calls `rt_pcall_caught` |
| `ldebug.c` | dialect branches (typeerror/concaterror/ordererror); `luaG_runerror` skips addinfo for wasm CIs; wasm-frame guards in `currentpc`/`getfuncname` |
| `lvm.c` | index errors carry the key under the dialect |
| `lauxlib.c` | `luaL_where` routes wasm frames through the line-stack |
| `lbaselib.c` | `error()` strict-string + no-arg; `assert` strict message arg |

## Bug classes (root-caused, in discovery order)

1. `luaF_newLclosure` leaves `l.p` unset (A2) — the adapter sets it from the registry.
2. Dispatch arm depth: `Br(n-k)` targets the LOOP — same-proto re-dispatch, infinite loop (A3).
3. Latent M4 `frameCellAddrConst`: fixed-count RETURNs staged at absolute addresses (A4).
4. **The reentrancy law** (A4): an `rt_*` body that `luaD_call`s can re-enter the
   ABI — argument statics read after the call must live on the C stack. This is
   the standing design rule for every future rt_*.
5. Init ordering: upvalue rows emitted before the child's `rt_wasm_proto` are dropped (A4).
6. Scratch collision: multret VARARG destinations reach `R(A+nv-1)` past nregs —
   scratch lives in the adapter's +1 spare cell above the vararg staging area (M5b).
7. Sticky rt-staging after a pcall catch: `luaD_pcall` recovery consumes it (M5c).
8. `error()`-family prefixes: the where-flag is sticky per in-flight error —
   outer re-stagings never double-prefix (M5d).
9. `getfuncname` on wasm frames dereferenced `p->code` (NULL) — corrupted the
   error machinery so pcall-caught library errors returned `(false, nil)` (M5e).

## Reentrancy findings

- One wasmtime store handles nested wasm↔C↔Go dispatch to the full depth of
  the RTW guard (150) — the A2 fallbacks stay unused.
- wasm memory views must be re-read after any allocation (heap growth
  relocates the base) — the spike caches nothing.
- The main chunk runs without a CallInfo (the engine calls lua_main directly);
  every walk over the CI array must fall through to the line-stack bottom.

## Open (ledgered, with owners)

- **Row 10/18** table.sort + deep `__call`+tailcall chains corrupt the return
  path — the M5d/M6 error/EH investigation bucket.
- **Row 20** stack-overflow depth/unwind through 150 adapter levels doesn't
  reach pcall cleanly — M6 (with the arena lifecycle).
- **Row 22** coroutines — design scope (Asyncify/CPS study for the general build).
- **Row 23** debug introspection over wasm frames — M7 tracebacks (plumb the
  line-stack into getinfo).
- **Row 24** loadstring-mixed chunks — unsupported until M6 per the plan ruling.
- **Row 25** library-error wording — the M7 host module replaces these surfaces anyway.

## M8 perf observations

- 10⁶-deep flat tail recursion ≈ 0.7s wall (luawasm-run) — the trampoline's
  per-cycle cost (stage + 4 readbacks + restage + re-dispatch) is the floor;
  M8's restage-loop tightening (fold the readback into one call) is the first
  candidate, before any v2 control-flow work.
- The restage reuses the memory the stager restored — frame memory is O(1)
  for tail chains; the M6 arena must preserve that property.

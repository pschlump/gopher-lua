# P4 — call path: fast paths and devirtualization

**Date:** 2026-11-02 · tree post-`0fe4625` (P2) · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md` through
`Perf-P2-Table-Engine.md`).

## Where fib's cycles were (post-P2 profile, warm)

| symbol | flat | note |
|---|---|---|
| `doReturnTail` | 9.4% | staging + checks + 3 interface calls |
| `doReturn` | 2.9% | guard sequence before the tail |
| `fixedCallFrameStack.Push` | 5.3% | 13-word copy + **itab dispatch** |
| `Last` / `Pop` | 2.3% each | **itab dispatch** |
| `markerKind` / `resolveCall` | 2.3% each | calls that return immediately for a Lua callee |
| `closeTBCVM` | 1.8% | called per return to test an empty list |
| `hookCallEvent` + `hookReturnEvent` | 2.9% | called per call/return to test `hookMask == 0` |

Every line is per-call/per-return **generality** — the plan's thesis.
Unlike P1 (where OOO hid the predictable bookkeeping), these are real
calls with real marshalling on the critical path, and removing them
measured.

## What changed (three moves, all behavior-preserving)

1. **Fast-path call resolution** (`OP_CALL`, `OP_TAILCALL`): a Lua
   callee — the overwhelming shape — is never a marker and never a
   `__call` chain; that is exactly `markerKind`'s and `resolveCall`'s
   fast out, inlined at the top of both handlers
   (`callable, _ := lv.(*LFunction)`). A G callee skips `resolveCall`
   too (it is resolveCall's fast arm already); only a non-function
   callee takes the `__call`-chain call. The marker machinery and the
   raise order are byte-identical on the slow path.
2. **Return fast path + inline guards**: `doReturn` jumps straight to
   `doReturnTail` when `cf.Pcall == 0 && (tbclist == nil ||
   tbclist.idx < lbase)` — the exact negation of `closeTBCVM`'s loop
   condition, so the call is skipped when there is nothing to close.
   All five hot hook sites (`hookCallEvent` ×3, `hookReturnEvent` in
   `doReturnTail` and `callGFunction`, `hookVarargCallEvent` ×2) carry
   an inline `L.hookMask != 0` guard — the common no-hook posture
   (every bench posture, every Ultima EVAL) no longer pays the call.
3. **Frame-stack devirtualization**: `LState.stack` is an interface
   (`callFrameStack`) with two implementations — fixed (default) and
   auto-growing segmented (`MinimizeStackMemory`). The hot trio in
   `pushCallFrame` (IsFull/Push/Last) and `doReturnTail`
   (Pop/IsEmpty/Last) now type-assert once and run direct on the fixed
   stack (Push's body inlines at the call sites); the segmented path
   keeps the interface calls, unchanged. The growing path was verified
   live (coroutine + fib + pcall under `MinimizeStackMemory: true`) in
   addition to `state_test.go`'s coverage.

One Go trap caught by the goldens on the first build: in
`a, b, c := lv.(*LFunction), lv, 0` the assertion is the single-value
form and panics on mismatch (calls.lua's `__call`-table callee) — the
two-value `callable, _ :=` form is required.

## Results (3 interleaved reps per tree, all 44 cells)

    geomean   −5.32%

| lane | old (warm) | new (warm) | Δ | note |
|---|---|---|---|---|
| fib | 155.8m | 121.0m | **−22.3%** | all 4 postures −22…−24%; fib ≈ 6.1×C now (was 7.8×C) |
| callret | 20.59m | 17.26m | **−16.2%** | all postures −16…−17%; ≈ 7.0×C (was 8.4×C) |
| tco | 11.04m | 10.61m | **−3.9%** | tailcall keeps the window copy (stage not attempted) |
| hostfn | 546µ | 535µ | −2% | G calls still pay `markerKind` (measured flat — left) |
| str/strlong | | | −4…−5% | calls inside gmatch/string loops |
| numloop, nsieve, tablechurn, concat | | | flat | not call-bound |

Post-change profile (fib warm): the whole call-path apparatus is gone
from the top of the profile — `Push`/`Last`/`Pop`/`markerKind`/
`resolveCall`/`closeTBCVM`/hooks all below 1%; `doReturn` 1.3% flat.
What remains is `OP_CALL`'s flat 12.4% (the inlined frame copy + arg
window fill — real work), `condJump`, `registry.Get`, the arith family
(~24%, P6's boxing), and `opOrderI`.

## What P4 deliberately did NOT do

- **Frame representation** (`[]callFrame` → `[]*callFrame`) to kill the
  13-word `Push` copy: invasive (touches every frame consumer) and, per
  the P1 refutation's lesson about this hardware, likely a wash — the
  copy is now inlined and its stores are absorbed. The measured ladder
  says the next call-path win is not there.
- **`markerKind` for G callees** (hostfn lane): −2% at best, and the
  builtin identity compares are the cheapest correct check available.
- **`initCallFrame`'s nil-fill loops**: the window's cells must read
  nil before first write — a semantic safety net, O(NumUsedRegisters).

## Gates (all green)

Goldens byte-exact (58), full `./...`, `-race` host ×2 +
channel/context/golden-glua, CGO-free build, `MinimizeStackMemory`
live check. Behavior changes: none (no ledger rows needed).

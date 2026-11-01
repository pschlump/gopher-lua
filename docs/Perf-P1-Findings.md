# P1 — register windows: measured refutation

**Date:** 2026-11-01 · tree `7cf9f40` (post-P1a) · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md` and
`Perf-P1a-Deadline-Check.md`).

## What P1 promised

The plan's P1 (register-window slicing, est 10–25%) rested on the
2026-09 ranking's #2: "register bookkeeping" — the per-store
`checkSize`, the top-compare branch, the per-handler `currentFrame` /
`LocalBase` loads, and (in the full slicing design) the bounds checks
on every register access. Two experiments, each targeting one layer of
that mechanism, measured it on this hardware (Apple silicon, out-of-order
core). Both came back zero.

## Experiment 1 — remove the bookkeeping (stage A)

A complete implementation of the bookkeeping layer:

1. **cf threading**: `instFunc` handlers receive the loop's `cf`
   parameter (64 signatures); the per-handler `L.currentFrame` reload
   (62 sites) is deleted.
2. **Windowed stores**: `registry.setW`/`setIntW`/`setNumW` twins —
   identical to `Set`/`SetInteger`/`SetNumber` minus `checkSize`, which
   `initCallFrame`'s frame-entry guarantee (capacity through
   `base+max(nargs, NumUsedRegisters)`, including the tailcall reuse
   path) makes dead for every in-frame RA store. 57 audited in-window
   sites switched (OP_VARARG's multret table arm — the one write that
   can exceed the window — stays on checked `Set`).
3. **Block nil-fill**: `SetTop`/`initCallFrame` growing-fills via
   `copy()` from a prebuilt 512-slot LNil block.

All gates green (goldens byte-exact, full suites, race, CGO-free) —
behavior unchanged by construction. Benchstat A/B, 3 reps per tree,
concurrent binaries:

    geomean (all 44 cells)   −0.15%

Lane medians scattered ±3% in both directions (numloop/interp −5% was
the largest single cell, p=0.100 n=3 — noise). **Verdict: a wash.**
Reverted.

## Experiment 2 — the bounds-check ceiling (`-gcflags=all=-B`)

Stage A leaves exactly one per-access cost standing: the bounds check
on every `reg.array[i]` load/store. The full slicing design
(`*[256]LValue` per-frame window, masked 8-bit operands → provable
in-range → elided checks) could remove only that. `-B` removes ALL
bounds checks in the entire program — an overstatement of any BCE
design's ceiling. Interleaved same-session runs, fib warm (the most
register-intensive lane):

    with checks (stage A build)   157.7ms / 158.6ms
    -B (no checks anywhere)       157.0ms / 156.9ms

~0%. numloop/callret: within session noise (≤±4%, both directions
across runs). **Verdict: the slicing design's ceiling is zero.** Not
built.

## Why (the honest reading)

On this out-of-order core, every cost P1 targeted is either already
hidden (the predictable compare-and-branch of `checkSize`, the
`currentFrame` load — off the critical path, absorbed by OOO) or
already free (Go's masked-index + slice-len bounds checks compile to a
single compare the branch predictor never mispredicts). The 2026-09
estimate was formed on an in-order-shaped mental model that this
hardware does not match. P0's Finding 1 (the ctx select — a REAL
channel-readiness probe, a potential mispredicting call, on the true
critical path) was the exception; that one measured +63% and P1a took
it.

## Where the cycles actually are (post-P1a profiles, fib/numloop)

| cost center | evidence | owner |
|---|---|---|
| **LValue boxing in arithmetic** | numloop: `arithCompute`+`LInteger2I`+`LNumber2I` ≈ 12% of total samples, ~57% of engine time; every result past the [0,128) preload arena-allocates | new item — LValue representation (NaN-boxing / tagged union); the biggest possible change, worst ×C lane (numloop 14.6×C) |
| **Call-path sequence** | fib: `doReturn`+`doReturnTail` cum ~35%, frame `Push` 5%, `resolveCall`+`markerKind` ~2.5% — the per-call generality (marker/tbc/yield guards + struct-copy push + generic return tail) | P4, promoted by this finding |
| **tables** | tablechurn 2.7×C, nsieve 9.6×C; `RawSetString`/`Next` visible | P2 (unchanged, next) |
| loop fetch (3 dependent loads/instr) | small; loop flat 15% on fib incl. inlined bodies | — |

## Decision

P1 closed as **measured-refuted**; the register-window workstream
(window plumbing, BCE slicing, top-maintenance relocation) is dead on
this hardware and will not be built. The P-track order is now:
**P2 table engine → P4 call path (promoted) → P3 interning** — with
the LValue-representation item flagged as the largest open lever
(needs its own milestone + a decision, it touches everything).

Re-baseline discipline unchanged: every step benchstat A/B, negative
results recorded like this one — that is the rig working.

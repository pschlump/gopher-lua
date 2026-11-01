# P0 — measurement rig + baseline (luabench port)

**Date:** 2026-11-01 · tree at `docs/Option-A-Plan.md` post-N5 (engine
`3daee73`-line, working tree + this harness) · go1.27.0 · darwin/arm64
(M-series, GOMAXPROCS=14) · same machine as glasm's perfcmp record
(`../glasm/docs/Perf-vs-C-Lua.md`, tree `732fc8e`+`18bfcb4`) — the C
reference numbers below are that record's, cited, not re-run.

## The rig

`cmd/luabench`, ported from glasm's harness of the same name (same 11
scripts — the U7c seven + the U9 call-path lanes — same `benchIt`
cadence: 2 warmups, probe, then n iterations each preceded by
`runtime.GC()`, reporting min/median/mean; `-benchstat`, `-cpuprofile
DIR`, `-only SUBSTR`). Interp-only: the wasm/wazero legs are gone (this
repo IS the engine). Postures:

| posture | what it measures |
|---|---|
| `interp TOTAL (fork)` | fresh LState + KEYS/ARGV staging + DoString + Close — the engine's raw loop, no host machinery, **no context** |
| `cold: fresh VM/run` | `e.Run` with no VM cache — compile-once, fresh VM per run |
| `warm: pooled VM rerun` | one VM, `vm.Run` reruns — the daemon steady state, **`SetContext(rctx)` live** |
| `vmcache: engine cache` | `e.Run` against the `WithVMCacheSize` pool (primed) |

Output check: every script's interp-leg return values are compared to
the host leg's — all 11 OK in the recorded run.

## Baseline (median per run, recorded 2026-11-01)

| script | interp | cold | warm | vmcache |
|---|---|---|---|---|
| h_tiny | 38.9µs | 22.8µs | **1.8µs** | 2.2µs |
| h_str | 104.7µs | 97.9µs | 80.0µs | 81.3µs |
| h_strlong | 1.10ms | 1.29ms | 1.26ms | 1.26ms |
| h_numloop | 117.96ms | 183.95ms | **188.83ms** | 184.70ms |
| h_tablechurn | 9.47ms | 11.50ms | 11.52ms | 11.52ms |
| h_concat | 662.9µs | 786.4µs | 775.5µs | 776.6µs |
| h_hostfn | 401.4µs | 609.7µs | 597.6µs | 595.3µs |
| h_fib | 148.44ms | 219.17ms | **217.02ms** | 214.03ms |
| h_nsieve | 55.51ms | 85.66ms | 84.66ms | 85.09ms |
| h_callret | 19.89ms | 29.87ms | 29.29ms | 29.07ms |
| h_tco | 10.22ms | 17.58ms | **17.41ms** | 17.32ms |

Warm ≈ vmcache everywhere (the cache adds only lookup) — one production
posture to optimize. h_hostfn warm = 597.6µs / 3000 calls ≈ **0.20µs
per hostfn round trip** (consistent with the N3b embedcmp record's
~0.30µs).

## × native C Lua 5.5.1 (warm posture)

C reference: glasm perfcmp warm medians, same machine (their C harness:
state kept, chunk kept, staged pcall per run). Cross-harness comparison
— posture details differ (ours stages KEYS/ARGV + a live cancelable
context per run; theirs doesn't), so treat as calibration, not parity
claims:

| script | C (glasm) | ours warm | ours ×C | ours interp (ctx-free) ×C |
|---|---|---|---|---|
| h_tiny | 14.0µs | 1.8µs | 0.13×* | 2.8× |
| h_str | 36.0µs | 80.0µs | 2.2× | 2.9× |
| h_strlong | 387µs | 1.26ms | 3.3× | 2.8× |
| h_numloop | 8.52ms | 188.8ms | **22.2×** | 13.8× |
| h_tablechurn | 3.90ms | 11.52ms | 3.0× | 2.4× |
| h_concat | 620µs | 775.5µs | 1.25× | 1.07× |
| h_fib | 20.23ms | 217.0ms | **10.7×** | 7.3× |
| h_nsieve | 5.98ms | 84.7ms | **14.2×** | 9.3× |
| h_callret | 2.41ms | 29.3ms | **12.2×** | 8.3× |
| h_tco | 1.62ms | 17.4ms | **10.7×** | 6.3× |

\* pooled rerun skips parse + state build; the C harness's warm leg pays
its own per-run staging. The embedcmp suite (Embedded-EVAL-Perf.md)
owns the C-API-embedded comparison.

Our interp legs land where glasm's interp did (sanity: same lineage —
e.g. numloop 118 vs 130ms, fib 148 vs 154ms across trees/harnesses).

## Finding 1 — the per-instruction context check is the #1 measured cost in the production posture

Every compute lane's warm leg is 1.3–1.7× its own interp leg (numloop
189 vs 118ms, tco 17.4 vs 10.2ms, callret 29.3 vs 19.9ms) on the same
compiled script. The delta is `mainLoopWithContext`'s per-instruction
non-blocking `select { case <-L.ctx.Done(): ... default: ... }` plus the
`memLimit` probe (`_vm.go` hot loop) — 2026-09 ranking line #9, "misc",
actually the top measured cost.

Isolated A/B (one LState, one script, only `SetContext` differing;
h_numloop shape, best of 3):

| context | time |
|---|---|
| none (`mainLoop`) | 114.5ms |
| `context.Background()` (nil Done channel) | 148.7ms (+30%) |
| real cancelable context (`WithCancel`, open channel) | **186.8ms (+63%)** |

A nil-channel select is cheap (never-ready short-circuit); a real open
channel — which is what every production run has — costs the full
channel-readiness probe per instruction, ~5ns × ~14M instructions here.
This exactly reproduces luabench's interp→warm gap (118→189ms).

**Implication:** a cheaper deadline mechanism (check on back-edges +
calls, or every Nth instruction — infinite loops are backward jumps or
unbounded recursion, both still hit a check within µs of firing) is
worth more on every loop lane than the whole P1 register-window
estimate (10–25%), and it is days, not weeks. The deadline contract
(error shape, kill latency) is preserved; only the raise's instruction
granularity changes, which nothing gates byte-exactly.

## Finding 2 — where the engine's own cycles go (clean lanes)

pprof per-(script, posture) via `-cpuprofile`; short lanes (tiny, str,
tablechurn, hostfn) are dominated by benchIt's inter-iteration
`runtime.GC()` in the sampling window (engine frames 2–6%) — a rig
limitation, noted; the long lanes are clean:

- **h_fib warm** (engine frames): `mainLoopWithContext` 46% flat (the
  go-inline'd opcode bodies live there), then the call path —
  `doReturnTail` 10.6%, `condJump` 6.1%, `opArith` 4.4%,
  `(*registry).Get` 3.9%, `(*fixedCallFrameStack).Last` 1.7%,
  `doReturn` 13.3% cum.
- **h_callret warm**: same shape (loop 13% flat + `LInteger2I`,
  `doReturnTail`, `opArith`, `fixedCallFrameStack.Pop`).

**2026-09 ranking, revalidated on the current tree:**

| 2026-09 rank | then claim | now |
|---|---|---|
| #9 ctx select / misc | "per-instruction select" | **#1 measured, production posture: +30–63% by lane (Finding 1)** |
| #2 register bookkeeping | 10–25% est | confirmed present (fib/callret: Get/Last/Pop, top-maintenance in handlers) — P1's target, second priority |
| #6 call overhead | push frame, nil-fill, vararg | confirmed — `doReturnTail`/`doReturn` cum dominate fib after the loop itself |
| #1 LValue interface | asserts + re-box per op | confirmed visible (`arithCompute`, `LInteger2I` flat) but behind #9/#2 on these lanes |
| #3 tables (5 collections, rehash) | est 1.5–3× on hash lanes | visible on tablechurn/nsieve (`RawSetString`, `Next`) but the loop tax inflates those lanes first |
| #5 interning, #4 metamethod-on-miss | — | not separately visible in these profiles; unchanged prior |

## Recommended P-track order after this record

1. **P1a (new): amortized deadline check** — back-edges + calls (or
   Nth-instruction); days; expected to reclaim most of the 1.3–1.7×
   warm/interp gap on loop lanes (numloop 22.2×C → ~14×C).
2. **P1: register windows** (unchanged; fib/callret evidence above).
3. **P2: table engine** (unchanged).
4. P3 interning (unchanged).

Re-baseline with this file's table after each step; benchstat A/B is
the merge gate (plan §5 law).

# P1a — amortized deadline check

**Date:** 2026-11-01 · tree `bc31b4f` + this change · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md`, which is
the "before" record — its Finding 1 is this change's motivation).

## The change

`mainLoopWithContext`'s ctx/budget probe ran before **every**
instruction: a non-blocking `select` on the context's Done channel. On a
real cancelable context (every production run) that channel is open, and
the probe measured +63% on h_numloop and 1.3–1.7× the ctx-free loop on
every compute lane (P0 Finding 1) — the single largest
production-posture cost.

P1a amortizes it to every **64** instructions (`deadlineCheckEvery`) via
a countdown in the hot loop, and adds one compensating probe at the seam
the stride could otherwise strand:

- **loop probe** (`_vm.go` mainLoopWithContext): countdown; on expiry,
  re-arm to 64 and run the existing select + `memLimit` check unchanged.
- **G-return probe** (`_vm.go` callGFunction, immediately after
  `frame.Fn.GFunction(L)` returns ≥ 0): same select, same raise shape.

The G-return probe exists because a G function that **blocks** observes
cancellation *inside* itself and can return **normally** instead of
raising — channellib's `ch:receive()`/`channel.select` race
`L.ctx.Done()` inside `reflect.Select` and come back `(false, nil)`.
Under the per-instruction probe the very next dispatch converted that
into the "context canceled" raise; under a bare stride-64 countdown a
script with < 64 instructions left would have exited successfully
(TestCancelChannelReceive/2 caught exactly this). Go-call returns are
the one boundary where cancellation gets observed while blocked, the
per-call probe is noise against call cost (a ~2 ns select against the
~175 ns h_hostfn round trip), and it restores the old semantics
exactly: the old loop probed at the first instruction after every
G-call return too. Suspended calls (yield, `gfnret < 0`) skip the probe
— their resume bookkeeping must complete, and the parent loop probes on
its own cadence.

## The contract, unchanged

- Error shape: both probes raise `L.ctx.Err().Error()` bare — the
  N5 daemon normalization (host mapError → `ScriptError{ctx text,
  Line 0}`) applies identically.
- Kill/deadline latency: any unbounded execution — a loop (backward
  jump) or unbounded recursion — executes instructions continuously, so
  a crossed deadline still raises within 64 instruction boundaries; the
  blocked case raises at the G-return boundary, as before. Nothing
  gates the raise's instruction granularity byte-exactly.
- The ctx-free `mainLoop` is untouched (ctx-less states never probe).
- Gates: `go test .` (full suite incl. the golden corpus — byte-exact,
  zero golden diffs), `go test ./...`, `go test -race ./host/
  -count=2`, race pass on the channel/context tests. All green.

## Results (median per run, luabench)

Warm posture (the daemon steady state), baseline → P1a:

| script | warm before | warm after | Δ | ×C before → after |
|---|---|---|---|---|
| h_tiny | 1.8µs | 1.8µs | — | 0.13× → 0.13× |
| h_str | 80.0µs | 66.8µs | **−16.5%** | 2.2× → 1.9× |
| h_strlong | 1.26ms | 1.08ms | **−14.3%** | 3.3× → 2.8× |
| h_numloop | 188.83ms | 124.41ms | **−34.1%** | **22.2× → 14.6×** |
| h_tablechurn | 11.52ms | 9.67ms | **−16.1%** | 3.0× → 2.5× |
| h_concat | 775.5µs | 636.1µs | **−18.0%** | 1.25× → 1.03× |
| h_hostfn | 597.6µs | 526.1µs | **−12.0%** | 0.20µs → 0.18µs/call |
| h_fib | 217.02ms | 156.99ms | **−27.7%** | **10.7× → 7.8×** |
| h_nsieve | 84.66ms | 57.60ms | **−32.0%** | **14.2× → 9.6×** |
| h_callret | 29.29ms | 20.14ms | **−31.2%** | **12.2× → 8.4×** |
| h_tco | 17.41ms | 10.61ms | **−39.1%** | **10.7× → 6.5×** |

Cold legs drop the same (they pay the same probe): numloop 183.95 →
125.22ms, fib 219.17 → 156.02ms, tco 17.58 → 10.66ms, nsieve 85.66 →
58.37ms, callret 29.87 → 20.32ms.

**The warm/interp gap is gone.** P0's Finding 1 gap (warm 1.3–1.7× the
ctx-free interp leg) closed to ≈2% everywhere — e.g. numloop warm
124.4ms vs interp 121.8ms (was 188.8 vs 118.0). The interp legs
themselves are untouched code and move only within noise. The plan's
target for this step ("numloop 22.2×C → ~14×C") landed at 14.6×C.

Benchstat A/B — 3 reps per tree, baseline binary built from `bc31b4f`
(detached worktree), both run concurrently (equal contention; ratios
agree with the solo-run medians above). `~` marks only the n=3 sample
count (benchstat wants ≥4 for significance); the deltas are large and
consistent across all four postures:

    Luabench/h_numloop/warm-14     204.8m   131.8m   −35.6%
    Luabench/h_tco/warm-14         18.29m   11.01m   −39.8%
    Luabench/h_callret/warm-14     30.49m   21.07m   −30.9%
    Luabench/h_fib/warm-14         224.9m   158.2m   −29.7%
    Luabench/h_nsieve/warm-14      91.59m   60.61m   −33.8%
    Luabench/h_tablechurn/warm-14  12.68m   10.44m   −17.7%
    Luabench/h_concat/warm-14      884.5µ   716.5µ   −19.0%
    Luabench/h_hostfn/warm-14      682.2µ   589.8µ   −13.5%
    Luabench/h_str/warm-14         81.46µ   65.64µ   −19.4%
    Luabench/h_strlong/warm-14     1.445m   1.207m   −16.5%
    Luabench/h_tiny/warm-14        1.985µ   1.897µ    −4.4%  (setup-dominated)
    geomean (all 44 cells)                          −18.49%

The interp legs (untouched code, ctx-less) are flat — numloop
131.1→130.8m, fib 154.9→156.1m, nsieve 59.19→59.03m, callret
21.02→21.05m, hostfn 450.6→455.8µ — no regression from the G-return
probe's nil-ctx short-circuit on the ctx-free path.

## Next

Per P0's revised order: **P1 register windows** is now the top measured
cost center (fib/callret: `doReturnTail`/`doReturn`, `registry.Get`,
`fixedCallFrameStack.Last/Pop` — the ctx tax no longer masks them).

# P5 — misc: the concat fold

**Date:** 2026-11-02 · tree post-`b084b6e` (P3) · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md` through
`Perf-P3-String-Constants.md`).

## The plan's two items, one already gone

- **`LNumber.String` fmt.Sprint path — stale, resolved by history.**
  `LNumber.String` already routes to `numberToStringStock`
  (formatlib.go: the stock `tostringbuffFloat` %.15g / re-read / %.17g /
  ".0" port, hand-built on `strconv.FormatFloat` — a C-exact %g, since
  Go's `fmt` %g is not byte-identical to C's). The only `fmt.Sprint`
  left on the paths is cold (error-object stringification in the
  dying-thread arm, and `keyStr` in argument errors). Nothing to do.
- **`stringConcat`'s `[]string`+join — done.** The fold built a
  `make([]string, total+1)` slice, materialized a temp Go string per
  operand (`strconv.FormatInt` allocates; floats allocate 2–3×), then
  `strings.Join` made one more pass and the final allocation. For the
  dominant 2-piece integer shape (`"key:" .. i`, `"k" .. i`) that is
  three allocations and two copies per concat.

## What changed (`_vm.go` `stringConcat`, behavior-identical)

A type-check pass finds the convertible run's extent — leaving
`i`/`total`/`lhs` exactly where the old inner loop left them, so the
outer `__concat`/error continuation is untouched — while sizing the
buffer (exact for strings, 24-byte ceiling per number). One ascending
`strings.Builder` pass then appends the pieces: strings free,
integers via `strconv.AppendInt` into a 20-byte stack scratch (no temp
string at all), floats through the stock spelling. One allocation
(the result), zero temps, no join pass. `memCharge(len+24)` on the
result unchanged; the transient-slice garbage the old path produced
was never charged, and the builder's is no more observable.

One real bug caught by the goldens on the first build: the pieces
live at `i+1 … start` (the check pass walks `i` *down* past them),
not `[start, last)` — the wrong bounds dropped the first piece of a
3-operand run (`"hello!" .. " " .. 21` → `" 21"`, env02.lua).

## Results (6 interleaved reps vs the P3 tree, all 44 cells)

| lane | Δ warm | Δ interp | mechanism |
|---|---|---|---|
| concat | **−7.7%** (p=0.002) | −7.5% (p=0.002) | the target lane |
| hostfn | **−6.6%** (p=0.002) | −9.8% (p=0.002) | `bench.call("key:"..i)` ×3000 — the Redis-call shape |
| tablechurn | −1.8% (p=0.004) | −4.5% (p=0.002) | `"k"..i` stores/reads ×30k; also recovers P3's layout tax here |
| tco interp | | −2.1% (p=0.026) | favorable layout dice (no concat in the lane) |
| fib / callret / str / strlong / nsieve / numloop / tiny | flat | | no concat in the hot path |
| **geomean** | | | **−1.80%** |

Cumulative vs `d195bd2` (pre-P3): geomean −2.53%, hostfn −11.4% warm.
A pleasing shape for Ultima: the two biggest wins land on the
redis-call and key-churn postures, which is what EVAL scripts do.

## Gates (all green)

Goldens byte-exact (58, 32.1s uncached — the env02.lua drop above was
caught here), full `./...`, `-race` host/channel/context, CGO-free.

## P-track after P5

P6 (LValue representation — NaN-boxing/tagged union) is the only
milestone left and the biggest lever by measurement (numloop 14.6×C,
~57% of that lane's engine time in boxed-arith allocation). It needs
a decision and its own milestone; everything cheap is now taken.

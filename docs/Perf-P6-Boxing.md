# P6 — boxing-cost engineering (and why the representation didn't change)

**Date:** 2026-11-02 · tree post-`7fd95a2` (P5) · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md` through
`Perf-P5-Misc.md`).

## The decision this milestone opened with

P1's findings ranked **LValue representation** the biggest open lever
(numloop 14.6×C, ~57% of that lane's engine time in boxed-arith
allocation) and explicitly deferred it pending a decision. The
candidates were NaN-boxing (48-bit payload in a float64) or a tagged
union. **Both are rejected — not on perf grounds but on the
embeddability law:**

- `LValue` is the exported API — `interface{}`. Ultima's bridge,
  `host/value`'s converters, and every consumer pattern-match on it
  (`lv.(*LFunction)`, `v.(LInteger)`). Changing its kind rewrites every
  consumer signature; this engine exists to be embedded, and the
  interface *is* the product boundary.
- A NaN-box hides pointers inside float64 bits. Go's GC would never
  find them — every boxed reference would need a shadow interface kept
  alive elsewhere, which reinstates the boxing we were trying to remove
  and adds unsafety to the hot path.

With the representation fixed, the remaining lawful work is **boxing
cost**: how many boxes an arithmetic op pays, and whether each box
allocates at all.

## What boxing actually costs (measured, escape-proofed)

The naive micro-benchmark (`_ = LValue(x)`) reports 0 allocs and
sub-nanosecond times — dead-code elimination eats it. With a
package-level sink:

| strategy | ns/op | B/op | mechanism |
|---|---|---|---|
| plain conversion `LValue(int64)` | 5.1 | 8 | `convT64` → `mallocgc` (runtime caches only [0,256) by raw bits) |
| arena `allocator.LInteger2I` | 3.0 (amortized) | ~0 hot | bulk `[]int64` block, scratch-interface word swap |
| preload cache hit | 0.22 | 0 | static array slot |

And a semantics check that had to pass before any of this was lawful:
Go interface `==` compares **dynamic values** for int64/float64 —
verified at runtime with computed values and cross-strategy compares
(arena-boxed `LInteger(700)` == plain `LInteger(700)`). Table keys and
`rawequal` are value-based in this engine; no code path relies on
pointer identity for numbers, so which box a value lives in is
unobservable. Pinned in `boxing_p6_test.go`.

## The double-box (the actual bug-shaped win)

`opArith`/`opArithK` called `arithCompute`, which returned a
**plain-converted** `LValue` — the 5.1ns/8B `convT64` box — and then
the handlers immediately did `v.(LInteger)` / `v.(LNumber)` and
re-boxed the payload through the registry's allocator
(`reg.SetInteger`/`SetNumber` → arena). Every fast-path arith result
paid for two boxes and kept neither. `arithCompute` now returns
unboxed `(iv LInteger, fv LNumber, isInt bool, err string, res
arithOutcome)`; the handlers box exactly once, into the arena. The
cold callers adapted in place: `objectArith` (MMBIN slow path) boxes
once via the allocator; `strArith` pushes the typed payload; the
compile-time `constFold` keeps its exact policy (float NaN/±0 never
folded, integer zero folds).

## intCache: the negatives were never cached

The allocator's float preloads cover [0,128). For integers, plain
conversion's runtime cache indexes by raw bits, so only [0,256) ever
hit — **every negative integer result and every result ≥256
allocated**, and loop counters walk straight through that gap. `intCache`
is a shared static `[2048]LValue` covering **[-1024, 1024)**: 32KB per
process, zero per-hit cost, and `LInteger2I` consults it before the
arena. (The float preload path keeps its `Signbit` guard — -0.0 must
never ride a cached +0.0, row 42.)

Deliberately *not* widened further: beyond a few thousand the static
table would grow RSS for every hosting process for diminishing
traffic, and the arena path (3.0ns amortized) is already cheap.

## Results (6 interleaved reps vs the P5 tree `7fd95a2`, all 44 cells)

| lane | Δ | p | mechanism |
|---|---|---|---|
| numloop | **−21.3 … −23.2%** (all 4 postures) | 0.002 | the target lane: every add paid convT64 **plus** the arena re-box — now one box. The profile's mallocgc family (~12%) + scanObject (~4%) mostly vanishes with the 8B/op garbage |
| callret | **−12.8 … −13.6%** | 0.002 | loop counters: intCache hits **and** single-boxing |
| tco | **−12.6 … −14.6%** | 0.002 | same shape |
| tablechurn | −6.7 … −7.7% | 0.002 | `"k"..i` keys — counter arith on top of P2/P5's wins |
| concat | −4.6 … −5.9% | ≤0.015 | counter arith feeding the P5 fold |
| hostfn | −4.8 … −7.3% | ≤0.026 | the redis-call shape, again |
| str / strlong | −2.1 … −6.6% | ≤0.026 | loop counters in the string lanes |
| fib / nsieve / h_tiny | flat | — | fib's arith is a minor share (P4 took the calls); nsieve's values exceed the cache and its cost is table-bound |
| **geomean** | | | **−7.41%** |

Warm numloop: 126.2ms → 99.3ms; by the P0 C calibration that is
14.6×C → **≈11.5×C**. Across the whole P-track, each milestone
measured against its predecessor tree, the compounding geomean is
**≈ −35%** vs the pre-P1a baseline (−18.49% P1a, −6.73% P2, −5.32%
P4, −0.54% P3, −1.80% P5, −7.41% here). Against the P0 record's
same-machine C calibration: numloop warm 22.2×C → **11.7×C**
(188.8ms → 99.3ms), tablechurn 3.0×C → **1.9×C** (11.52 → 7.39ms),
hostfn 0.20 → **0.15µs per round trip** — the Redis-shaped lanes are
the program's compounding beneficiaries.

## Gates (all green)

New durable gate `boxing_p6_test.go`: cross-strategy interface equality
(cache edges ±1023/±1024, computed values, arena fallback), the -0.0
Signbit guard, `arithCompute` unboxed contract across every opcode
class (subtype fidelity, error messages), and the constFold zero
policy. Goldens byte-exact (58, uncached), full `./...`, `-race`
host/channel/context battery, CGO-free build.

## P-track status

P6 was the last milestone. The plan's perf program is complete; the
compounding is in the results table above. What remains open is not a
milestone but an ambient law from P3: any `_vm.go` perturbation pays a
~1–2% no-op layout floor, so future micro-tuning there must clear that
bar with targeted wins or not be done.

# P2 — table engine: the unified hash part

**Date:** 2026-11-01 · tree post-`4958ae7` · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md`,
`Perf-P1a-Deadline-Check.md`, `Perf-P1-Findings.md`).

## What changed

The hash part of every `LTable` was four data structures —
`strdict map[string]LValue`, `dict map[LValue]LValue`, `keys []LValue`
and `k2i map[LValue]int` — written through double bookkeeping on every
insert (map store + k2i store + keys append) and walked with a map
lookup per step in `Next`. It is now one open-addressed `[]hnode`
array (power-of-two, ≤ 3/4 load, linear probe) at 40 bytes per slot:

- **Order rides intrusive links, not slots.** Each node carries `ins`,
  the next slot in first-insertion order, with `first`/`tail` heads.
  A slot is storage; iteration order is the chain. Rehashing re-slots
  every node but rebuilds the chain in order, so growth at any size
  never reorders iteration.
- **Deletion is a tombstone** — the value becomes `LNil`, the key and
  the link stay. `next()` skips tombstones, and a deleted-then-
  reinserted key lands back in its **original** iteration position,
  which is the behavior the old stale-k2i quirk produced and the
  order probe pins (`order2: z=1,a=2,m=9,q=4`). Tombstones are never
  slot-reused for a different key, and never unlinked — the same
  unbounded `keys[]` the old code carried (its TODO said so), but at
  zero extra memory.
- **One insertion sequence for all key kinds** — strings, numbers,
  everything — exactly what `keys`/`k2i` maintained, now without the
  side vector.
- **Hashes** are `hash/maphash` for strings and splitmix64-mixed words
  for the rest, salted per kind, consistent with `LValue` equality
  (callers normalize numeric spellings first; `-0.0` folds to `+0.0`).
  A nil interface key marks a never-used slot; the empty-check is one
  word compare. The hash is cached in the node, so a rehash never
  re-hashes a string, and it pre-filters the interface compare while
  probing.
- **`RawGetString`** (the OP_GETFIELD/GETTABUP hot path) is a bare
  probe loop: hash, mask, string compare — no query boxing, no method
  layer.
- **`gc.go`** `markTable`/`clear` walk the chain (live nodes only —
  like the old map walks, a deleted key pins nothing); weak clearing
  still routes through `RawSetH`/`RawSetString`, which now tombstones.

Materialization is lazy: a table pays nothing until its first hash
write, then 8 slots (320 bytes — the old `strdict` preallocation was a
Go map with room for 32).

## The charge model is byte-identical (the contract)

`gcCharge` at creation is untouched (`tableCharge(acap, hcap)`), so
`collectgarbage("count")` deltas — sort.lua's memdiff battery — are
unchanged. The per-VM budget seams are mirrored exactly:

| old event | new mirror |
|---|---|
| first string write ever (`strdict == nil`) → +`defaultHashCap`·48 | `strdCharged` one-shot, +32·48 |
| first non-string hash write ever (`dict == nil`) → +`len(strdict)`·48+64 | `dictCharged` one-shot, +`strKeys`·48+64 (`strKeys` = live string keys, the old `len(strdict)`) |
| new key (`k2i` miss, non-nil value) → +48 | probe-miss insert → +48 (tombstone reuse charges nothing, like a `k2i` hit) |
| delete → no charge, no refund | tombstone → no charge |
| birth with `hcap != 0` (strdict pre-made; the seam never fires later — OP_VARARG's table packs) | `strdCharged` set at birth, nodes stay lazy |

Both one-shot seams fire even for nil-value (delete) writes, as the
old `make()` calls did.

## Semantics: what pins it

- **`/tmp/p2/order.lua` probe** (kept in the session record; regenerate
  from the transcript if needed): byte-identical before/after —
  first-insertion order, tombstone re-insert at original position,
  delete-then-new-key appending, positive-integers-always-array
  (nil-fill + append), float normalization, negative/maxinteger keys
  in hash, next() skipping deletes, 200-key growth preserving order.
- **Golden corpus**: 58 cases byte-exact, official leg included
  (weak tables, next order, sort, gc-count batteries).
- **New durable gate**: `table_p2_test.go` — 30,000 random
  set/delete/re-insert steps against a first-insertion-order reference
  model checked through the public boundary (full `Next` walk +
  point reads every 97 steps), across sizes well past every doubling.
  (Its first draft hung: `RawSetH(LInteger(3))` parks a small positive
  integer in the hash part, where `next(3)`'s array→hash jump
  re-yields key 3 forever. The old engine loops identically on that
  misuse; it is unreachable from Lua, where `RawSet`'s array arm and
  the `table index is nil/NaN` raises screen it.)

Divergence ledger: **no Lua-observable change** (hence no new row).
The two Go-API-only corners: `ForEach`'s hash order is now
deterministic first-insertion (was two randomized map ranges — nothing
could pin that), and Go-side `RawSetH(LNil, v)` still stores (the nil
*interface* is the empty sentinel, `LNil` the singleton is not).

## Plan items (i)–(iv), honestly

- **(i) intrusive insertion-order links** — done, as above.
- **(ii) precomputed string-key hashes** — the node's cached hash
  covers the rehash case; per-op hashing is `maphash.String` (≈1ns/
  16B, hardware-accelerated). Full precompute is P3 interning's to do
  (an interned string can carry its hash).
- **(iii) ≤8-slot linear fast path** — implemented as the 8-slot
  initial table (one mask + a probe that nearly always ends on the
  first slot at ≤ 3/4 load). A separate linear-scan mode was not
  built: against one masked probe, a ≤8 interface compare scan is not
  a win worth a second representation.
- **(iv) inline the no-metatable check** — not needed: `metaOp1`
  already fast-outs through `metatable()`'s type switch, and the lanes
  that would show it (fib/callret) measured flat.

## Benchstat A/B (3 reps per tree, interleaved, full 44 cells)

    geomean   −6.73%

| lane | old (warm) | new (warm) | Δ | note |
|---|---|---|---|---|
| tablechurn | 10.03m | 8.25m | **−17.8%** | all 4 postures −18…−20% |
| concat | 674µ | 540µ | **−19.9%** | buffer tables |
| h_tiny | 1.82µ | 1.78µ | ~ | interp/cold −37/−39% (startup tables) |
| h_str | 67.2µ | 65.5µ | −2.5% | interp −13%, cold −11% |
| nsieve | 59.7m | 58.5m | ~−2% | array-heavy |
| numloop, fib, callret, tco, strlong, hostfn | | | flat | not table-bound (P6/P4 own those) |

The confirmation run after the charge fix (`-only tablechurn`):
9.63–9.69m → 7.72–7.98m. n=3 shows `~` per cell as always; the deltas
hold across all four postures, which is the evidence the rig gives at
this n.

## Gates (all green)

Goldens byte-exact (58), full `./...`, `-race` host ×2 +
channel/context, CGO-free build, order probe identical, churn-vs-model
gate, charge seams audited against every `newLTable*` call site.

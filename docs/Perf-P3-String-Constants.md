# P3 — string constants: per-proto hash side table

**Date:** 2026-11-02 · tree post-`d195bd2` (P4) · go1.27.0 ·
darwin/arm64 (same machine/session as `Perf-P0-Baseline.md` through
`Perf-P4-Call-Path.md`).

## The plan's premise, measured away

P3 as planned ("intern at compile time and table-key store; per-VM
pool; pointer-equality compares; est 5–15%") was ranked against the
pre-P2 engine, where a string-key lookup walked Go maps with interface
boxing on every step. Post-P2 the whole probe is one open-addressed
slot visit — measured directly:

| op (short key, "format") | ns |
|---|---|
| `maphash.String` of the key | 3.67 |
| full `RawGetString` (incl. that hash) | 5.52 |

The hash is two thirds of a lookup that is already ~5ns. Eliminating
it can only matter where the *same string* is looked up repeatedly —
which is exactly the constant-key ops (`GETFIELD`/`GETTABUP`/`SELF`/
`SETFIELD`/`SETTABUP`: every `string.rep`, every `t.x`, every global).
The plan's remaining parts measured as non-wins: tablechurn's keys are
freshly concatenated per store (`t["k"..i]`), so a store-side intern
pool would *add* a map op per store; and pointer-equality compares
attack only the ~2ns short-memcmp that follows a hash match. The pool
was dropped; the hash elimination was built.

## What changed

- **`FunctionProto.strConstHash []uint32`** — parallel to `Constants`,
  holding `uint32(maphash.String(mhseed, s))` for every string constant
  (0 for non-strings; strings hashing to 0 are re-hashed inside —
  correctness holds, the fast path just doesn't trigger). Filled at
  compile finalize (compile.go, beside the existing `stringConstants`
  walk) and at undump (dump.go — maphash's seed is per-process, so a
  hash carried in a dump would silently probe wrong slots; it is always
  derived at load). A hand-built proto leaves it nil and the VM falls
  back to hashing per access.
- **The five constant-key handlers** fetch the hash once and pass it
  down; `getFieldString`/`setFieldString` grew an `h uint32` param
  (0 = compute inside), and `RawGetString` delegates to a new
  `rawGetStringH(key, h)` so the VM path never re-hashes.

Byte-exactness: no behavior change of any kind — same slots, same
probe sequence (the hash enters the identical computation), same
metamethod order. No ledger rows.

## Results (6 interleaved reps per tree, all 44 cells)

| lane | Δ warm | Δ interp | note |
|---|---|---|---|
| str | **−4.4%** (p=0.002) | −2.2% (p=0.004) | all 4 postures negative |
| strlong | **−3.4%** (p=0.002) | −2.4% (p=0.041) | all 4 postures negative |
| hostfn | **−4.2%** (p=0.004) | −6.9% (p=0.002) | all 4 postures negative |
| fib / callret / concat / tiny | flat | | untouched lanes, flat |
| tablechurn | +1.1% | +2.1% | see below — layout, not the change |
| tco / nsieve / numloop(interp) | +1–3% | | same band |
| **geomean** | | | **−0.54%** |

## The null experiment (the milestone's real finding)

The non-target regressions were stable across three different builds —
too stable for layout churn — so a null was run: **the same code with
the side-table fill disabled** (behavior byte-identical to pre-P3,
`h` always 0). Result: tablechurn **+2.11%**, fib +1.14%, geomean
**+1.20%** — the plumbing alone, changing nothing, pays a ~1–2% tax on
several lanes. That is the no-op cost of *any* perturbation to the
generated dispatch code (address/alignment shifts in the handler
table), not a cost of this change: enabling the fill buys back the
whole tax and lands the targeted lanes 4–5% under baseline.

Consequences for the program: (i) sub-2% "regressions" on untouched
lanes after any `_vm.go` edit are the machine's floor — don't chase
them and don't budget against them; (ii) a milestone must clear that
floor by its targeted wins, which this one does where it aims and not
elsewhere.

## What P3 deliberately did NOT do

- **Inline direct-slot probes in the three GET handlers** (a hit would
  skip the `getFieldString` call entirely): built and measured — str
  −5.2% vs −3.1% at stage A, but the extra ~30 lines of closure code
  pushed numloop +2.7% (p=0.004) and tablechurn +1.6%; the marginal
  ~1.4% on str was a wash against the floor. Reverted.
- **The per-VM intern pool** (shared backing → pointer-equality
  compares): measured premise gone (above); also an unbounded
  script-keyed pool is a sandbox liability under `membudget`.
- **SET-side hash plumbing into `hashInsert`**: writes re-hash once
  per store; rarer than reads, not worth the extra `RawSetString`
  surface.

## Gates (all green)

New `consthash_p3_test.go` (side table matches engine hashing across
nested protos; undump fills it; h==0 fallback resolves — including a
nil-side-table proto executed through the VM), goldens byte-exact (58,
full 31.9s uncached run), full `./...`, `-race` host/channel/context,
CGO-free build. Behavior changes: none.

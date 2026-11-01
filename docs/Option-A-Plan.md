# Option A — gopher-lua as the standalone Lua 5.5.1 embed engine

**Status:** decision made and carve executed 2026-11-01. This repo (branch
`v5.5`) is the pure-Go Lua 5.5.1 virtual machine carved out of **glasm @
`0affa95e2177337baa3703fff449dd8738564aea`**. Glasm is preserved untouched as
its own project — a Lua→wasm compiler, useful for computationally intensive
standalone Lua — and remains the frozen source of this carve. This document
is the roadmap for everything that happens here.

---

## 1. The decision

WASM was ruled out as the embedded scripting posture for Ultima (the pure-Go
Redis clone): per-VM memory cost (~52 MB + a 256 MiB mapping vs ~8–13 MB for
the interpreter), the value-wire tax on every host call (`redis.call`), the
frozen-GC/watchdog complexity class (glasm ledger rows 93, 99), and blob/JIT
lifecycle management in a daemon. Two paths were assessed for obtaining the
Go-native 5.5.1 engine Ultima needs:

- **Option A — carve the existing Go VM out of glasm.** The Go VM never left
  glasm: the root package *is* the gopher-lua interpreter upgraded in place
  to stock 5.5.1 (the U1–U5 program), kept byte-exact against the C oracle
  by every gate, and used as the primary oracle for the wasm backend. The
  work is carve + wrap + sandbox, not a port.
- **Option B — fresh fork of yuin/gopher-lua, re-apply the 5.5.1 changes.**
  This re-executes the U1–U5 program (2026-09-19 → 2026-10-07 of intense,
  verification-driven work, 105 ledger rows of found-and-fixed divergences)
  on a skeleton whose every untouched corner still carries 5.1-era
  assumptions. Its endpoint is exactly Option A's starting point, with
  re-introduction risk A does not have and nothing to sync against
  (upstream is dormant).

**Option A won decisively.** Estimated 1–2 weeks to a production embed
engine vs 3–6 weeks to arrive at the same code with less confidence.

### The verification this tree inherits

The interpreter carved into this repo is not "probably conformant" — it is
the most-verified component of the glasm program:

- **Official 5.5.1 suite, interp ↔ stock C Lua byte-exact, ≥95%** with
  vararg/locals/goto/code/calls/errors/coroutine/db green outright (glasm's
  `TestU5OfficialSuite`, corpus `_lua5.5-tests/` — copied here).
- The 1501-case opcode matrix (`_wasm-tests`, generated; regenerable from
  glasm if needed) with the interp column.
- `_u1` (5.2-core cases, copied here), `_u5`, `_u9` differential gates.
- The U7d counting fuzz soak: 2.57M cases / 10.3M execs across the
  4-engine set (interp included), zero findings.

Cross-checks against stock C Lua 5.5.1 remain available ad hoc through
glasm's `testdiff/` harness (its corpora are identical to the copies here).

## 2. The carve (done, 2026-11-01)

**Came over:** the root interpreter package (36 files, ~32.6k lines incl.
generated `state.go`/`vm.go`), `ast/`, `parse/`, `pm/`, `_tools/go-inline`,
corpora `_lua5.5-tests/` + `_glua-tests/` + `_u1/`, `cmd/glua` (adapted:
interp-only, no testdiff/wasm flags), `LICENSE`, docs (below), the `.claude`
model setup, `Makefile`/`go.mod` rewritten for
`github.com/pschlump/gopher-lua` (root package name stays `lua`, as
upstream).

**Stayed in glasm:** `wasm/`, `luawasm/`, `runtime/` (the C tree),
`testdiff/`, `host/` (the wazero-based embed package), the wasm/corpus-gate
cmds, all wasm-era docs.

**Docs:** `docs/Performance-Improvement-Plan.md` (the P-track source),
`docs/Why-Not-Thread-Safe.md`, `docs/A-Go-Routine-Thread-Safe-LState-Pool.md`,
`docs/Lua-5.2-to-5.5.md`; inherited history under `docs/glasm-history/`
(the full 5.2→5.5 upgrade program, the divergence ledger, the interp-vs-C
perf baseline).

**Identity restored:** module `github.com/pschlump/gopher-lua`,
`_GOPHER_LUA_VERSION` global (the pre-glasm name), `PackageName =
"GopherLua"`.

**Verified at carve:** `go build ./...` clean; `_tools/go-inline`
regeneration reproduces `vm.go` byte-identical to glasm's (state.go differs
only in the rewritten import path) — the codegen pipeline is intact; the
full test suite passes (`go test ./...`, ~6.4s).

## 3. Laws

1. **Byte-exactness with stock C Lua 5.5.1 is the contract.** Error
   messages, `luaV_*` semantics, observable iteration order, numerals —
   byte-for-byte. Any divergence must carry a row in a divergence ledger
   (start `docs/Divergence-Ledger.md`, continuing glasm's row numbering —
   its ledger lives in `docs/glasm-history/` as reference) and a covering
   test. The interpreter is no longer "the spec" (that was its role in
   glasm); stock C Lua is the spec, the interpreter is the product.
2. **`state.go` and `vm.go` are GENERATED** from `_state.go`/`_vm.go` by
   `_tools/go-inline`. Never edit them directly — edit the `_`-prefixed
   source and `make build` (or `make test`).
3. **No behavior change without its gate.** The golden-corpus gate (N3a,
   below) is the safety net for the whole P track; it lands before any
   perf work.

## 4. Track N — the embed package (`host/`, mirroring glasm's API where
mechanism-free)

| glasm host/ surface | Native equivalent | Effort |
|---|---|---|
| `Engine` (compile cache) | `FunctionProto` cache keyed by source SHA-1; memory tier only (recompile is µs–ms; disk tier unnecessary) | trivial |
| `WithVMCacheSize` pool | LRU over warm `LState`s; the A9 law carries over (one script per state at a time, mutex-held) | small |
| `RegisterGlobal` (wire + blob seam) | direct Go closure — the entire wire/blobs/hostfn seam deletes | trivial |
| `WithEventSink` | print override | trivial |
| `Deadline` | `SetContext` + count hook (both exist natively; pure-Go preemptible — the glasm row-99 STW-wedge class cannot occur here) | small |
| `WithGlobalsProtection` | Go metatable readonly guards + absent-global raise; error texts must match the glasm blob's exactly | medium |
| `WithMemoryBudgetBytes` | **new work — see N2** | the real piece |

- **N1 — embed core (~2–3 days):** `host/` package: `Engine`/`VM`/`Script`,
  proto cache, VM pool, `RegisterGlobal`/`RegisterValue`, `EventSink`,
  `Seed`, `Deadline`. Reuse glasm `host/`'s test-suite shape.
  **DONE (2026-11-01):** `host/` = `host.go` (Engine, compile cache,
  pool, registry-freeze rules), `vm.go` (VM, Run/Kill, deadline via
  `SetContext` + per-run cancel), `value.go` (the seam Value,
  API-identical to glasm's wire Value; conversions are direct
  LValue-tree walks carrying the retired encoder's caps — depth 8,
  512 entries + More, cycle/deep sentinels, 4096/frame, 64-deep
  host→guest guard, 65 536-value conversion budget replacing the
  staging-buffer bound), `hostfn.go` (trampolines; hostfn errors raise
  bare `lua_error` — verbatim Redis texts, the rt_sea_raise_nopos
  analog; panics contained), `lru.go`. Mechanism-born deletions vs
  glasm: `NewEngine` returns no error; no ErrScriptBound/one-script law
  (protos are self-contained); no TrapError (ApiErrorPanic maps to a
  plain engine error); pooled-VM recycling is runs-served (1024), not a
  frozen-GC watermark. Reentrancy: contention serializes on the mutex;
  ErrReentrant fires only while the owner sits inside a host function
  (hostDepth counter — only the lock-holder runs trampolines), narrower
  than glasm's whole-run window. Root-package companion fix (race
  class): `gcMemBytes`/`gcMode`/`warnOn` made concurrency-safe —
  package globals mutated at runtime, benign single-VM, unsound under
  parallel VMs; behavior-neutral (golden gate green); `gcMode` stays
  process-wide = a known cross-VM divergence (stock's is per
  global state), ledger row when the ledger exists. Gates:
  `host_test.go` + `compilecache_test.go` + `vmcache_test.go`
  (glasm shapes; expectations re-pinned where glasm's were wasm-seam
  residuals — error("inner",0) is bare per stock, print's table marker
  decodes instead of dropping the frame), green under `-race -count=2`.
- **N2 — sandbox (~3–5 days):** globals protection (byte-matched texts);
  stdlib selection — exclude `io`/`os`/`package`/`debug` by default, the
  `lua55_prod.wasm` posture, as an engine option; coroutine spawn cap
  (coroutines are goroutines here — unbounded `coroutine.create` needs a
  ceiling); **memory budget** — decision below.
  **DONE (2026-11-01):** all four surfaces, opt-in and off by default.
  Root package: `WithGlobalsProtection` = a one-way readonly walk
  (`LTable.SetReadonly`, guarded in `setField`/`setFieldString` +
  `baseRawSet`; `LState.RawSet`/`LTable.RawSet*` stay raw = the host
  staging seam) + the missing-global `__index` guard (glasm-blob texts);
  per-VM accounted budget (`membudget.go`: charges at table
  creation/growth + string-builder seams, monotone never-refunded,
  sticky-over + per-instruction boundary check beside the deadline
  check, pcall-catchable "not enough memory"; approximation = ledger
  row 107); `Options.MaxCoroutines` (live-thread counter, cap raises
  pcall-catchable "too many coroutines"); **`SetMx` deleted** (ledger
  row 106) with `cmd/glua`'s `-mx`. Host package: `host/protection.go`,
  `WithStdLibs`/`SafeStdLibs`/`AllStdLibs` (safe set = the prod posture,
  base's require/loadfile/dofile/load/warn additionally nil'd),
  `WithMemoryBudgetBytes` (armed before lib-open), `WithMaxCoroutines`,
  `VM.UsedBytes`; NewVM order = budget → libs → nils → staging →
  protection; Run stages KEYS/ARGV through the raw seam
  (guest-writable, pooled-VM-safe). Text deltas vs glasm's host tests:
  stock `luaO_chunkid` strips the chunkname's `=` (`test:1:`, not
  `=test:1:`) and rawset-under-protection raises prefixed in both
  shapes (ledger row 108). Gates: `host/sandbox_test.go` +
  `host/usedbytes_test.go` (protection battery, budget spins —
  concat/gmatch/tconcat, stdlib selection, cap+release), `-race
  -count=2` green, full golden corpus byte-stable (26s official leg).
  Founding `docs/Divergence-Ledger.md` (rows 106–109; continues glasm's
  numbering; 109 KEEP-OPEN: `LuaLDir`'s 5.1-era root in package.path).
- **N3 — gates:** (a) **golden-corpus gate — DONE (2026-11-01):**
  `golden_test.go` + `golden_runner_test.go` + `_goldens/` (58 goldens:
  26 official-suite, 11 glua, 21 u1). Every corpus case runs through this
  interpreter and must reproduce its golden event log byte-for-byte
  (PRINT/ERROR/RESULTS/GLOBALS; the runner is glasm's Interp engine
  conventions verbatim — deterministic os shims, SuiteFlags posture,
  sorted-table serialization). Regenerate deliberately:
  `GOLDEN_UPDATE=1 go test . -run TestGolden`. Findings at landing: the
  official suite contains blocks **stock C Lua 5.5.1 itself fails** under
  the user-tests posture (coroutine.lua:184's close-while-closing assert —
  glasm's U5 gate proved the interp byte-exact with stock *including*
  that failure), so pinned errors are stock-faithful, not divergences;
  the `_GOPHER_LUA_VERSION` global is normalized to `<version>` in
  GLOBALS lines so version bumps don't churn the corpus; the official leg
  skips in `-short` mode (26s full / 6.4s short), byte-stable across
  repeated runs; (b) **embedcmp interp legs — DONE (2026-11-01):**
  `cmd/embedcmp` (ported from glasm at N3b) races the C embedding API
  (`cmd/embedcmp/c/embedbench55.c` over the vendored `c/lua55` tree —
  C sources live under `c/` because a Go package dir may not hold
  `.c` without cgo) `{clua-reuse, clua-fresh}` against `host/`
  `{glua-cold, glua-fresh, glua-pool}` for the Redis-EVAL shape;
  mechanism deltas (memory-only cache → real cold parse; µs NewVM;
  `USED` = the N2 accounted gauge) are recorded with the run in
  `docs/Embedded-EVAL-Perf.md`. Headline vs glasm's same-day wasm run:
  **pool posture 3.4–16.5× C (wasm was 13–62×), hostfn ~0.30 µs (was
  ~0.85), no-pool 24–28 µs ≈ 1.6× C's own fresh (was ~2.6 ms — the
  pool is now an optimization, not feasibility), warm RSS 8.1–8.6 MB
  (was 50–70)**; RET cross-check zero mismatches, 7 scripts × 5 legs.
- **N5 — Ultima cutover — DONE (2026-11-01, ultima `main` 85c66a6-line,
  commit "N5: cut EVAL/SCRIPT onto the native Lua 5.5.1 embed engine"):**
  `../ultima` consumes this repo through `host` via a sibling `replace`
  (like `../pluto`); the vendored yuin 5.1 tree + host shim
  (`third_party/gopher-lua`) and the `gopher_lua_original` experiment are
  deleted. Contract deltas landed here for the cutover: the deadline/kill
  normalization (0403677) and the raise-line contract — prefixless raises
  report the raise-time traceback top frame; `rawset`'s readonly guard
  raises bare, Redis's C-builtin shape (3daee73). 5.5.1 semantic deltas
  ledgered ultima-side (`docs/Redis-Errors.md` §11): `error(nil)` →
  stock's `<no error object>`, integer staging lossless beyond 2^53.
  Gates: ultima `go test ./...` + `-race` over scripting/commands/tests;
  **the M8 differential corpus vs live redis-server 7.2.7 ran 332
  comparisons, 0 mismatches** (two engine-law divergences unit-pinned
  instead). Ultima docs: `AGENTS.md` (engine note, sibling list),
  `docs/implementation-history.md` §N5, `docs/ULTIMA-DESIGN.md` M8 row.

**Memory budget decision — TAKEN (N2, 2026-11-01):** the wasm posture's
semantics (per-VM budget, pcall-catchable "not enough memory") shipped as
the **per-VM accounted budget** — seam-charged, monotone, sticky-over
(`membudget.go`; the count-hook alternative was not taken — polling
`ReadMemStats` is process-global and approximate in the wrong direction).
`SetMx` is **deleted** (it armed a process-wide poller that `os.Exit(3)`'d
the host — never acceptable from an embed library); ledger rows 106/107.

## 5. Track P — the performance program

Source: `docs/Performance-Improvement-Plan.md` (2026-09-03 — its line
references predate the 5.5.1 encoding rewrite of `compile.go`/`opcode.go`;
revalidate each against the current tree before executing). Calibration
from glasm (`docs/glasm-history/Perf-vs-C-Lua.md`): interp ≈ **2.8–3.3× C
Lua** on the h_* micros; upstream-class numeric loops historically 3–10×.
Goal after P1–P3: **≤2× C Lua** on the Ultima-shaped benches; P4/P5
scope decided by data.

Every step: golden gate + full suite byte-exact green before merge, plus a
benchstat A/B against the previous commit — no divergence without a ledger
row (a perf change that alters observable behavior — table order, error
text, numerals — is a bug, not a trade).

- **P0 — measurement rig (~1 day):** port glasm's `cmd/luabench`
  interp-only (7 scripts + `-benchstat` + `-cpuprofile`); re-baseline;
  pprof top-10 to confirm the 2026-09 cost-center ranking still holds.
- **P1 — register-window slicing (1–2 weeks, est 10–25%):** frame setup
  knows `NumUsedRegisters`; slice the register window once per frame and
  make handlers store without `checkSize`/top bookkeeping (explicit top
  maintenance only at vararg calls, multret returns, SETLIST). Touches
  every handler in `_vm.go` — the go-inline pipeline is set up for exactly
  this.
- **P2 — table engine rewrite (2–4 weeks, est 1.5–3× on hash-heavy):**
  (i) replace `keys`/`k2i` double-map with intrusive insertion-order links;
  (ii) precomputed string-key hashes + own open-addressing table (kills
  per-op re-hashing, gives native `next()`); (iii) ≤8-slot linear
  small-table fast path (where most EVAL-shaped tables live); (iv) inline
  the no-metatable check into get/set field. `Next()` order observability
  is test-pinned — it is part of the byte-exact contract.
- **P3 — string interning (~1 week, est 5–15%):** intern at compile time
  (constants) and table-key store; per-VM pool; pointer-equality compares.
- **P4 — call path (data-scoped):** frame setup cost (push, capacity
  check, nil-fill, vararg shuffle). Glasm's U9/U10 lessons on call-path
  costs apply in shape even where the wasm mechanisms don't transfer.
- **P5 — misc (data-scoped):** `mainLoopWithContext`'s per-instruction
  `select` (make the deadline check count-sampled); `LNumber.String`
  `fmt.Sprint` path; `stringConcat`'s `[]string`+join.

## 6. Sequencing

| Order | Milestone                                        | Estimate  |
|-------|--------------------------------------------------|-----------|
| done  | N0 carve + this plan                             | —         |
| done  | N3a golden-corpus gate (58 goldens, byte-stable) | —         |
| done  | N1 embed core (`host/`, race-gated)              | —         |
| 2     | P0 measurement rig + baseline                    | ~1 day    |
| 3     | P1 register windows                              | 1–2 weeks |
| 4     | done N2 sandbox (budget, protection, stdlib)     | ~3–5 days |
| 5     | P2 table engine                                  | 2–4 weeks |
| 6     | P3 interning                                     | ~1 week   |
| done  | N3b embedcmp legs (Embedded-EVAL-Perf.md)        | —         |
| done  | N5 Ultima cutover (differential 332/0 vs live Redis) | —     |
| 8     | P4/P5                                            | by data   |

A usable Ultima engine exists after step 4 (~3 weeks in); the full program
is ~6–9 weeks.

## 7. Open decisions

- Memory budget: accounted per-VM (recommended) vs count-hook soft — N2.
- `config.go`'s `LuaLDir = "/usr/local/share/lua/5.1"` — a 5.1-era default
  inherited from upstream; stock 5.5 uses `.../5.5`. Cosmetic
  (`package.path` default only) but it is a divergence from stock — ledger
  row + fix when next touched.
- `channellib` (Go-channel stdlib extension) stays; embed users select
  libraries, so it ships only where wanted.
- Version tagging: the repo carries pre-carve tags (latest `v0.0.6`), so
  `make build` currently injects `PackageVersion=0.0.6` — tag the first
  release of this line to supersede them. Caveat learned in N5: a tag
  like `v5.5.0` is NOT consumable as a module version while the module
  path lacks a `/vN` suffix ("should be v0 or v1" — Go rejects major >1
  without one). Ultima currently pins `v0.0.0` under a directory
  `replace`, which is advisory. First real release must either move to
  `github.com/pschlump/gopher-lua/v5` (import-path churn for embedders)
  or stay `v0.x`-line (`v0.5.0` say); decide before any consumer pulls by
  version instead of replace.

## 8. Relationship to glasm

Glasm is frozen as the carve source (`0affa95`) and continues as the
Lua→wasm compiler project; no backflow is expected. Its `testdiff/`
harness + C 5.5.1 oracle remain available for ad-hoc cross-checks (identical
corpora). Inherited records live in `docs/glasm-history/` and stay
read-only history.

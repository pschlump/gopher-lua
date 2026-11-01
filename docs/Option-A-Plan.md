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
- **N2 — sandbox (~3–5 days):** globals protection (byte-matched texts);
  stdlib selection — exclude `io`/`os`/`package`/`debug` by default, the
  `lua55_prod.wasm` posture, as an engine option; coroutine spawn cap
  (coroutines are goroutines here — unbounded `coroutine.create` needs a
  ceiling); **memory budget** — decision below.
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
  repeated runs; (b) **embedcmp interp legs:** port glasm's
  `runtime/embedbench55.c` + `cmd/embedcmp` pattern — C-API
  `{clua-reuse, clua-fresh}` vs `{glua-cold, glua-fresh, glua-pool}` for
  the Redis-EVAL shape (short scripts × thousands of staged KEYS/ARGV
  runs).
- **N5 — Ultima cutover:** EVAL/SCRIPT over `host/` per Ultima M8
  (`../ultima`); glasm's How-To §5 remains the shape of the plan, rewritten
  for the native package.

**Memory budget decision (open, N2):** the wasm posture shipped a hard
per-VM budget (`rt_set_memlimit`) raising a pcall-catchable "not enough
memory" — the native target should match those semantics. Recommended:
**per-VM accounted budget** — an allocation counter on the LState
incremented at the allocation seams (`alloc.go` arena, table growth, string
concat buffers, upvalue/closure creation), crossing raises the same
catchable error. Alternative (cheaper, approximate): count-hook polling of
`runtime.ReadMemStats`. **Note:** the existing `SetMx` is neither — it is a
process-wide polling goroutine that `os.Exit(3)`s the whole daemon
(`_state.go` GopherLua-original APIs). It must never run in the embed path;
retire or replace it in N2 (ledger row).

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

| Order | Milestone | Estimate |
|---|---|---|
| done | N0 carve + this plan | — |
| done | N3a golden-corpus gate (58 goldens, byte-stable) | — |
| 2 | N1 embed core | ~2–3 days |
| 3 | P0 measurement rig + baseline | ~1 day |
| 4 | P1 register windows | 1–2 weeks |
| 5 | N2 sandbox (budget, protection, stdlib) | ~3–5 days |
| 6 | P2 table engine | 2–4 weeks |
| 7 | P3 interning | ~1 week |
| 8 | N3b embedcmp legs, N5 Ultima cutover | ~2–3 days |
| 9 | P4/P5 | by data |

A usable Ultima engine exists after step 5 (~3 weeks in); the full program
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
  release of this line (e.g. `v5.5.0`) to supersede them.

## 8. Relationship to glasm

Glasm is frozen as the carve source (`0affa95`) and continues as the
Lua→wasm compiler project; no backflow is expected. Its `testdiff/`
harness + C 5.5.1 oracle remain available for ad-hoc cross-checks (identical
corpora). Inherited records live in `docs/glasm-history/` and stay
read-only history.

# Lua→Wasm Compiler — Detailed Design & Test Plan

**Date:** 2026-09-03
**Status:** Design
**Companion to:** `docs/Performance-Improvement-Plan.md` §7 (Option E)
**Goal:** A production Lua 5.1 → WebAssembly compiler reusing this repo's frontend (`parse/`, `ast/`, `compile.go`), targeting wazero on linux/amd64 + linux/arm64, with a test regime strong enough to trust it inside a Redis-class network daemon running untrusted client scripts.

---

## 1. System overview

```
                 ┌────────────────────────── Go host (your Redis) ─────────────────────────┐
                 │  script cache (SHA→module) · wazero Runtime · host module ("redis")     │
                 │  reply⇄linear-memory conversion · deadline flag · memory caps           │
                 └───────▲──────────────────────────────────▲──────────────────────────────┘
                         │ imports "rt.*"                   │ imports "redis.*"
                 ┌───────┴────────────┐            ┌────────┴──────────────┐
                 │ runtime.wasm       │            │ script.wasm          │
                 │ (C Lua 5.1 runtime │◄───────────│ (per-script, emitted │
                 │  minus lvm; built  │  rt_* calls│  by the new backend) │
                 │  once, shipped)    │            │                      │
                 └────────────────────┘            └──────────────────────┘
                          ▲                                ▲
                          └───── same pipeline, both oracles ─────┐
  source.lua → parse.Parse → lua.Compile → FunctionProto → backend → script.wasm
                 (existing frontend, reused untouched — see §2)
```

Three code-producing pieces and one host piece:

1. **`wasm/`** — a standalone wasm binary emitter (sections, LEB128, encoders for the ~50 instruction forms we generate). No dependencies; usable for any wasm project.
2. **`luawasm/`** — the backend: `[]FunctionProto` (tree) → module. This is the new compiler.
3. **`runtime/`** — C Lua 5.1 runtime adapted to freestanding wasm32, compiled **once** with wasi-sdk/clang, shipped as a versioned blob (`runtime.wasm`).
4. **Host shim** — Go package wiring wazero: instantiation, host imports, conversion, lifecycle.

### 1.1 Key architectural decisions (beyond the plan doc's §7)

| # | Decision | Rationale |
|---|---|---|
| A1 | **One script module imports two modules (runtime, host); the runtime never calls back into script code.** All Lua→Lua calls compile to `call_indirect` on a funcref table *inside* the script module. | Kills the circular-import problem entirely. Works because in the Redis model every function a script can call is either its own proto (in-module) or a host/C function (downward call). |
| A2 | **Single wasm calling convention for every emitted Lua function** (see §4.2); results returned via the frame, count as i32 return. | Uniform handling of multret/vararg; no reliance on wasm multi-value; one table, one signature → cheap `call_indirect`. |
| A3 | **v1 control flow is flattened** (`loop` + `br_table` over basic blocks); v2 adds structured-region emission (if/loop nesting) where the jump graph is reducible, falling back per-function. | Lua 5.1 bytecode has arbitrary gotos (this fork even accepts Lua 5.2 `goto`); wasm requires structured control. Flattened costs one indirect branch per *basic block* (not per instruction) and is unconditionally correct — v2 is a pure optimization. |
| A4 | **Line numbers are passed as immediates to every throwing runtime call.** `DbgSourcePositions[pc]` is known at emit time; `rt_*` signatures take a `line i32`. | Error messages get correct line info for free, making **byte-exact error-message diffing** against the oracle possible (§8.3). |
| A5 | **Deadline via direct memory write.** Host writes a flag at a fixed linear-memory address; emitted loop back-edges load it and branch to `rt_deadline`. | No host call on the hot path; replaces the interpreter's per-instruction `select` on `ctx.Done()`. |
| A6 | **Arena reset instead of GC for the common case.** Stateless script runs bump-allocate and reset; mark-sweep (frames-as-roots) engages only past a per-run memory threshold. | Most Redis scripts never collect. GC becomes a safety valve, not a component on the critical path. |
| A7 | **Bring-up vehicle: stock C Lua 5.1 compiled whole (including `lvm.c`) to wasm first.** | Validates the toolchain and gives an in-wasm oracle *before the backend exists* (§8.4). The backend then progressively replaces `lvm.c` calls with inline code — same seam LuaJIT used. |

---

## 2. Reused from this repo (unchanged)

| Component | Location | Note |
|---|---|---|
| Lexer/parser | `parse/lexer.go:468` (`Parse → []ast.Stmt`), `parser.go.y` | untouched |
| AST | `ast/` | untouched |
| AST→bytecode | `compile.go:1849` (`Compile → *FunctionProto`) | untouched; carries scoping, upvalue capture, vararg lowering, constant handling |
| IR | `FunctionProto` (`function.go:25`) | 41 opcodes; `NumUsedRegisters`, `NumParameters`, `IsVarArg`, `NumUpvalues`, `Code`, `Constants`, `FunctionPrototypes[]`, `DbgSourcePositions[]` |
| Oracles + corpora | `_lua5.1-tests`, `_glua-tests` | the interpreter in this repo is the primary differential oracle; C Lua 5.1 native is the semantic authority |

Not reused on this path: `vm.go`/`_vm.go`, `state.go`, `table.go`, `alloc.go` (the interpreter keeps them; the wasm path replaces them).

---

## 3. Linear memory map (script instance)

Fixed layout, compile-time constants shared by backend, runtime, and host:

```
0x0000_0000  null guard (never mapped use; trap on deref)
0x0000_1000  control block:
               +0x00 err_flag i32        (runtime→code propagation)
               +0x08 err_value TValue    (error object)
               +0x10 deadline i32        (host writes; code polls at back-edges)
               +0x18 mem_watermark i32   (per-run allocation threshold)
               +0x20 gc_active i32
               +0x28 frame_top i32       (current frame-stack top)
               +0x30 stack_limit i32
0x0001_0000  frame stack (grows up; N slots × 16B TValue; capped, overflow → rt_error)
0x1000_0000  heap arena (bump pointer rt_alloc; mark-sweep above watermark)
0x8000_0000  args/results staging area (host writes KEYS/ARGV blobs; script returns here)
0xF000_0000  interned-constant handles + closure descriptors (module data)
```

### 3.1 Value representation

- **In memory (heap, frame slots, table cells):** 16-byte `TValue { u32 tag; u32 pad; f64 num; u32 ref }`.
  Tags: `0 nil, 1 false, 2 true, 3 number, 4 str, 5 table, 6 func, 7 userdata, 8 thread`.
- **In wasm locals (compiler convention):** a register is three locals `(t i32, n f64, r i32)`; exactly one of `n`/`r` meaningful per `t`. Constants: numbers inline as `f64.const`; strings interned at module init (data section → `rt_intern` → handle table at fixed offsets, loaded with `i32.load` of a constant address); nil/true/false are tag immediates.
- `ref` is a 32-bit offset into the arena — never a raw anything the host GC could care about. (Your GC, your rules: heap-cell NaN-boxing is a legal later optimization, unlike in the Go heap.)

### 3.2 Register placement (from §7.3 of the plan, made precise)

`compile.go`'s upvalue descriptors identify, per proto, which registers any nested `OP_CLOSURE` captures.

- **Captured registers** → frame slots in linear memory (addressable by open-upvalue objects).
- **Unaptured registers** → wasm locals.
- `OP_GETUPVAL`/`OP_SETUPVAL` → load/store through the closure's upvalue array (open: indirected to frame slot; closed: cell in the object).
- `OP_CLOSE`/function return → `rt_close_upvalues(frameSlotAddr)` — doubly-linked open-upvalue list exactly as in C Lua.

For-loop registers A..A+2 (hidden control vars) are **always** wasm f64 locals — they are uncapturable by construction; only the visible copy A+3 is materialized each iteration.

---

## 4. The backend (`luawasm/`)

### 4.1 Module skeleton

```wat
(module
  (import "rt"    "alloc"        (func $rt_alloc (param i32) (result i32)))      ;; ~30 imports
  (import "rt"    "gettable"     (func $rt_gettable (param i32 i32 i32 i32)))    ;; L tbl dst line
  (import "rt"    "settable"     ...)
  (import "rt"    "arith"        ...)   ;; op lhs dst line  (metamethod/coercion path)
  (import "rt"    "concat"       ...)
  (import "rt"    "newtable"     ...)
  (import "rt"    "newclosure"   ...)   ;; protoIdx upvalDescPtr -> handle
  (import "rt"    "find_upval"   ...)
  (import "rt"    "close_upvals" ...)
  (import "rt"    "call_cfn"     ...)   ;; C/host function dispatch
  (import "rt"    "error"        ...)   ;; msg TValue line
  (import "rt"    "stackoverflow" ...)
  (import "redis" "call"         ...)   ;; host functions (ptr len) -> (ptr len)

  (table $protos N funcref)              ;; one entry per FunctionProto, proto index = slot
  (func $lua_init  ...)                  ;; intern constants, build closure descriptors
  (func $lua_main  (param i32) (result i32))  ;; host entry: want -> nret, results staged
  (func $p0 $p1 ... $pN)                 ;; one per proto, common signature, all in $protos
  (memory 1 ..max)
  (export "lua_main" ...) (export "lua_init" ...))
```

### 4.2 Calling convention (every emitted Lua function)

```
sig: (param $L i32)         ;; thread/state pointer (current frame base etc.)
     (param $frame i32)     ;; absolute frame-slot base for this activation
     (param $nargs i32)
     (param $want i32)      ;; >=0: exactly want results; -1: multret
     (result $nret i32)     ;; results occupy $frame..$frame+$nret
```

- Callee sets up its window: uncaptured registers → locals; captured ones are already addressable at `$frame + k*16`.
- Stack-depth check at entry against `stack_limit` → `rt_stackoverflow` (message parity with the interpreter).
- `OP_CALL` lowering:
  1. load callee `TValue` from register A;
  2. tag = Lua-closure → `call_indirect $protos (local.get protoIdx)` with callee frame at `frame + (A+1)*16`;
  3. tag = C/host function → `rt_call_cfn`;
  4. else → `rt_call_meta` (`__call` metamethod loop, in runtime);
  5. after any call: `if err_flag != 0 { return -1 }` — the entire error-propagation protocol.
- `OP_TAILCALL` → trampoline: write a call-descriptor (callee, frame, nargs) into the thread state and return; `$lua_main` and every `call` site run a small loop that re-dispatches descriptors — `return f(x)` recursion is O(1) wasm stack.
- `OP_RETURN` with B=0 (multret): copy from `$frame` up to current top; else fixed count + nil padding (matches `copyReturnValues` semantics in `_vm.go:70`).

### 4.3 Control flow

- v1 (correctness first): function body = basic blocks; `loop $next { block … br_table }` on a block-id local; each block ends by setting `next_id` and `br $next`. All register state lives in locals, which persist across blocks.
- v2 (perf): structural analysis over the jump graph (loop detection via back-edges, if/else region formation) → nested wasm `block`/`loop`; irreducible functions (rare; `goto`-heavy code) fall back to v1 per-function. Emit both under a flag and **differential-test them against each other** (§8.5).

### 4.4 Opcode lowering table (complete inventory, 41 opcodes)

| Opcode(s) | Lowering |
|---|---|
| `MOVE`, `MOVEN`, `LOADK`, `LOADBOOL`, `LOADNIL` | pure inline local moves / immediate tags / constant-handle loads |
| `GETUPVAL`, `SETUPVAL` | inline: closure upvalue array load/store |
| `GETGLOBAL`, `SETGLOBAL` | `rt_gettable/settable` on the globals-table handle with the interned name handle (globals are a table; env chain per closure) |
| `GETTABLE`, `GETTABLEKS` | fast path inline: table-tag ∧ number-key ∧ `0 < key ≤ array_len` → bounds-checked `i32.load` of the array part; else `rt_gettable` |
| `SETTABLE`, `SETTABLEKS` | symmetric; the runtime call also owns the write barrier when GC is active |
| `NEWTABLE` | `rt_newtable(hintFromBC)` |
| `SELF` | `rt_gettable` + register move |
| `ADD..POW`, `UNM` | fast path: both tags `number` → f64 op inline; else `rt_arith(op,…)` (string coercion, `__add`… ) |
| `NOT`, `TEST`, `TESTSET` | inline tag tests |
| `LEN` | `rt_len` (string len inline fast path: tag=str → i32.load header) |
| `CONCAT` | `rt_concat(frame, start, count, line)` (right-associative, `__concat`) |
| `JMP` | branch (v1: set next-id; v2: wasm br) |
| `EQ` | fast: number×number → f64 cmp; string×string (both interned) → `i32 eq` on handles; else `rt_equals` |
| `LT`, `LE` | fast: number×number; else `rt_lessthan` (mixed-type errors, `__lt`/`__le`) |
| `CALL`, `TAILCALL`, `RETURN` | §4.2 |
| `FORPREP`, `FORLOOP` | `FORPREP`: guard/convert A..A+2 to numbers once (`rt_forprep` handles metamethod/coercion errors) → f64 locals; `FORLOOP`: pure inline f64 add + compare + visible copy at A+3 |
| `TFORLOOP` | runtime-driven: call iterator via §4.2 mechanism, nil test inline |
| `SETLIST` | `rt_setlist(tbl, frameSlot, n, baseIndex)` (flush at `FieldsPerFlush`) |
| `CLOSE` | `rt_close_upvals` |
| `CLOSURE` | `rt_newclosure(protoIdx, descPtr)` — capture descriptor in module data |
| `VARARG` | copy from vararg area of frame (`frame + np*16 …`), nil-pad to B |
| `NOP` | nothing |

**Fidelity note:** every `rt_*` behavior contract is written down as "what `_vm.go` does today" — the interpreter source is the executable spec (e.g. `opArith` `_vm.go:831`, `stringConcat` `_vm.go:930`, `lessThan` `_vm.go:967`, `equals` `_vm.go:989`, vararg frame shuffle `state.go:1192-1240`).

### 4.5 Init & host entry

- `$lua_init` (host calls once after instantiate): intern all string constants into the handle table, build closure descriptors, create the globals table, seed `math.random` (host-provided seed).
- `$lua_main(want)`: push KEYS/ARGV as TValues from the staging area, invoke the script proto, return `nret`; results TValues already in the staging area for host conversion.
- Per-run lifecycle v1: **fresh instance per execution** (wazero instantiation is µs–low-ms). v2 option: `rt_reset()` (arena top ← start; globals snapshot restore; interned strings retained) — measure before adopting.

---

## 5. Runtime module (C Lua 5.1 port)

- **Source**: Lua 5.1.4 `ltable.c lstring.c lgc.c lmem.c lobject.c ltm.c lvm.c(arith/compare helpers only) lapi.c(subset) lfunc.c ldo.c(structure only) lauxlib(subset)` — everything except the bytecode interpreter loop; MIT license.
- **Toolchain**: `clang --target=wasm32-unknown-unknown -nostdlib` (or wasi-sdk with a stub libc: `memcpy/memset/memmove/strlen` only). Built once per release, checksummed, embedded in the Go binary via `//go:embed`.
- **setjmp/longjmp → error protocol**: `rt_error` sets `err_flag/err_value` and returns; callers (emitted code) early-return; `pcall` = `rt_pcall` saves frame/stack state, invokes, restores on flag — mirrors `PCall` semantics in `state.go:2029`.
- **Allocator**: bump arena (`rt_alloc`), watermark check, mark-sweep collector (roots: frame list, globals, registry, upvalue list) run inside `rt_alloc` only when past watermark.
- **Strings**: interned hash table (cached 32-bit hashes — the fix §2b of the plan wanted, now free); `string.format` etc. ported from `lstrlib.c` (deterministic subset).
- **Exports**: the `rt_*` surface of §4.4, frozen and versioned (`LUA_RT_ABI = 1`); the backend refuses modules whose runtime ABI doesn't match.
- **In-wasm determinism**: no clock, no `os.*`, host-seeded PRNG.

---

## 6. Host shim (Go)

```go
type ScriptEngine struct {
    rt      wazero.Runtime          // shared
    rtMod   api.Module              // runtime.wasm, instantiated once
    cache   map[SHA256]*compiledScript  // module + metadata (fnIdx→proto, line tables)
}

type compiledScript struct {
    wazMod  wazero.CompiledModule
    meta    ScriptMeta              // proto info for stack traces on the Go side
}

func (e *ScriptEngine) Run(sha []byte, keys, argv []string, deadline time.Time) (Result, error)
```

- Host module `"redis"`: `call(ptr,len,argc,argvArea) -> (ptr,len)` implemented over `api.Module.Memory()`; reply→TValue writer emits Tier-3 typed tables (`[]float64` array part / string hash) directly.
- Deadline: `mem.WriteBool(ctrlDeadline, true)` from a watchdog — no guest involvement until the next back-edge.
- Memory cap: instance max pages + watermark → `rt_oom` error object; Redis returns `-BUSY`/script error per policy.
- Errors escaping as wasm traps are **always a backend bug** (§8.7 classification) — logged with module SHA + meta, never surfaced as a script error.

---

## 7. Test strategy — overview

**Backbone principle: differential testing against two independent oracles.**

| Oracle | Role | Notes |
|---|---|---|
| **gopher-lua interpreter (this repo)** | primary differential oracle | same frontend, same test corpus, run-for-run comparison; catches backend divergence with zero setup |
| **C Lua 5.1 (native, via wasmoon-style build of stock Lua — see M3)** | semantic authority | where the interpreter itself deviates from Lua 5.1, C wins; every known divergence goes in the ledger (§8.9) |

Equality oracle = **normalized event log** (not just final results):

1. Every `print`, `error`, `assert` failure, and pcall-captured error → one log line, normalized.
2. End of run → deterministic serialization of all globals (sorted-key traversal), returned values, and (in a special `--inspect` mode) any tables reachable from them.
3. Byte-compare logs. A diff is a failure regardless of which side is "wrong" — then the ledger decides.

Test pyramid:

```
L7  production soak (fuzz 24/7, Redis integration, perf gates)
L6  conformance suites   (_lua5.1-tests, _glua-tests, curated real-world Redis scripts)
L5  differential fuzzing (random Lua programs, oracle comparison)
L4  end-to-end scripts   (multi-feature programs, coroutines of features per file)
L3  per-opcode semantic tests (41 opcodes × edge-case matrix)
L2  component tests      (emitter, ABI, runtime C, host shim)
L1  unit tests           (LEB128, sections, TValue codec, allocator)
```

Every layer gates a milestone (§9). CI partitions: per-commit = L1–L4 (~minutes); nightly = L5–L6 + sanitizers; weekly = perf gates.

---

## 8. Test design, layer by layer

### 8.1 L1/L2 — component tests

**Emitter (`wasm/`):**
- Round-trip: emit → decode with wazero's module reader → re-encode → byte-identical.
- External validation: CI job runs `wasm-tools validate` / `wasm2wat` (wabt) over a corpus of emitted modules.
- Every instruction form has a micro-module whose *execution result* on wazero is asserted (don't test bytes, test behavior).
- Malformed-input tests: emitter must error cleanly on impossible requests (never produce an invalid module).

**Backend ABI:**
- Hand-written 3-proto micro modules exercising the calling convention: fixed-arity, multret, vararg, want=-1, missing-arg nil padding — expected register/frame layouts asserted via exported test hooks (a `#ifdef LUAWASM_TEST` export that dumps frames).
- Error-propagation contract: any `rt_*` setting `err_flag` must produce `nret=-1` from every level — tested with synthetic injected errors.

**Runtime (C):**
- The same C sources compile **natively** with a test harness (`runtime/tests/`): unit tests for allocator, table ops (incl. the `Next()` ordering semantics), string interning, upvalue open/close.
- Native build runs under ASan+UBSan in CI. Pointer width differs from wasm32 — the runtime keeps refs as `uint32_t` offsets internally precisely so the native build is layout-identical, not just logic-identical.
- wasm32 build additionally gets wasi-sdk's experimental wasm ASan when available; primary wasm safety net is that OOB = trap = loud failure.

### 8.2 L3 — per-opcode semantic matrix

For each of the 41 opcodes: a table in `luawasm/opcode_test.go` of (Lua snippet isolating the opcode, inputs, expected outputs, expected event log). The edge-case checklist each opcode must survive:

- Numbers: NaN (`NaN==NaN` false, `NaN<1` false, NaN keys error on table store), ±0, ±Inf, `1e308*10`, integer-valued floats, `-0//1`-style modulo signs (`luaModulo` semantics), `math.pow` edge powers.
- Coercion: `"10"+1`, `"0x10"+0` (5.1 rules), `"abc"+1` → error with exact message+line, string→number in for-loops.
- Table keys: `t[1]` vs `t["1"]` vs `t[1.0]` identity; array-boundary writes (`t[#t+1]`), holes, `#t` with trailing nils; large keys → hash part (`MaxArrayIndex` boundary from `config.go`).
- Closures/upvalues: the classic `for i=1,3 do f[i]=function() return i end end` (each closure distinct, values 1..4 per 5.1 semantics); break-from-loop closing; upvalue shared across two closures; `OP_CLOSE` via `break`.
- Varargs: `select('#')`, nil-padded varargs (`select('#', nil, nil) == 2`), `...` in non-vararg function (compile error parity), vararg + fixed params layout (the frame shuffle at `state.go:1192-1240`).
- Metamethods: each of `__index/__newindex/__call/__eq/__lt/__le/__unm/__concat/__len` (table and non-table operands), `__index` chained tables, `__newindex` function vs table, metamethod erroring mid-dispatch, metamethod invoked **exactly once** (property test with counters).
- Errors: non-string error objects (tables/nil), `error()` with level 0/1/2, pcall of pcall, error inside metamethod, error inside `__gc`-adjacent paths (out of scope: no `__gc`), stack-overflow message parity at the exact recursion depth (needs identical frame accounting — assert message and that depth differs by ≤ ε or matches documented divergence).
- Concat: right-associativity with numbers (`1 .. 2 .. 3`), `__concat` on left then right, long chains.
- Control: `goto` (this fork's 5.2 extension) — forward and backward (loop continue patterns) — the flattening fallback's stress test.
- Equality: `1 == 1.0` true; table identity by handle; string vs number never equal in `==` (but coerced in arith) — assert against oracle.

Each snippet runs in **all three engines** (interpreter, C-Lua-wasm, backend-wasm) and event logs must match; C divergences land in the ledger.

### 8.3 L4 — end-to-end + error-message fidelity

- Multi-feature programs (each combining ≥6 opcode classes, closures, metamethods, string lib).
- **Byte-exact error message suite**: a corpus of ~100 scripts each raising a specific error; assert identical (message, line) triples across engines — enabled by decision A4 (line immediates).
- Debug parity: `debug.traceback()` shape under a flag (or documented divergence + ledger entry).

### 8.4 L5 — differential fuzzing

- **Generator**: grammar/AST-based random Lua program generator (seeded, shrinking support). Reuse this repo's AST to generate *valid* programs by construction, then print them back to source — guarantees syntactic validity while exercising semantic corners. Mutations: swap operators, wrap subexpressions in `(… or nil)`, change literal kinds, insert `pcall` wrappers.
- Run: interpreter vs backend (and nightly vs C-Lua-wasm), compare event logs; any mismatch minimizes the seed and files a regression test automatically.
- **Bytecode-level fuzz**: mutate `FunctionProto` trees (register indices, RK bits, jump targets) — backend must either compile-and-agree with the interpreter running the same mutated proto (the interpreter is fed identical protos via an internal test hook) or reject the proto cleanly. Never trap.
- **Emitter fuzz**: random module shapes → wazero instantiate must succeed or our validator rejects pre-emptively.
- Budget: nightly corpus growth with dedup; 24/7 soak during M5–M6.

### 8.5 L6 — conformance

- `_glua-tests` and `_lua5.1-tests` run through the differential harness (they already run against the interpreter; add the backend and C-Lua-wasm as columns). Explicit, maintained skip list (os/io-dependent, interpreter-known-bugs) — **every skip has a reason string**; CI fails on skips without one.
- **Curated real-world corpus**: collect public Redis Lua scripts (rate limiters, locks, token buckets, cjson/cmsgpack users from Redis docs and common libraries) — run through the full oracle matrix. This is the corpus that actually predicts production behavior for your Redis.

### 8.6 L7 — production hardening tests

- **Isolation**: run each corpus script twice in the same instance (v2 lifecycle) / new instance (v1) — globals and arena must be indistinguishable from a cold run (memory-diff the globals serialization).
- **Deadline**: infinite-loop scripts must be killed within `deadline + ε` (measure the back-edge check interval), returning the Redis-documented error.
- **Memory caps**: allocation-heavy scripts hit the watermark → clean OOM error, not a trap or host OOM.
- **Recursion**: non-tail deep recursion → "stack overflow" error (not a wasm trap) at a deterministic frame count; tail recursion 10⁷ deep → success, flat memory.
- **Determinism**: every L6 corpus script executed 5× — byte-identical event logs; `math.random` sequences reproducible from host seed.
- **Chaos host**: `redis.call` shim that returns wrong types, huge replies, and errors mid-iteration — engine must survive all (these are host-behavior tests of the conversion layer).
- **Soak**: 24h mixed corpus at randomized deadlines/caps; asserts no memory growth (host-side RSS slope), no trap classes.

### 8.7 Failure classification (drives CI severity)

| Class | Meaning | CI action |
|---|---|---|
| TRAP | wasm trap (OOB, div-by-zero miscompiled, unreachable) | **always a backend bug**; blocks |
| DIVERGE | event-log mismatch vs interpreter | blocks; ledger decides fix-vs-document |
| C-DIVERGE | interpreter and C Lua disagree | ledger entry; blocks only if backend ≠ interpreter |
| TIMEOUT-DEADLINE | deadline honored | expected behavior test |
| OOM | watermark honored | expected behavior test |

### 8.8 Semantic coverage instrumentation

The differential harness logs an opcode histogram per script (interpreter side). A coverage report maps every corpus test to opcodes exercised; CI fails if any opcode's semantic matrix (8.2) isn't fully covered by at least one automated layer. Dead opcodes (`NOP`, `MOVEN` corner arity) must still have their rows green.

### 8.9 The divergence ledger

`docs/Lua-Wasm-Divergence-Ledger.md` — one row per known interpreter↔C-Lua↔backend difference: what, why, oracle ruling, ticket. Seed it from the README's "Differences between Lua and GopherLua" section. **Rule: a divergence without a ledger row is a bug; a row without a test is a bug.**

### 8.10 Performance testing

- Benchmark corpus = Week-0 corpus from the plan doc (string ops, cjson decode, table building, redis.call-heavy, numeric loops) + standard Lua benchmarks (fannkuch, nbody, binary-trees) for external comparability.
- Fixed CI hardware, `benchstat`, gates: backend-wasm ≥ 2× interpreter fork on numeric corpus at M8; no regression >5% week-over-week; memory: peak arena per corpus script trended.
- Comparative columns: interpreter fork, C Lua 5.1 native (informational, same CI box).

---

## 9. Milestones — each with a hard test gate

> **Status (2026-09-03): M0 complete.** `wasm/` package (wasm.go, instr.go, leb128.go — 685 lines source + 566 lines tests), wazero v1.12.0 as test-only dependency. Gates: 13 tests green (LEB128 spec vectors, section-framing walk, golden-bytes stability, and wazero execution of arith/loop/br_table/memory+data/call/call_indirect/globals/host-roundtrip); `wasm2wat` (wabt 1.0.41) validates emitted modules. M0 open questions answered on darwin/arm64 (M4 Max): instantiation ≈ **7.4 µs** (compiled module) / 8.3 µs compile+instantiate for a tiny module → **fresh-instance-per-run is viable** (v1 lifecycle decision); exported-call overhead ≈ **21 ns**; wasm recursion reached the **2,000,000-frame safety cap without trapping** (re-probe on linux/compiler engine at M3 — darwin/arm64 uses wazero's interpreter engine, so these numbers are the conservative case).

| M | Deliverable | Gate (tests that must be green) | Est. |
|---|---|---|---|
| **M0** | Spike: `wasm/` emitter core (sections, LEB128, mov/arith/branch/call), host↔guest roundtrip on wazero | Emitter micro-modules execute correctly on wazero; `wasm2wat` validates; round-trip byte-identical | 1 wk |
| **M1** | **Test infrastructure first**: differential harness (event logs, globals serialization), corpus ingestion, CI wiring; oracle validated by diffing the interpreter against itself (must be 0 diffs) | Self-diff = 0 across `_glua-tests`; harness runs in CI per-commit | 1 wk |
| **M2** | Stock C Lua 5.1 compiled whole to wasm (bring-up vehicle + third oracle); host shims for its `print`/test needs | `_lua5.1-tests` ≥95% pass inside wazero (documented skips only); divergence ledger seeded | 1–2 wk |
| **M3** | Runtime split: `rt_*` ABI implemented (C, exported), native unit tests, ASan/UBSan CI, error protocol | L1/L2 for runtime green; ABI freeze review | 2–3 wk |
| **M4** | Backend v1 core: all pure-inline opcodes + arith/compare fast paths + `CALL/RETURN` direct & indirect, flattened control flow; scripts with no tables run | Opcode matrix rows green for covered set; differential corpus subset (hand-chosen ~200 cases) 100% match | 3–4 wk |
| **M5** | Backend v1 complete: all 41 opcodes, closures/upvalues/varargs/metamethods/pcall/tailcall trampoline, line immediates | Full `_glua-tests` + curated Redis corpus: 0 unledgered DIVERGE; error-message suite byte-exact | 3–4 wk |
| **M6** | Hardening: arena/GC watermark, stack limits, deadline, determinism, isolation | All 8.6 tests green; 7-night fuzz soak zero new classes; TRAP count = 0 over corpus ×10⁷ executions | 2 wk |
| **M7** | Redis integration: host module, script cache, EVAL/EVALSHA/SCRIPT FLUSH flows, caps, typed-table reply conversion | Integration suite (incl. deadline kill, OOM, flush-isolation) green in the Redis repo | 2–3 wk |
| **M8** | Performance: v2 structured control flow (differential-tested against v1 per-function), inline caches at hot sites, instantiate-vs-reset measurement | Perf gate: ≥2× fork on numeric corpus; v1↔v2 differential zero diffs; no >5% regressions | 3–4 wk |

**Total: ~3.5–5.5 months** solo (consistent with the plan doc's estimate). M1 before M4 is deliberate: the differential harness existing *before* the backend is what makes every later milestone measurable. M2 before M3 is deliberate too: a whole-C-Lua-in-wasm oracle de-risks the runtime port with zero new compiler code.

---

## 10. Repo layout

```
wasm/            emitter (pure Go, no deps)         + wasm/*_test.go
luawasm/         backend: proto→module              + luawasm/opcode_test.go, corpus/
runtime/         C sources + wasi-sdk build + native test harness
runtime/wasm/    build artifacts (embedded via go:embed, checksummed)
host/            wazero shim, host module, conversion (Go)
testdiff/        differential harness, event-log oracle, generators, CI glue
docs/Lua-Wasm-Divergence-Ledger.md
```

---

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| wazero maintenance pin | version-pinned; module is portable → wasmtime-cgo escape hatch; `wasm2wat` keeps us toolchain-honest |
| Flattened control flow too slow in v1 | M8 v2 exists; measure at M4 gate with the numeric corpus before committing to v2 timing |
| Runtime ABI churn during M3–M5 | freeze at M3 gate; additive-only afterwards with minor version bump |
| Error-message parity fights (Go `fmt` vs C `sprintf` formatting of floats) | format via a single shared formatter (C side) for messages produced in both engines; ledger documents the rest | 
| Coroutine scope | Out of scope for Redis subset (decision); general-purpose build needs the Asyncify/CPS study (plan §7.7) |
| Deep-recursion wasm stack traps | enforced Lua-level limit below the engine's (measure wazero's at M0; set `stack_limit` conservatively) |
| Divergence between the two runtime sources of truth | ledger discipline (8.9); C Lua is final authority |

**Open questions to resolve at M0–M2:** wazero instantiation cost on target hardware (drives v1-vs-v2 lifecycle); exact wazero max wasm-stack depth (drives `stack_limit`); wasi-sdk wasm-ASan maturity (drives whether wasm-mode sanitizers are CI or advisory); whether this fork's `goto` surfaces any other codegen oddities (grep `compile.go` during M4).

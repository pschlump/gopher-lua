# Gopher-Lua Performance Improvement Plan

**Date:** 2026-09-03
**Scope:** This fork (`pschlump/gopher-lua`, based on `yuin/gopher-lua`), targeting use as the scripting engine inside a Go implementation of Redis. Audience: compiler-experienced developer.

---

First, a note on what this fork already has, so nothing below re-does work that's done:

**Already applied in this tree** (this is `yuin/gopher-lua` + the known performance patches): per-opcode function-pointer dispatch instead of a switch (`vm.go:299`), a bulk float allocator with a 0–127 small-number cache (`alloc.go:57`), `go-inline`-generated opcode bodies with the register helpers manually inlined (`vm.go`, `_vm.go` as source), and a split string-constants table (`stringConstants`, used by `OP_GETTABLEKS`/`OP_SETGLOBAL` at `vm.go:641`).

---

## 1. Where the cycles actually go

Ranked by my read of the code, for typical Redis-script workloads (string/table manipulation, small numeric loops, lots of `redis.call`):

| # | Cost center | Evidence |
|---|---|---|
| 1 | **`LValue` is a Go interface.** Every register slot is 16 bytes; every arithmetic op does 2 interface type-asserts, then re-boxes the result into an arena float (`alloc.go` keeps the *allocation* amortized but the indirection remains); every table load is an interface load + assert. | `value.go:29`, `vm.go:2220-2223`, `vm.go:1818` |
| 2 | **Per-write register bookkeeping.** Every register store re-checks capacity (`checkSize`) and conditionally bumps `top`. Real Lua computes the frame top once at call time; instructions just store. | inlined `Set` block repeated in every handler, e.g. `vm.go:311-330` |
| 3 | **Tables carry up to 5 collections.** `array`, `strdict`, `dict`, `keys`, `k2i`. Every `RawSetH`/`RawSetString` does a *second* map operation (`k2i`) for iteration order. Go maps don't cache string hashes — every `t.foo` re-hashes the full key bytes. | `value.go:143-151`, `table.go:202-248` |
| 4 | **Metamethod check on every table miss.** `getField` → `metaOp1("__index")` through non-inlined call chains even when there's no metatable. | `state.go:1276-1305` |
| 5 | **No string interning.** Equality and hashing compare/rehash full bytes; Lua's `TString` gives pointer-equality and cached hashes. | `value.go:95` |
| 6 | **Call overhead:** push frame, `IsFull` check, nil-fill the whole register window, vararg shuffle. | `state.go:1132-1244`, `vm.go:1168+` |
| 7 | **Dispatch:** one indirect call per instruction (`vm.go:31`). Cheaper than it looks — Go's switch over 41 dense opcodes is itself a jump table — but it defeats inlining and costs a call frame per opcode. | |
| 8 | **GC = Go's GC** (`collectgarbage` is literally `runtime.GC()`, `baselib.go:67`). Fine for pauses; the tax is *allocation rate* — number boxes, table internals, concat buffers. | |
| 9 | Hot-path misc: `mainLoopWithContext` does a `select` on `ctx.Done()` **per instruction** (`vm.go:55`); `LNumber.String()` goes through `fmt.Sprint` (`value.go:114`); `stringConcat` builds a `[]string` then joins. | |

For calibration: upstream gopher-lua benches ~3–10× slower than C Lua 5.1 and ~15–40× slower than LuaJIT on numeric benchmarks. This fork's patches claw back maybe 1.2–1.5× of that. All numbers below are estimates — validate against actual scripts with `pprof`/`benchstat` before committing to anything.

---

## 2. Tier 1 — Interpreter work (days to weeks each)

**2a. Kill per-write register bookkeeping.** At frame setup you already know `NumUsedRegisters`; slice the window once per frame — `frame := rg.array[lbase : lbase+numregs]` — and have handlers do `frame[A] = v` with no `checkSize`, no `top` maintenance, and (bonus) bounds checks the compiler can actually eliminate, plus no `lbase` add per operand. The only places `top` genuinely changes are vararg calls, `MultRet` returns, and `SETLIST` — handle those explicitly. **Effort: 1–2 weeks** touching every handler in `_vm.go` (the go-inline pipeline is already set up). **Payoff: 10–25%.**

**2b. Rewrite the table engine.** Concretely: (i) replace `keys`/`k2i` with an intrusive insertion-order link in the entry itself (order-preserving dict, one map op per store instead of two); (ii) for string keys, store a precomputed 32-bit hash in the entry and use your own open-addressing table (or a Swiss-table scheme) instead of `map[string]LValue` — this eliminates re-hashing and gives you Lua-style `next()` for free; (iii) small-table fast path: tables with ≤8 hash slots use a linear array with (len, first-byte) prefilter — this beats Go's map by 2–5× at that size, which is where most Redis-script tables live; (iv) inline the `Metatable == LNil` check into `getField`/`setField` before calling `metaOp1`. **Effort: 2–4 weeks** (`table.go` is nicely self-contained; `Next()` is the fiddly part). **Payoff: 1.5–3× on hash-heavy code, plus large allocation reduction.**

**2c. String interning.** Intern at compile time (constants — `stringConstants` already exists) and on table-key store; keep a per-VM `map[string]string` pool. Then equality on interned strings becomes a pointer compare of the data words (`reflect.StringHeader.Data` via unsafe — the same class of trick `alloc.go` already uses), and table hashes are free. **Effort: ~1 week. Payoff: 5–15%, and it's a prerequisite for doing fast dispatch on string keys later.**

**2d. Superinstructions.** There are 23 free opcode slots (6-bit opcode, 41 used). The high-value fusions in Lua 5.1 codegen: compare+jump (`EQ/LT/LE` is always followed by `JMP` — one dispatch saved per test), `arith` with a constant operand (skip the RK test), `LOADK`+`SETTABLE`, `GETGLOBAL`+`MOVE`. Note `OP_MOVEN` already fuses the common multi-return case. **Effort: 2–3 weeks across `compile.go` and `_vm.go`. Payoff: 10–20%.**

**2e. Cheap hits, days each:** throttle the context `select` to every 32nd instruction or a countdown byte (Redis's busy-script deadline doesn't need per-instruction resolution); `LNumber.String()` via `strconv.AppendFloat` on a stack buffer; `stringConcat` with a single pre-sized `strings.Builder`. Also profile whether the jump table still beats a switch once 2a lands — with bookkeeping gone, the switch's inlining advantage may flip it, and it certainly matters for the JIT decision later.

**Tier-1 total: roughly 6–10 weeks for ~1.7–2.5× combined.**

---

## 3. Tier 2 — Value representation (the fork-level decision, ~2× by itself)

The interface `LValue` is the single structural bottleneck. Options, with the Go-specific constraints you must design around:

| Representation | Size | Type test | GC story | Notes |
|---|---|---|---|---|
| Interface (current) | 16B + box | itab load+cmp | free | Boxed floats even with the arena; `alloc.go`'s trick already leans on Go's GC being non-moving |
| **Tagged struct** `struct{ tag uint8; num float64; ptr unsafe.Pointer }` | 24B | one byte cmp | **free and safe** — `unsafe.Pointer` fields are scanned by Go's GC | Numbers unboxed in registers/tables; no allocation on arith. The pragmatic choice |
| NaN-boxing in `uint64` | 8B | tag bits | **unsafe** — a pointer bit-pattern in a `[]uint64` is invisible to GC; needs a pin arena like `alloc.go`'s `fptrs` (works only while Go's GC stays non-moving) | Fastest slots, but every pointer store/load is `unsafe` packing; fine on 64-bit only |
| 32-bit handles into an object pool | 8B | tag bits | free (pool is one rooted slice) | Adds an indirection per object access, but this is the representation a JIT wants (see §5) |

For a pure-Go interpreter the **tagged struct** is the recommendation: it's mechanical but pervasive (every `LValue` in `value.go`, `vm.go`, tables, `auxlib.go`), you control the embedding so you can keep a thin `LValue`-ish facade at the host boundary, and it's the layout the JIT tier will want anyway. **Effort: 4–8 weeks. Payoff: ~2× on numeric code plus a large GC-pressure drop.**

A taste of why: `OP_FORLOOP` today (`vm.go:1792-1845`) does three interface asserts, a float load through a pointer, an add, two `LNumber2I` boxes, and two top-maintenance checks. With a tagged struct it's: load tag, load num, addsd, store num — four instructions.

---

## 4. Tier 3 — Exploiting known types (the Redis angle)

This is where "I know the datatypes" pays, and Redis's execution model is unusually favorable: **scripts arrive once, are cached, and run thousands of times** — so per-script compilation/specialization cost is fully amortizable, and the workloads are extremely monomorphic.

- **Typed table kinds.** An `LTable` variant whose array part is `[]float64` (or the hash part `map[string]string`), generalizing lazily to the generic representation on first non-conforming store. `cjson.decode` and the `redis.call` reply conversion emit these directly — Redis replies are strings/numbers/nested arrays in practice, so the lazy generalization almost never fires. **Effort: 2–3 weeks.**
- **Inline caches.** A side table parallel to `Code` (`[]site`), one entry per `GETTABLE`/`SETTABLE`/arith site, caching the last key/hash + shape ("string table, array hit", "float+float"). On hit, one compare; on miss, generic path + update. Given Redis's monomorphism, hit rates will be ~99%. **Effort: 3–4 weeks. Payoff: 1.3–2× on top of Tier 1/2.**
- **Profile-then-specialize at bytecode level.** Run the script in the interpreter once (or a few hundred iterations), record per-site type profiles, then run a rewrite pass over the bytecode substituting specialized opcodes (`ADD_FF`, `GETTABLE_SSTR_ARR`, …) with guard-and-bail to the generic opcode. This is an "AOT JIT at the bytecode level" — all the type-feedback payoff, none of the machine-code machinery, fully debuggable. For an experienced compiler dev: **1–2 months**, and it's the natural prototype for the real JIT's feedback interface.
- Since you own the host, you can also skip Lua tables entirely on the data path: expose Redis values as userdata with typed accessors rather than materializing `LTable`s for `KEYS`/`ARGV`/replies.

---

## 5. The JIT — what it actually takes on ARM64 + x86-64 in Go

Five viable architectures. The requirements analysis differs sharply, so pick explicitly:

### Option A — Pure-Go machine-code JIT (the "wazero" model)

Generate machine code into `mmap`'d pages from Go, enter it via an `unsafe` function pointer. Existence proof: wazero does exactly this for wasm on linux/amd64 and linux/arm64, no cgo, in production. This keeps the static pure-Go binary, which is presumably why you're writing Redis in Go.

Requirements, in order of how much they'll hurt:

1. **GC visibility — the central problem.** Go's GC is precise; a Lua object pointer held only in a native register or a JIT frame while *any* goroutine triggers a concurrent mark = freed object under you. Three rules make it safe:
   - **Shadow stack:** all live reference values live in the existing `registry` array (`[]LValue` is already GC-scanned and rooted from `LState`). JIT code may hold pointers in registers only across straight-line sequences containing no calls and no allocation; at every **safepoint** the shadow stack is the complete root set. Conveniently, the interpreter's frame layout already gives you this — deopt becomes trivially correct for free (see 5).
   - **Write barriers:** any store of a pointer into a Go heap object (table slot, upvalue, registry) must go through a barrier. Don't try to inline `runtime.gcWriteBarrier` — it's not a stable ABI. Route pointer stores through small Go helper functions (the Go compiler emits the barrier) and keep the helper in the fast path only when a tag says "pointer"; number stores with the tagged-struct representation are plain stores and barrier-free, which is most of a Redis script's traffic.
   - **Safepoints in loops:** a JIT'd loop containing zero Go calls would stall stop-the-world forever (Go's async preemption declines to unwind non-Go frames). Emit a counter check on every back-edge that calls a tiny `//go:nosplit` Go function every N iterations. Same hook serves the busy-script 5-second deadline, replacing the per-instruction `select`.
2. **The goroutine stack problem.** JIT frames living on the goroutine stack break stack growth (morestack copies the stack by walking Go frames; a JIT return PC has no unwind info). Two clean answers: (a) run JIT'd Lua on its **own mmap'd stack**, switched by a small Go-asm trampoline, with all re-entry into Go funneled through trampolines that guarantee headroom; or (b) study how wazero bridges this and copy its discipline. Design for (a) — it also gives you a place to put per-thread JIT state.
3. **Two code emitters.** `golang.org/x/arch` is disassembly-only; hand-roll the encoders (wazero did). The instruction inventory for a baseline JIT is small — ~50 forms: mov imm/reg/mem(+offset), float and int add/sub/mul/div, cmp+cond branch, load/store, call imm/reg, and the arm64 `IC IVAU` dance for icache flushing (amd64 is coherent; arm64 needs explicit per-line flush after code write, doable from a tiny asm helper).
4. **Page permissions.** Linux: `mmap` PROT_READ|WRITE, `mprotect` to PROT_READ|EXEC after emitting (proper W^X), or accept RWX if the deployment allows. macOS: `MAP_JIT` + `pthread_jit_write_protect_np` toggling, mandatory on Apple Silicon — and Apple has said RWX forks will get harder, so budget for it if dev-machine support is wanted.
5. **Compile strategy — and here Redis is your friend.** You don't need tiering or hot-loop counters: scripts are registered once (`EVALSHA`) and run forever. Compile whole functions **eagerly at script load**, on a worker goroutine, while the interpreter runs it the first time; swap in the compiled code pointer with an atomic write. Baseline JIT, not trace JIT.
6. **The design that minimizes risk:** per-opcode compilation with guarded fast paths and slow paths that **call the existing interpreter helpers** (`getField`, `objectArith`, `pushCallFrame`…). You inherit the interpreter's semantics for everything uncommon; the JIT only accelerates the ~15 opcodes that matter (arith, compare, `GETTABLE`/`SETTABLE` fast paths, `FORLOOP`, `MOVE`/`LOADK`, `EQ`, calls). With the shadow-stack-always-current discipline, **deoptimization is a jump back to the interpreter at the next instruction boundary with zero state repair** — a genuinely simpler deopt story than most JITs.

**Effort: 3–5 months full-time** for production quality on both architectures (2–3 months to correct-and-benching on amd64, +3–4 weeks for arm64 if the emitter interface is clean, +2–4 weeks hardening). **Expected: 2–5× over the tuned interpreter from Tier 1/2**, landing within ~1.5–3× of C Lua 5.1. Still behind LuaJIT — it has a trace compiler, escape-analyzed allocations, and a hand-typed C runtime — but fast enough that script execution stops being the Redis bottleneck relative to network and storage.

### Option B — Off-heap Lua world + handles

Represent all Lua objects in your own `mmap` heap with 32-bit handle references; JIT code then touches no Go pointers at all, and the entire GC hazard class (barriers, safepoint spills, preemption) evaporates — you own a simple mark-sweep with safepoints you control. The cost: you're now writing a Lua runtime (allocator, GC, string interning, tables) rather than adapting gopher-lua, plus every host boundary crossing (`redis.call`, 100% of your scripts' interesting work) is a handle↔Go-value conversion. **+1–2 months over Option A**, but it's the cleanest JIT semantics and the fastest steady-state. For a compiler-experienced developer embedding into your own system, this is the "build RedisLuaJIT properly" path.

### Option C — cgo LuaJIT

One cgo call per script execution, cgo callbacks up into Go per `redis.call` — cgo overhead is now tens of ns per crossing, so ~µs total tax per script against LuaJIT's 10–50× speed advantage. Costs: `LockOSThread` per script (LuaJIT state isn't thread-safe and cgo callbacks have goroutine-affinity rules), losing the static pure-Go binary, signal-handler coexistence. If "pure Go" isn't actually sacred, this is the max-perf-per-effort answer by an order of magnitude — **~2 weeks** to a working embed. Worth an honest look before spending 5 months on Option A.

### Option D — Lua → Go source → compiled, dynamically loaded (`plugin`)

Go's literal analog to `dlopen` is the `plugin` package: generate Go per script, `go build -buildmode=plugin -o scripts/<sha>.so`, then `plugin.Open` + `plugin.Lookup("Run")`; after one type assertion, calls are ordinary Go calls, so per-call overhead is negligible. All the pain is in the lifecycle:

| Constraint | Consequence for a Redis-style embed |
|---|---|
| Host and plugin must share the **exact Go toolchain version, build flags, and versions of shared packages** | Runtime load failures with notoriously cryptic errors — the "ABI" is the entire build environment |
| **No unload** (there is no `dlclose`) | Scripts are SHA-keyed so redefinition gets a new SHA, but `SCRIPT FLUSH` or script churn leaks every .so permanently; hostile clients generating unbounded distinct scripts = unbounded leak. Needs a hotness threshold + cap before compiling |
| Ships the **full Go toolchain** with the server and forks `go build` per script | ~50–300ms per script (build cache helps), a large toolchain in the container, and a compiler-server surface in a network-facing daemon |
| Platforms | linux/amd64 solid; darwin supported but rough (golang/go#58826, golang/go#63401); **Windows: never**; no cross-compiling plugins |
| Effectively frozen | The Go team has left `plugin` in maintenance; no `dlclose` or Windows coming — don't build a product on it |

Related dead ends: `-buildmode=c-shared` + real `dlopen` via cgo gets `dlclose` but embeds a **second complete Go runtime** per .so (two schedulers, two GCs, pass-copies-only pointer rules between the heaps) — strictly worse than `plugin` in-process. A **subprocess per hot script** (compile to a standalone binary, Unix socket or shared-memory ring) buys crash containment and real OS isolation for untrusted scripts at ~5–20µs IPC per `redis.call` — keep it as a possible sandboxing tier, not for speed.

**Verdict:** days to wire; fine as a dev-machine calibration of what compiled speed buys; hostile to production deployment.

### Option E — Lua → wasm → wazero (the "compile & load" route that actually fits)

Reframes "Lua → Go → compile → run" as **Lua → wasm → JIT → run**, and maps directly onto Option B (off-heap + handles):

- **Emit a `.wasm` module per script.** The wasm binary format is trivial (LEB128 + a handful of sections); for a backend writer the emitter is days of work — and it is **one backend instead of amd64 + arm64**.
- **wazero** (pure Go, zero dependencies, Apache-2.0) does `CompileModule` + `Instantiate` with a **native JIT on exactly linux/amd64 and linux/arm64**, interpreter fallback elsewhere. It is the existence proof for Option A's mechanics — using it means its mmap/codegen/stack handling comes for free.
- **Linear memory is the off-heap Lua world.** Lua heap = bytes, all references = 32-bit handles. Every hazard from Option A — shadow-stack spills, write barriers, back-edge safepoints, the goroutine-stack/morestack problem — **disappears**, because JIT'd code never touches a Go pointer and Go's GC never scans the Lua heap. A simple bump/mark-sweep allocator inside linear memory replaces it (which Option B needed anyway).
- **`redis.call` is a host-module import** — a plain Go function wazero trampolines; strings/numbers cross by copy, the same conversion any boundary pays.
- **Lifecycle is everything `plugin` isn't:** ~ms per compile, nothing extra shipped at runtime, no `fork`/`exec` of a compiler (safe against untrusted script input — you run your own codegen, not `go build`), modules are closable (`Close()` — no `SCRIPT FLUSH` leak), and the wasm 1.0/2.0 spec surface is frozen, so dependency churn risk is low.
- **Performance:** wazero is a baseline JIT — expect ~1.5–3× of hand-native for compute-bound loops, the same band Option A promised for a fraction of the effort. The Lua frontend (register allocation → wasm locals, effectively SSA) does the real work.

Dynamic-load mechanisms compared:

| | `plugin` | c-shared + `dlopen` | subprocess | **wasm + wazero** |
|---|---|---|---|---|
| Ship at runtime | Go toolchain | Go toolchain | Go toolchain | nothing |
| Per-script compile | ~50–300ms | same | same | ~ms |
| Call overhead | Go call | cgo | ~µs IPC | host-import call |
| Unload | never | `dlclose` (dual runtime) | kill | yes |
| GC hazards | none (shared runtime) | dual-runtime pointer rules | none | none (off-heap) |
| Platforms | linux solid, darwin rough, no windows | cgo matrix | everywhere | **amd64+arm64 JIT, rest interpreted** |
| Effort to wire | days | ~1wk | ~1wk | ~1–2wk + your wasm backend |

**Verdict: the default recommendation for the compile-and-load tier in production** — Option B's semantics with the machine-code tier outsourced, and the safest choice for untrusted client scripts on a public port.

---

## 6. Suggested order of attack

1. **Week 0:** benchmark *your* candidate scripts (not fannkuch) — string ops, cjson decode, table building, a `redis.call`-heavy loop — and pprof them. This reorders Tiers 1–3 with data.
2. **Tier 1** (bookkeeping, tables, interning, cheap hits): biggest ratio of gain to risk, and none of it is wasted by later tiers.
3. **Tier 2 (tagged struct)** before any JIT work — the JIT's fast paths want a fixed value layout, and this is where you decide the handle-vs-pointer question that determines JIT architecture A vs B.
4. **Tier 3 (ICs / bytecode specialization)** — this *is* the JIT prototype: the feedback infrastructure, guard discipline, and deopt mechanism all carry forward.
5. Then the machine-code tier: **Option E (wasm + wazero) is the default** — Option A only if wazero's baseline band proves insufficient on your workloads, and Option C as a two-week calibration experiment to measure what LuaJIT-class speed actually buys before committing months anywhere.

Realistic combined landing point: **~10–30× over today's fork** (Tier 1 ≈ 2×, Tier 2 ≈ 2×, JIT ≈ 2.5–5× on top), i.e., roughly C Lua 5.1 territory, in a pure-Go binary — with the type-specialization work (`[]float64` tables, string-keyed fast paths) pushing Redis-shaped scripts further than general benchmarks would suggest.

---

## 7. Option E detailed design — the Lua→wasm compiler

> **Full engineering spec:** `docs/Lua-Wasm-Design-and-Test-Plan.md` — module/ABI design, complete opcode-lowering table, linear-memory map, the differential test regime (3 oracles, 8 test layers), and milestone-by-milestone plan with test gates. This section is the summary.

What follows is a grounded analysis of what building E takes, based on this tree: what gets reused, what gets replaced, the target architecture, the central design decisions, a phased plan, and the risks.

### 7.1 What this codebase gives you for free (the hard half of a compiler project)

| Component | Location | Role on the wasm path |
|---|---|---|
| Lexer + parser | `parse/lexer.go:468` `Parse(reader, name) → []ast.Stmt` (goyacc grammar in `parser.go.y`) | Reused untouched |
| AST | `ast/` | Reused untouched |
| AST→bytecode compiler | `compile.go:1849` `Compile(chunk, name) → *FunctionProto` | Reused untouched — all the hard frontend semantics (scoping, upvalue capture analysis, vararg shuffling, constant folding) are already correct here |
| Bytecode IR | `FunctionProto` (`function.go:25`): 41-opcode Lua 5.1 register ISA, `NumUsedRegisters`, upvalue descriptors, nested `FunctionPrototypes`, `DbgSourcePositions` (pc→line) | This is the backend's input — a small, stable register IR |
| Conformance suites | `_lua5.1-tests` (official Lua test suite), `_glua-tests` | Validation harness for the backend |
| Execution runtime | `vm.go`/`_vm.go`, `state.go`, `table.go`, `alloc.go`, `value.go`'s `LValue` | **Not reused** — replaced by emitted wasm + the runtime module. (The interpreter path keeps them; the two share only the frontend.) |

### 7.2 Target architecture: three layers

```
┌─────────────────────────────────────────────────────────────┐
│ Host (Go, wazero) — redis.call, deadline flag, reply        │
│ conversion, script cache, instantiation policy              │
└───────────────▲─────────────────────────────▲───────────────┘
                │ host imports (wazero host   │ instantiate/
                │ module: ptr+len in linear   │ call exports
                │ memory)                     │
┌───────────────┴──────────────┐ ┌────────────┴────────────────┐
│ script.wasm (your compiler)  │ │ runtime.wasm (precompiled   │
│ one wasm function per proto; │ │ blob, shipped with server): │
│ inline fast paths; calls →   │ │ tables, strings+interning,  │
│ runtime for everything else  │ │ allocator, GC, metamethods, │
└──────────────────────────────┘ │ varargs, stdlib             │
                                 └─────────────────────────────┘
```

**Where the runtime comes from — the make-or-break decision.** Recommended: **adapt the Lua 5.1 C runtime** (`ltable.c`, `lstring.c`, `lgc.c`, `lapi`-subset, `lmem`, minus `lvm.c`'s interpreter) and compile it once with `wasi-sdk`/clang to freestanding `wasm32`. The framing that makes this tractable: *your backend is a code generator that replaces `lvm.c` while calling the rest of the runtime* — structurally the same move LuaJIT made. ~10–15k lines of MIT-licensed, battle-tested semantics; the port work is bounded (no libc, replace `setjmp/longjmp` with an error flag, define the export ABI your backend calls). Alternatives: hand-emit the runtime as wasm (full control, much more work — only worth it for a later optimizing tier), or compile gopher-lua itself with `GOOS=wasip1` (drags a whole second Go runtime along; useful only as a week-1 baseline measurement, not the end state).

### 7.3 Central design decisions

**Value representation.** A Lua value is a tagged tuple in wasm: `(tag i32, num f64, ref i32)`. `ref` is a 32-bit offset/handle into your object heap in linear memory. This is Option B's representation, and inside wasm it is hazard-free: your GC, your rules (NaN-boxing in heap slots later becomes legal, since only *your* collector ever decodes them).

**Frame layout — the one subtle interaction.** Naively, Lua registers map to wasm locals (engine-register-allocated, free access). But *open upvalues* must point at live registers, and wasm locals have no address. Resolve with a static split the proto already tells you: `compile.go`'s upvalue descriptors identify exactly which registers a nested `OP_CLOSURE` captures → **captured registers live in the linear-memory frame slot array (addressable by upvalue objects); uncaptured registers live in wasm locals.** Frames form a linked list in linear memory.

**GC roots without stack maps.** Allocation and GC can only happen inside runtime calls. Therefore: at every call site, spill ref-valued locals into the linear-memory frame (numbers/bools/nil never need spilling — not roots). Root set = linked frame list + globals table + registry. This is Option A's shadow-stack-at-safepoints discipline, but with zero hazard because nothing is visible to Go's GC at all.

**Calls.** Each Lua proto → one wasm function. `OP_CALL` to a compile-time-known local function → direct `call` (wazero devirtualizes within-module direct calls well); everything else → runtime `lua_call` dispatch on the function handle. **Tail calls:** don't depend on the wasm tail-call proposal — emit the trampoline pattern (return a call-descriptor to a small outer loop) so `return f(x)` recursion is O(1) stack, as Lua requires. **Varargs:** count + pointer to a linear-memory arg area; `OP_VARARG` copies. **Errors/pcall:** no wasm exceptions in wazero → error-flag propagation: runtime sets a global error object; every wasm call site checks the flag and early-returns; `pcall` saves/restores frame state around the call. Cost: one branch per call.

**The Redis-specific GC simplification.** Redis scripts are stateless between runs. Run each execution on a fresh arena: **bump-allocate, reset the arena at end of run — no GC at all** unless a single run allocates past a threshold (then mark-sweep kicks in, roots as above). Most scripts never trigger collection.

**Debug info.** Ship `DbgSourcePositions` as a pc→line side table (exported function or parallel array) so error messages match the interpreter's.

### 7.4 Fast-path inventory (where the speed comes from)

| Opcode class | Inline fast path | Fallback |
|---|---|---|
| `MOVE`, `LOADK`, `LOADNIL`, `LOADBOOL`, `TEST*`, `JMP`, `FORLOOP`/`FORPREP` | pure inline, never calls runtime; `FORPREP` guards all three are numbers once, then the loop is raw f64 adds/compares | conversion/metamethod error path |
| arith (`ADD`…`UNM`) | both tags = number → f64 op | runtime call (string→number coercion, metamethods) |
| `EQ`/`LT`/`LE` | number×number → f64 compare; interned-string×interned-string → single `i32` compare | runtime `equals`/`lessthan` |
| `GETTABLE`/`SETTABLE` | table tag + number key in array range → inline `i32.load`/`store` with bounds check; (v2: per-site inline-cache slot in linear memory — Tier 3's IC design drops in here unchanged) | runtime table get/set (`__index`/`__newindex`) |
| `CALL`/`TAILCALL`/`RETURN`, `CONCAT`, `LEN`, `SETLIST`, `CLOSURE`, `GETGLOBAL`/`SETGLOBAL`, `SELF` | — | runtime calls (globals are just a table get on a handle) |

### 7.5 Host boundary (Redis specifics)

- `redis.call` as a wazero host-module function: args passed as `(ptr, len)` in linear memory; host reads via `api.Module.Memory()`, writes results into a buffer allocated by an exported `alloc`. Per-call cost ~100–300ns + copies — invisible next to a µs-scale Redis command.
- **Deadline:** host writes a flag checked at loop back-edges — replaces the per-instruction `ctx` `select` from §1.
- **Per-run isolation:** one compiled module per script (cached by SHA); instantiate per execution, or reuse the instance and reset the arena + restore a globals snapshot (wazero instantiation is µs–low-ms).
- **Determinism:** host-seeded `math.random`, no clock — Redis's reproducibility requirement.

### 7.6 The portability kicker

Once the compiler exists, the `.wasm` artifact is host-agnostic: it runs on **wazero** (Go hosts — your Redis), **wasmtime/wasmer** (C hosts), **browsers/Node** (JS embedding via custom imports or a WASI shim), **edge runtimes** (Cloudflare Workers-class), and embedded interpreters (**WAMR, wasm3**) as a slow-but-everywhere fallback. Only the host-import surface varies (`redis.*` vs stdlib shims). One artifact, N hosts — "Lua everywhere" falls out of E as a side effect, which is exactly the general-purpose tool mentioned above.

### 7.7 Phased plan and effort (experienced compiler dev)

| Phase | Work | Effort |
|---|---|---|
| 0 — spike | Hand-rolled `.wasm` emitter (module/type/code/export sections, LEB128 — days of work); compile toy protos (`return a+b`); run on wazero; also measure the `GOOS=wasip1` gopher-lua baseline for reference | ~1 wk |
| 1 — runtime | C Lua 5.1 runtime → freestanding wasm32 (wasi-sdk); error-flag longjmp replacement; export ABI co-designed with 7.4; bump allocator; frames-as-roots mark-sweep (used only past the arena threshold) | 3–6 wks |
| 2 — backend | Full 41-opcode coverage over `FunctionProto`; captured/uncaptured register split; calls/tailcall trampoline/closures/varargs; error propagation; pc→line tables | 4–8 wks |
| 3 — conformance & perf | `_lua5.1-tests` + `_glua-tests` in a wasm harness; profile against the native gopher-lua fork and C Lua | 3–4 wks |
| 4 — Redis integration | host module, reply→Lua conversion (emit Tier 3's typed tables here), script cache, instantiation policy, deadline checks, `SCRIPT FLUSH` | 2–3 wks |

**Total ≈ 3–5 months solo** for the Redis subset (no coroutines, no `os`/`io`, deterministic libs). The general "Lua on every platform" tool adds: **coroutines** (the hard one — stackful coroutines need Asyncify instrumentation (Binaryen at build time), CPS emission, or one-wasm-instance-per-coroutine: +3–6 wks), `os`/`io` via WASI (+1–2 wks), full stdlib incl. `string.format` (+2–4 wks).

### 7.8 Risks

- **wazero maintenance** — small maintainer team historically; Apache-2.0 and forkable, and your emitted module is portable so the host is swappable (wasmtime via cgo as escape hatch). Risk contained, but pin the version.
- **Wasm stack depth** — deep non-tail Lua recursion traps at the engine's stack limit; enforce a Lua-level recursion cap (Redis wants one anyway).
- **f64-only arithmetic** — correct for Lua 5.1 semantics (matches this fork); Lua 5.3+ integer semantics would be a frontend change, out of scope.
- **GC pauses** — your collector, your tuning; the arena-reset model means most Redis runs never collect at all.
- **Two implementations to keep honest** — interpreter and wasm path share the frontend, so divergence risk is confined to runtime semantics; the in-tree conformance suites are the regression gate.

### 7.9 Expected performance

Fast paths (arith, comparisons, locals, numeric loops) compile to near-native f64/int code with no dispatch; table/string/metamethod traffic runs at C-in-wasm speed (~1–2× native C). Overall expectation: **2–5× over today's fork, roughly C Lua 5.1 territory (0.7–1.5×)**. Startup: module compile ~ms once per script (SHA-cached); instantiation µs–low-ms per run. Combined with Tier 1/3 typed-table work at the boundary, Redis-shaped scripts should land at the fast end of that band.

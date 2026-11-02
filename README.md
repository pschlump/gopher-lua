# gopher-lua (pschlump fork) — Lua 5.5.1 in pure Go

A **Lua 5.5.1 virtual machine, compiler, and standard library in pure
Go** — byte-exact with stock C Lua 5.5.1 (error messages, semantics,
numeral formatting, observable table-iteration order), no cgo, built
as the embedded scripting engine for [Ultima](../ultima), a pure-Go
Redis clone (EVAL/SCRIPT). It runs as a single static Go binary on
every platform Go supports.

## Credit where credit is due

This repo is a fork of **[gopher-lua](https://github.com/yuin/gopher-lua)**
by Yusuke Inuzuka — a capable Lua VM and compiler in Go (implementing
Lua 5.1). The parser, the register-VM interpreter, the code generator,
and the Go embedding API all originate there. **Without gopher-lua,
this fork would not exist.** Thank you, yuin.

The fork lineage runs through **[glasm](../glasm)**, which upgraded
the language core in place from 5.2 through **Lua 5.5.1** semantics and
gated it byte-exact against stock C Lua 5.5.1; this repo was carved
from glasm (`0affa95`, 2026-11-01) as the standalone native engine.
It builds on that foundation with:

- the full **Lua 5.5.1** language surface — stock 5.5.1 instruction
  encoding, `_ENV`, integer/float duality, goto, `<const>`/`<close>`,
  the 5.3+ libraries — differentially gated against stock C Lua 5.5.1
  (58-case golden corpus, byte-exact output);
- the daemon-facing **`host/` package**: untrusted scripts with memory
  budgets, deadlines and kill, sandboxed globals, Go host functions,
  and a warm-VM pool — the Redis-EVAL embedding posture (below);
- a measured performance program (table engine, call path, boxing) —
  see [How about performance?](#how-about-performance).

## Table of Contents

- [Design principle](#design-principle)
- [How about performance?](#how-about-performance)
- [Installation](#installation)
- [Usage](#usage)
- [Embedding in a server: the `host/` package](#embedding-in-a-server-the-host-package)
- [Data model](#data-model)
- [Callstack & Registry size](#callstack--registry-size)
- [Differences between Lua and gopher-lua](#differences-between-lua-and-gopher-lua)
- [Standalone interpreter](#standalone-interpreter)
- [Build & test](#build--test)
- [License](#license)

## Design principle

- **Be a scripting language with extensible semantics**, with a
  user-friendly Go API. The API is object-based, not the stack-based
  API of the C implementation.
- **Byte-exactness with stock C Lua 5.5.1 is the contract.** Stock is
  the spec: error strings, `tostring` formatting, iteration order —
  any divergence is a ledgered row with a covering test, not an
  accident.
- **Pure Go, no cgo.** The engine embeds in any Go daemon (Ultima's
  EVAL, an HTTP service) as plain library code: static binaries,
  per-VM memory budgets, and per-instruction preemption — none of
  which a cgo-bound C Lua can give a Go host without a process
  boundary.
- `LValue` is an `interface{}` — the public API. Consumers
  pattern-match on it (`lv.(*LFunction)`, `v.(LInteger)`); embedding
  friendliness outranks the last few percent of interpreter speed.

## How about performance?

All numbers below are from one Apple M4 Max (arm64, go1.27.0) in one
2026-11-02 session, tree at the P6 perf milestone; the C reference is
**stock C Lua 5.5.1** built `-O2` (`clang 21`). Full methodology and
raw runs: [docs/Perf-P0-Baseline.md](docs/Perf-P0-Baseline.md) (the
same-session C calibration), [docs/Perf-P6-Boxing.md](docs/Perf-P6-Boxing.md)
(engine numbers), and [docs/Embedded-EVAL-Perf.md](docs/Embedded-EVAL-Perf.md)
(the embedded suite). The meaningful column is always the *ratio* —
absolute numbers move with the machine. (`cmd/luabench` and
`cmd/embedcmp` reproduce any of it.)

### vs the native C Lua 5.5.1 interpreter

Benchmark scripts (`cmd/luabench`), warm steady state. The C leg is
the `lua` CLI re-`loadfile`ing per run — the best its CLI can do,
since it has no pooling surface; the gopher-lua leg is the daemon
posture (pooled VM, live cancelable context):

| script (shape)                      | C Lua 5.5.1 | gopher-lua | ×C    |
|-------------------------------------|-------------|------------|-------|
| h_tiny (startup only)               | 14.0 µs     | **1.8 µs** | **0.13×** |
| h_concat (concat-heavy)             | 620 µs      | **470 µs** | **0.76×** |
| h_str (short string ops)            | 36 µs       | 57 µs      | 1.6×  |
| h_tablechurn (table alloc churn)    | 3.9 ms      | 7.4 ms     | 1.9×  |
| h_strlong (150 KB text processing)  | 387 µs      | 1.0 ms     | 2.6×  |
| h_tco (tail-recursive countdown)    | 1.6 ms      | 8.9 ms     | 5.5×  |
| h_fib(30) (call-heavy recursion)    | 20.2 ms     | 119 ms     | 5.9×  |
| h_callret (call-heavy)              | 2.4 ms      | 15.0 ms    | 6.2×  |
| h_nsieve (numeric + table stores)   | 6.0 ms      | 58 ms      | 9.7×  |
| h_numloop (4 M-iteration numeric)   | 8.5 ms      | 99 ms      | 11.7× |

The shape is the story. Where the work is setup-shaped (compile a
small script, run it, return) a pooled gopher-lua VM **beats the C
CLI** — it re-runs an already-compiled chunk while C re-parses. Where
the work is per-instruction compute, `-O2` C's dispatch wins: string
and table lanes within ~2×, call-shaped lanes ~6×, tight numeric
boxing-heavy loops ~12× (a 5.1→11.7× improvement across this fork's
perf program; numloop was 22.2×C at its baseline).

### Embedded usage — the EVAL shape (`cmd/embedcmp`)

The table above flatters C: a daemon does not shell out to `lua`. The
fair comparison embeds C Lua through **its own C API** the way Redis
does — one `lua_State`, chunk loaded once, fresh KEYS/ARGV staged per
call — and races it against `host/` over the same scripts with
byte-identical staged arguments and cross-checked results (zero
mismatches across the suite):

| script (short EVAL-shape) | C, state reuse | C, fresh state | gopher-lua, fresh VM | gopher-lua, pooled VM | pooled ×C |
|--------------------------|----------------|----------------|----------------------|-----------------------|-----------|
| e_arg (arg arithmetic)       | 0.4 µs  | 15.4 µs | 13.8 µs  | 2.7 µs  | 7.2×  |
| e_sum (KEYS aggregation)     | 0.8 µs  | 17.0 µs | 15.8 µs  | 4.6 µs  | 6.1×  |
| e_tag (reply array build)    | 1.3 µs  | 17.3 µs | 16.4 µs  | 5.4 µs  | 4.0×  |
| e_norm (string normalize)    | 0.5 µs  | 16.6 µs | 18.2 µs  | 7.2 µs  | 15.8× |
| e_hash (40-field hash loop)  | 5.4 µs  | 23.5 µs | 22.5 µs  | 11.9 µs | 2.2×  |
| e_call (200 Go-fn crossings) | 14.8 µs | 38.7 µs | 52.6 µs  | 44.3 µs | 3.0×  |
| e_sort (500-element sort)    | 44.7 µs | 58.8 µs | 142.0 µs | 132.6 µs| 3.0×  |

Reading it:

- Against the state-reuse embedder, scripts that do real work run at
  **2–3× C**; the residual on trivial scripts is a fixed ~2.6 µs
  per-run floor (pool dispatch + KEYS/ARGV staging + result
  collection) against C's ~0.2 µs, not per-instruction slowness.
- A host-function crossing costs **~0.21 µs** (vs C's ~72 ns through
  its C API): a Go closure, argument conversion, no wire.
- Fresh-VM-per-run — the maximum-isolation posture, no shared guest
  state at all — lands at 13.8–18.2 µs on trivial scripts, **on par
  with C's own newstate-per-run** (and a fresh VM here is a µs-scale
  `LState` build, not a process).
- First run after daemon start: 33–186 µs in-process; compile is
  32–41 µs vs C's 8–10 µs loadbuffer (the known parser gap — it is a
  real parse, no cache tier, paid once per script).

### Memory utilization

Peak process RSS over the same embedded suite (one Lua state / VM in
the loop):

| posture              | C Lua 5.5.1 | gopher-lua |
|----------------------|-------------|------------|
| one-shot process     | 2.0–2.1 MB  | 7.2–7.6 MB |
| warm loop            | 2.1–2.5 MB  | 8.2–8.8 MB |
| live heap after loop | ~20 KB Lua heap | ~0.5 MB Go heap |

The ~4× RSS is the Go runtime floor plus one `LState` and the
standard library tables — it is a **process** number, not a per-VM
multiplier: the runtime is shared, and each additional VM costs its
LState + library tables (sub-megabyte). What the 4× buys a daemon:

- **no cgo** — one static binary, every platform Go targets, no C
  allocator arenas next to the Go heap;
- **per-VM memory budgets** — `WithMemoryBudgetBytes` charges every
  allocation seam (table growth, string builders) against a monotone
  accounted counter and raises a catchable `not enough memory` past
  the cap; `vm.UsedBytes()` reports the charge;
- **per-instruction preemption** — a deadline or `Kill()` stops the
  script at the very next instruction boundary (a C interpreter can
  only be made cooperative, never preempted, from a Go host);
- coroutines are goroutines, with a per-VM cap.

C Lua is the right choice for a C daemon. For a **Go** daemon, the
comparison is not "8.8 MB vs 2.3 MB" but "8.8 MB in-process with
budgets and kill" vs "a subprocess C Lua per script (or a cgo seam
with none of it)".

### The perf program

These numbers are the product of a measured, milestone-gated program
(P0–P6, [docs/Option-A-Plan.md](docs/Option-A-Plan.md)) — context
checks, the table engine, the call path, string-constant hashing, the
concat fold, and boxing — compounding to **≈ −35% geomean** against
its own baseline, every step benchstat-gated and byte-exactness-gated.

## Installation

```bash
go get github.com/pschlump/gopher-lua@v0.0.8
```

## Usage

```go
import (
    lua "github.com/pschlump/gopher-lua"
)

L := lua.NewState()
defer L.Close()
if err := L.DoString(`print(("Lua"):upper() .. " " .. _VERSION)`); err != nil {
    panic(err)
}
```

```go
L := lua.NewState()
defer L.Close()
if err := L.DoFile("hello.lua"); err != nil {
    panic(err)
}
```

Refer to the [Lua 5.5 Reference Manual](http://www.lua.org/manual/5.5/)
and the Go doc for `LState` methods. The API is the gopher-lua API
(objects, not stack indices) wherever the 5.5 upgrade kept it; see the
sections below and [docs/](docs/).

### Data model

All data in a Lua program is an `LValue` — an interface with
`String() string` and `Type() LValueType`:

| Type name   | Go type        | Type() value | Constants         |
|-------------|----------------|--------------|-------------------|
| `LNilType`  | (constants)    | `LTNil`      | `LNil`            |
| `LBool`     | (constants)    | `LTBool`     | `LTrue`, `LFalse` |
| `LNumber`   | float64        | `LTNumber`   | -                 |
| `LInteger`  | int64          | `LTInteger`  | -                 |
| `LString`   | string         | `LTString`   | -                 |
| `LFunction` | struct pointer | `LTFunction` | -                 |
| `LUserData` | struct pointer | `LTUserData` | -                 |
| `LState`    | struct pointer | `LTThread`   | -                 |
| `LTable`    | struct pointer | `LTTable`    | -                 |
| `LChannel`  | chan LValue    | `LTChannel`  | -                 |

Test types the Go way (type assertion) or via `Type()`. `LBool`,
`LNumber`, `LInteger`, `LString` are values, not pointers; test
nil/booleans against the constants (`lv == lua.LTrue`).
`lua.LVIsFalse` / `lua.LVAsBool` implement Lua truthiness (nil and
false are false).

### Calling Go from Lua

```go
func Double(L *lua.LState) int {
    lv := L.ToInt(1)              /* get argument */
    L.Push(lua.LNumber(lv * 2))   /* push result */
    return 1                      /* number of results */
}

L.SetGlobal("double", L.NewFunction(Double))
```

```lua
print(double(20)) -- > "40"
```

Coroutines, user-defined types (`LUserData` + metatables), module
creation (`PreloadModule`), and calling Lua from Go (`CallByParam`)
all work as in upstream gopher-lua — see the Go doc and the upstream
README; the 5.5 upgrade changed none of those shapes.

### Callstack & Registry size

An `LState`'s registry and callstack sizes are tunable per state —
worth doing when many `LState`s live in one process:

```go
L := lua.NewState(lua.Options{
    RegistrySize:     1024 * 20, // initial registry size
    RegistryMaxSize:  1024 * 80, // max growth (0 = fixed)
    RegistryGrowStep: 32,
    CallStackSize:    120,
    // MinimizeStackMemory: true, // page callframes in/out (default: fixed)
    // SkipOpenLibs:        true, // open libs yourself (sandboxing)
})
```

### Terminating a running LState

The interpreter checks a Go `context` at every instruction — a cancel
or deadline raises an ordinary (pcall-catchable) error at the next
instruction boundary:

```go
L := lua.NewState()
defer L.Close()
ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
defer cancel()
L.SetContext(ctx)
err := L.DoString(`
  local clock = os.clock
  function sleep(n)  -- busy-wait seconds
    local t0 = clock()
    while clock() - t0 <= n do end
  end
  sleep(3)
`)
// err.Error() contains "context deadline exceeded"
```

## Embedding in a server: the `host/` package

`host/` is the daemon surface over the interpreter — what Ultima's
EVAL/SCRIPT runs on. One process-wide `Engine` holds the script cache
and the host-function registry; each `Run` executes a compiled script
against a VM with fresh KEYS/ARGV staging, a deadline, and a memory
budget. Host functions are plain Go closures (the `redis.call`
mechanism). Scripts are untrusted by default: safe stdlibs, locked
globals, capped coroutines, accounted memory.

A complete Go HTTP server — script endpoint, host functions, sandbox,
**and script timeouts**:

```go
package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pschlump/gopher-lua/host"
)

var engine *host.Engine

func main() {
	engine = host.NewEngine(
		host.WithVMCacheSize(16),          // pool warm VMs per script (repeat scripts skip VM build)
		host.WithCompileMemCache(1024),    // bound the script cache (LRU)
		host.WithGlobalsProtection(true),  // Redis-classic lockdown: unknown globals raise, _G readonly
		host.WithMemoryBudgetBytes(32<<20),// per-VM accounted budget: 32 MB, catchable overrun
		host.WithMaxCoroutines(64),        // coroutines are goroutines — cap them
	)

	// Host functions: Go closures the script calls. A closure runs on the
	// VM's goroutine and must not call vm.Run (reentrancy is refused).
	engine.RegisterGlobal("app", "get", func(vm *host.VM, args []host.Value) ([]host.Value, error) {
		key := args[0].Str // arguments arrive as owning seam Values
		// ...fetch from your store...
		return []host.Value{host.String("value-of-" + key)}, nil
	})
	engine.RegisterGlobal("app", "incr", func(vm *host.VM, args []host.Value) ([]host.Value, error) {
		return []host.Value{host.Int(args[0].Int + 1)}, nil
	})
	// Constants: redis.LOG_WARNING-shaped values.
	engine.RegisterValue("app", "VERSION", host.String("1.2.3"))

	http.HandleFunc("/script", scriptHandler)
	http.ListenAndServe(":8080", nil)
}

func scriptHandler(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src") // e.g.:
	//   local v = app.get(KEYS[1]) return app.incr(ARGV[1] + 0), app.VERSION
	script, err := engine.Compile([]byte(src), "=client") // syntax errors verbatim
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The run: fresh KEYS/ARGV tables, bounded by a hard 5 s deadline.
	res, err := engine.Run(r.Context(), script, host.RunOptions{
		Keys:     []string{"user:1", "user:2"},
		Argv:     []string{"41"},
		Deadline: 5 * time.Second,
		Seed:     0, // optional: math.randomseed(n) per run
	})
	if err != nil {
		var se *host.ScriptError
		switch {
		case errors.As(err, &se) && se.ErrValue.String() == "context deadline exceeded":
			// The TIMEOUT fired (or the request context was canceled):
			// the interpreter's per-instruction check raised at the first
			// instruction past the deadline — bare, Line 0.
			http.Error(w, "script timed out", http.StatusRequestTimeout)
		case errors.As(err, &se):
			// A script-level error (raise, library, host fn). se.Line is
			// the raise line — the "on @user_script:N" position.
			http.Error(w, fmt.Sprintf("script error (line %d): %s", se.Line, se.ErrValue.String()), http.StatusBadRequest)
		default:
			// Engine-level failure (a bug class, not a script error).
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	for _, v := range res.Values {
		fmt.Fprintln(w, v.String())
	}
}
```

**Timeouts, precisely.** `RunOptions.Deadline` wraps the run's context
in a timer; the interpreter checks it at every instruction (and at
every Go-function return), so even a tight numeric loop stops within
one instruction of the deadline:

```go
script, _ := engine.Compile([]byte(`
	local i = 0
	while true do i = i + 1 end	-- would run forever
`), "=spin")

_, err := engine.Run(context.Background(), script,
	host.RunOptions{Deadline: 100 * time.Millisecond})
// err is *host.ScriptError{ErrValue: host.String("context deadline exceeded"), Line: 0}
```

The same normalization covers a caller-context cancel and `vm.Kill()`
— the SCRIPT KILL mechanism, callable from another goroutine while the
run holds the VM. Deadlines are not free (the check is amortized to
every 64th instruction; the warm lanes above run with a live
cancelable context) but they are always on in the daemon posture.

Concurrency law: a VM serves one run at a time (its own mutex); any
number of VMs run in parallel. `engine.Run` makes that automatic — a
pooled VM is taken atomically, a pool miss builds a fresh VM. Guest
errors keep the VM pooled (the Lua stack unwinds to base); engine
failures close it; a VM that has served 1024 runs is recycled.

The low-level alternative — `lua.NewState` + `L.SetContext` +
`DoString`, with your own `sync.Pool` — remains fully supported (see
"Terminating a running LState" above); `host/` exists because the
deadline/kill/budget/pool plumbing is exactly what every daemon ends
up rewriting.

## Differences between Lua and gopher-lua

- `os.setlocale`, `package.loadlib` are not implemented;
  `lua_Debug.namewhat` is not populated.
- `collectgarbage` takes no arguments and runs the collector for the
  whole Go program.
- `file:setvbuf` has no line buffering; DST is not supported.
- Channels (`channel.select`, `LChannel`) are a Go-native extension;
  coroutines are goroutines.
- In production, `host/` scripts get the safe stdlib set (no
  io/os/package/debug); `require`, `loadfile`, `dofile`, `load`, `warn`
  are nil'd even when the base library opens.

## Standalone interpreter

```bash
go install github.com/pschlump/gopher-lua/cmd/glua@v0.0.8
glua script.lua   # no args: REPL
```

## Build & test

```sh
make build   # regenerates state.go/vm.go from _state.go/_vm.go (never edit the generated files) + builds cmd/glua
make test    # regenerate + go test ./...
go test -short ./...   # skip the 26 s official-suite golden leg
```

The golden-corpus gate (`_goldens/`, 58 cases over the official 5.5
suite + fork cases) is the byte-exactness contract's enforcement:
`GOLDEN_UPDATE=1 go test . -run TestGolden` regenerates, and every
diff line must cite a ledger row in [docs/Divergence-Ledger.md](docs/Divergence-Ledger.md).

## License

MIT — see [LICENSE](LICENSE). Upstream copyright Yusuke Inuzuka
(yuin/gopher-lua).

## Authors

Yusuke Inuzuka (original gopher-lua) · Philip Schlump

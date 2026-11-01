package host

// VM: one Lua state — a plain LState plus the mutex that makes it
// thread-proof (the A9 law, carried over verbatim from glasm host: one
// execution at a time per VM, any number of VMs in parallel, safe from
// any goroutine). Unlike glasm's image there is no per-VM registry to
// bind: any script runs on any VM, serially. The pool still keys VMs by
// script identity (host.go) because reuse is only correct per-source:
// globals persist across runs (the shared-state mode — x = x + 1
// accumulates).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lua "github.com/pschlump/gopher-lua"
)

// RunOptions is one execution of a script against a VM.
type RunOptions struct {
	// Keys and Argv are staged as the KEYS / ARGV globals (fresh tables
	// every run — the script can never alias daemon memory).
	Keys, Argv []string
	// Deadline bounds the run: the interpreter's native per-instruction
	// context check raises an ordinary pcall-catchable error
	// ("context deadline exceeded") at the first instruction after the
	// deadline — pure-Go, preemptible at every instruction (the glasm
	// row-99/100 STW-wedge class cannot occur). Zero disables the timer.
	Deadline time.Duration
	// Seed reseals the VM's math.random stream for this run
	// (math.randomseed(n) over the interpreter's own stock xoshiro256**
	// stream — same seed, same sequence). Zero keeps the VM's existing
	// stream (fresh VMs start at the build-pinned open seed 0).
	Seed int64
}

// Result is a script's return values, converted to owning seam Values.
type Result struct {
	Values []Value
}

// ScriptError is a Lua-level error (raised by the script, a library, a
// host function, or the deadline machinery). ErrValue is the exact
// error value — for string errors Redis semantics pass the text through
// verbatim. Line is the raise line inside the script when the message
// carries a position prefix (0 = unknown) — the position Redis reports
// as "on @user_script:N" in error replies.
type ScriptError struct {
	ErrValue Value
	Line     int
}

func (e *ScriptError) Error() string { return e.ErrValue.String() }

// ErrReentrant is raised when a HostFunc tries to re-enter its own VM:
// the VM lock is not reentrant and the same-goroutine call would
// deadlock, so Run refuses with this error (surfacing inside the script
// as a clean, pcall-catchable error).
var ErrReentrant = errors.New("host: VM.Run called from inside a host function (the VM lock is not reentrant)")

// VM is one locked Lua state.
type VM struct {
	mu     sync.Mutex
	e      *Engine
	L      *lua.LState
	closed bool

	hostFns   []registeredFn  // snapshot of the engine registry
	hostVals  []registeredVal // snapshot of the engine value registry
	eventSink func(vm *VM, args []Value)

	// hostDepth is the same-goroutine reentrancy guard: the nesting
	// depth of this VM's host-function trampolines, incremented for the
	// whole trampoline (conversions included). Only the lock-holding
	// goroutine ever executes a trampoline, so a Run observing
	// hostDepth > 0 is either a HostFunc re-entering its own VM — the
	// deadlock case, refused with ErrReentrant — or another goroutine
	// arriving while the owner sits inside a host function (transient,
	// retryable — the glasm contract, narrowed from its whole-run window
	// to the hostfn window). Contention outside host functions blocks
	// on the mutex and serializes cleanly.
	hostDepth atomic.Int32

	// cancelMu + cancel are the SCRIPT KILL mechanism: the in-flight
	// run's context cancel, registered for Kill() (which runs on another
	// goroutine while Run holds mu).
	cancelMu sync.Mutex
	cancel   context.CancelFunc

	// runs is the runs-served counter (pool recycling signal).
	runs int
}

// runsServed reports the run count (pool recycling; white-box).
func (vm *VM) runsServed() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.runs
}

// NewVM builds a fresh LState with the engine's host functions and
// value constants staged. µs-scale (a state build plus stdlib open).
// The engine's registry is frozen from this moment.
func (e *Engine) NewVM() (*VM, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("host: engine closed")
	}
	vm := &VM{e: e, hostFns: e.hostFns, hostVals: e.hostVals, eventSink: e.opts.eventSink}
	e.vms++
	e.mu.Unlock()

	vm.L = lua.NewState(lua.Options{IncludeGoStackTrace: false})

	// host functions (redis.call and friends): direct Go closures.
	// Staging order matches glasm's (sandbox → host staging →
	// protection) so N2's WithGlobalsProtection slots in at the same
	// place: after all host staging, before any script runs.
	for _, hf := range vm.hostFns {
		tbl := vm.globalTable(hf.table)
		vm.L.SetField(tbl, hf.name, vm.L.NewFunction(vm.trampoline(hf.fn)))
	}
	// value constants (redis.LOG_WARNING and friends)
	for _, rv := range vm.hostVals {
		lv, err := toLValue(vm.L, rv.v)
		if err != nil {
			// unreachable: RegisterValue admits only convertible kinds
			_ = vm.Close()
			return nil, fmt.Errorf("host: staging %s.%s: %w", rv.table, rv.name, err)
		}
		vm.L.SetField(vm.globalTable(rv.table), rv.name, lv)
	}
	// print override (the event sink): after host staging, before runs.
	if vm.eventSink != nil {
		vm.L.SetGlobal("print", vm.L.NewFunction(vm.printTrampoline))
	}
	return vm, nil
}

// globalTable returns (creating if needed) the named global as a table.
func (vm *VM) globalTable(name string) *lua.LTable {
	if tbl, ok := vm.L.GetGlobal(name).(*lua.LTable); ok && tbl != nil {
		return tbl
	}
	tbl := vm.L.NewTable()
	vm.L.SetGlobal(name, tbl)
	return tbl
}

// Run executes s against the VM, holding vm.mu end-to-end (A9): one
// execution at a time per VM, any number of VMs in parallel, safe from
// any goroutine.
func (vm *VM) Run(ctx context.Context, s *Script, opt RunOptions) (res Result, err error) {
	if s == nil {
		return Result{}, errors.New("host: nil script")
	}
	if vm.hostDepth.Load() > 0 {
		return Result{}, ErrReentrant
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.closed {
		return Result{}, errors.New("host: VM closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	vm.runs++

	L := vm.L
	base := L.GetTop()
	defer L.SetTop(base) // stack hygiene: pooled VMs start each run clean

	// the run context: caller ctx ∩ optional deadline timer, plus a
	// Kill cancel (SCRIPT KILL). Registered for the duration of the run.
	if opt.Deadline > 0 {
		var dcancel context.CancelFunc
		ctx, dcancel = context.WithTimeout(ctx, opt.Deadline)
		defer dcancel()
	}
	rctx, kcancel := context.WithCancel(ctx)
	defer kcancel()
	vm.cancelMu.Lock()
	vm.cancel = kcancel
	vm.cancelMu.Unlock()
	defer func() {
		vm.cancelMu.Lock()
		vm.cancel = nil
		vm.cancelMu.Unlock()
	}()
	L.SetContext(rctx)

	// stage KEYS/ARGV as fresh tables (copies — binary-safe by
	// construction, no quoting seam exists)
	L.SetGlobal("KEYS", stringTable(L, opt.Keys))
	L.SetGlobal("ARGV", stringTable(L, opt.Argv))

	if opt.Seed != 0 {
		// determinism (D5 shape): math.randomseed(n) on the
		// interpreter's own stock stream — identical sequence per seed
		if err := vm.reseed(opt.Seed); err != nil {
			return Result{}, err
		}
	}

	fn := L.NewFunctionFromProto(s.Proto)
	L.Push(fn)
	if perr := L.PCall(0, lua.MultRet, nil); perr != nil {
		return Result{}, mapError(perr, s)
	}
	n := L.GetTop() - base
	vals := make([]Value, 0, max(n, 1))
	for i := 0; i < n; i++ {
		v, cerr := fromLValue(L.Get(base + 1 + i))
		if cerr != nil {
			return Result{}, cerr
		}
		vals = append(vals, v)
	}
	return Result{Values: vals}, nil
}

// stringTable builds a fresh sequential table of strings.
func stringTable(L *lua.LState, items []string) *lua.LTable {
	tbl := L.CreateTable(len(items), 0)
	for i, s := range items {
		tbl.RawSetInt(i+1, lua.LString(s))
	}
	return tbl
}

// reseed runs math.randomseed(n) in the guest.
func (vm *VM) reseed(seed int64) error {
	L := vm.L
	fn := L.GetField(L.GetGlobal("math"), "randomseed")
	if fn == lua.LNil {
		return errors.New("host: math.randomseed unavailable (math library not open)")
	}
	base := L.GetTop()
	defer L.SetTop(base)
	L.Push(fn)
	L.Push(lua.LInteger(seed))
	if err := L.PCall(1, 0, nil); err != nil {
		return fmt.Errorf("host: math.randomseed: %w", err)
	}
	return nil
}

// mapError classifies a PCall failure: Lua-level errors (ApiErrorRun /
// ApiErrorSyntax — script raises, library errors, hostfn errors, the
// deadline raise) become ScriptError with the exact error value; a Go
// panic outside the Lua error protocol (ApiErrorPanic — the
// engine-bug class, glasm's TrapError analog) is returned as-is so the
// daemon logs it rather than answering it as a script error.
func mapError(err error, s *Script) error {
	ae, ok := err.(*lua.ApiError)
	if !ok {
		return err
	}
	if ae.Type == lua.ApiErrorPanic {
		return fmt.Errorf("host: engine-level failure (backend bug class): %w", err)
	}
	ev, cerr := fromLValue(ae.Object)
	if cerr != nil {
		ev = String(ae.Object.String())
	}
	return &ScriptError{ErrValue: ev, Line: raiseLine(ae.Object, s.name)}
}

// raiseLine extracts the raise position from a string error value's
// "<chunk>:<line>:" prefix (best-effort, 0 = unknown — the interpreter
// prefixes level-1 raises; level-0 and hostfn raises carry none). The
// position carries the chunkid form: stock luaO_chunkid strips a leading
// '=' from chunk names.
func raiseLine(obj lua.LValue, name string) int {
	str, ok := obj.(lua.LString)
	if !ok {
		return 0
	}
	if strings.HasPrefix(name, "=") {
		name = name[1:]
	}
	if !strings.HasPrefix(string(str), name+":") {
		return 0
	}
	rest := string(str)[len(name)+1:]
	if i := strings.IndexByte(rest, ':'); i > 0 {
		if n, err := strconv.Atoi(rest[:i]); err == nil {
			return n
		}
	}
	return 0
}

// Kill cancels the in-flight run's context: the next instruction check
// raises the ordinary (pcall-catchable) deadline-class error, exactly as
// if the run's Deadline had fired. No-op between runs. This is the
// SCRIPT KILL mechanism: the daemon's scripting manager calls it on the
// VM a killable script is running on.
func (vm *VM) Kill() {
	vm.cancelMu.Lock()
	f := vm.cancel
	vm.cancelMu.Unlock()
	if f != nil {
		f()
	}
}

// Close tears the LState down (running pending finalizers and closing
// to-be-closed variables first — LState.Close). The VM is unusable
// afterwards. A Run in flight keeps working to its own end; Close
// waits for the lock.
func (vm *VM) Close() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.closed {
		return nil
	}
	vm.closed = true
	if vm.L != nil {
		vm.L.Close()
	}
	return nil
}

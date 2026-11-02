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
	//
	// The deadline/kill class is normalized on the way out (mapError):
	// when the run context fired and the interpreter raised its message,
	// the error crosses as ScriptError{String("context deadline
	// exceeded"), Line: 0} — bare, no position — for the deadline timer,
	// a Kill, and a caller-ctx cancel alike. The daemon-side manager
	// (Ultima's RunOnVM shape) keys on exactly this pair and distinguishes
	// the three cases via its own flags.
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

	// intern is the staging-box cache: KEYS/ARGV strings that repeat
	// across runs reuse their boxed LValue instead of re-allocating the
	// 16-byte interface header per element per run (the bounded analog
	// of stock Lua's global string table, which interns every string the
	// same way). Only the lock-holding goroutine stages, so no lock.
	// Bounded by stagingInternMax; a full map just boxes without caching.
	intern map[string]lua.LValue

	// argsScratch is the hostfn trampoline's argument buffer, reused
	// across crossings (the A9 law makes this safe: only the
	// lock-holding goroutine ever runs a trampoline, and same-VM
	// re-entry is refused by the hostDepth guard, so no live crossing
	// can observe a refill). HostFuncs that need to keep the slice past
	// their return must copy it — the HostFunc contract says so.
	argsScratch []Value
}

// runsServed reports the run count (pool recycling; white-box).
func (vm *VM) runsServed() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.runs
}

// NewVM builds a fresh LState with the engine's sandbox posture,
// host functions, and value constants staged. µs-scale (a state build
// plus stdlib open). The engine's registry is frozen from this moment.
//
// Order (the glasm law): budget → libraries → dangerous-global nils →
// host staging → protection. The budget arms before the libraries open
// so lib-open allocations count; protection installs after all staging
// so nothing staged needs the raw seam.
func (e *Engine) NewVM() (*VM, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("host: engine closed")
	}
	vm := &VM{e: e, hostFns: e.hostFns, hostVals: e.hostVals, eventSink: e.opts.eventSink}
	e.vms++
	e.mu.Unlock()

	vm.L = lua.NewState(lua.Options{
		IncludeGoStackTrace: false,
		SkipOpenLibs:        true,
		MaxCoroutines:       e.opts.maxCoroutines,
	})
	if e.opts.memoryBudget > 0 {
		vm.L.SetMemoryBudgetBytes(e.opts.memoryBudget)
	}

	// the library posture (default: the SafeStdLibs prod set)
	libs := e.opts.stdlibs
	if libs == nil {
		libs = SafeStdLibs
	}
	for _, lib := range libs {
		vm.L.Push(vm.L.NewFunction(lib.Open))
		vm.L.Push(lua.LString(lib.Name))
		vm.L.Call(1, 0)
	}
	// the base library's own ambient loaders — the glasm sandbox nils
	// these even when Base opens (require/loadfile/dofile reach the
	// filesystem, load compiles arbitrary sources, warn writes stderr);
	// with SafeStdLibs package/io/os/debug never opened at all.
	for _, name := range []string{"require", "loadfile", "dofile", "load", "warn"} {
		vm.L.SetGlobal(name, lua.LNil)
	}

	// host functions (redis.call and friends): direct Go closures.
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
	// globals protection: last — everything staged above is inside the
	// readonly set; only the raw staging seam (Run's KEYS/ARGV) writes
	// after this point.
	if e.opts.globalsProtection {
		installProtection(vm.L)
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
	// construction, no quoting seam exists). The raw seam, not
	// SetGlobal: under globals protection _G is readonly, and KEYS/ARGV
	// are staged after the protect walk by design (guest-writable —
	// the glasm contract). The name LValues are pre-boxed package
	// constants and the element boxes come from the VM's intern cache,
	// so a pool-hit run with repeated keys/argv stages allocation-free
	// beyond the two fresh tables themselves.
	globals, _ := L.GetGlobal("_G").(*lua.LTable)
	L.RawSet(globals, keyKEYS, lua.LValue(vm.stringTable(L, opt.Keys)))
	L.RawSet(globals, keyARGV, lua.LValue(vm.stringTable(L, opt.Argv)))

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
		return Result{}, mapError(perr, s, rctx)
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

// stagingInternMax bounds the per-VM staging intern: hot scripts stage
// the same key/argv strings run after run (hot-key traffic), and each
// repeated element would otherwise cost a fresh 16-byte interface-box
// allocation per run. The bound keeps a pathological never-repeating
// workload from growing the cache without limit (stock C Lua interns
// unconditionally; a daemon embedding cannot).
const stagingInternMax = 512

// The staged globals' names, pre-boxed (boxing LString into an LValue
// interface allocates — do the two constants once, not per run).
var (
	keyKEYS = lua.LValue(lua.LString("KEYS"))
	keyARGV = lua.LValue(lua.LString("ARGV"))
)

// stringTable builds a fresh sequential table of strings. The table (and
// its array backing) is fresh every run — the script gets no alias into
// a previous run's state — but the per-element LValue boxes come from
// the VM's intern cache when the string was staged before: the box holds
// an immutable string, so reuse is observationally identical to
// re-boxing (Lua has no mutable strings).
func (vm *VM) stringTable(L *lua.LState, items []string) *lua.LTable {
	tbl := L.CreateTable(len(items), 0)
	for i, s := range items {
		tbl.RawSetInt(i+1, vm.internString(s))
	}
	return tbl
}

// internString returns the boxed LValue for s, caching it in the VM's
// staging intern on first sight (bounded — see stagingInternMax).
func (vm *VM) internString(s string) lua.LValue {
	if lv, ok := vm.intern[s]; ok {
		return lv
	}
	lv := lua.LValue(lua.LString(s))
	if len(vm.intern) < stagingInternMax {
		if vm.intern == nil {
			vm.intern = make(map[string]lua.LValue, 16)
		}
		vm.intern[s] = lv
	}
	return lv
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

// mapError classifies a PCall failure: the deadline/kill class
// normalizes to its bare contract first, then Lua-level errors
// (ApiErrorRun / ApiErrorSyntax — script raises, library errors, hostfn
// errors) become ScriptError with the exact error value; a Go panic
// outside the Lua error protocol (ApiErrorPanic — the engine-bug class,
// glasm's TrapError analog) is returned as-is so the daemon logs it
// rather than answering it as a script error.
//
// Line: the message's own position prefix wins (level-1 raises carry
// "chunk:N:"); prefixless errors (level-0 raises, hostfn errors) take
// the raise-time traceback — PCall captures the Lua stack trace while
// the dying frames are still live, and its first positioned frame is
// the innermost Lua frame. This is the Redis shape: its error handler
// reports the traceback's top line whatever the raise level. (Reading
// the frame stack after PCall returns cannot work — recovery restores
// sp before returning, unwinding every frame the call pushed.)
func mapError(err error, s *Script, rctx context.Context) error {
	ae, ok := err.(*lua.ApiError)
	if !ok {
		return err
	}
	// The deadline/kill class: the run context fired and the interpreter
	// raised its message (with whatever position prefix the raise site
	// carried). It crosses as the bare contract — Line 0, "context
	// deadline exceeded" — for the deadline timer, Kill, and a
	// caller-ctx cancel alike (the glasm runtime staged the same shape;
	// its ledger row 37). The exact/suffix match keeps a script that
	// errors this very text on the ordinary path when no context fired.
	if cerr := rctx.Err(); cerr != nil {
		if str, ok := ae.Object.(lua.LString); ok {
			msg := string(str)
			if msg == cerr.Error() || strings.HasSuffix(msg, ": "+cerr.Error()) {
				return &ScriptError{ErrValue: String("context deadline exceeded"), Line: 0}
			}
		}
	}
	if ae.Type == lua.ApiErrorPanic {
		return fmt.Errorf("host: engine-level failure (backend bug class): %w", err)
	}
	ev, cerr := fromLValue(ae.Object)
	if cerr != nil {
		ev = String(ae.Object.String())
	}
	line := raiseLine(ae.Object, s.name)
	if line == 0 {
		line = tracebackTopLine(ae.StackTrace, s.name)
	}
	return &ScriptError{ErrValue: ev, Line: line}
}

// tracebackTopLine scans a raise-time Lua traceback for the first
// positioned frame of the named chunk — the innermost Lua frame's
// current line, the raise position for errors that carry no prefix of
// their own. Traceback lines render "\t<chunkid>:<line>: in <what>"
// (Go frames have no line and never match); 0 = nothing found.
func tracebackTopLine(tb, name string) int {
	if name == "" {
		return 0
	}
	name = strings.TrimPrefix(name, "=")
	prefix := name + ":"
	for _, ln := range strings.Split(tb, "\n") {
		ln = strings.TrimPrefix(ln, "\t")
		if !strings.HasPrefix(ln, prefix) {
			continue
		}
		rest := ln[len(prefix):]
		i := strings.IndexByte(rest, ':')
		if i <= 0 {
			continue
		}
		if n, err := strconv.Atoi(rest[:i]); err == nil && n > 0 {
			return n
		}
	}
	return 0
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

// UsedBytes reports this VM's accounted allocation total (the same
// counter WithMemoryBudgetBytes enforces against): monotone — never
// refunded, the high-water gauge — and 0 once the VM is closed. A
// fresh VM's counter starts at its build allocations, so a recycled
// VM visibly drops below the watermark it crossed.
func (vm *VM) UsedBytes() int64 {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.closed || vm.L == nil {
		return 0
	}
	return vm.L.MemoryUsedBytes()
}

// Kill cancels the in-flight run's context: the next instruction check
// raises the ordinary (pcall-catchable) deadline-class error, exactly as
// if the run's Deadline had fired — normalized per mapError to the bare
// contract (ErrValue "context deadline exceeded", Line 0). No-op between
// runs. This is the SCRIPT KILL mechanism: the daemon's scripting
// manager calls it on the VM a killable script is running on.
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

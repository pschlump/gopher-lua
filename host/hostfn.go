package host

// The host side of the Lua→Go seam. Both trampolines fire on the
// goroutine that holds vm.mu (the running script's own goroutine — the
// A9 law), which is what makes redis.call deadlock-free: the daemon's
// bridge runs synchronously inside the guest's call frame.
//
// Where glasm crossed this seam with the wire protocol and staging
// buffers, here the trampoline converts args to owning Values, calls
// the closure, and pushes the returned Values back — and errors raise
// through L.Error(v, 0): a bare lua_error, no position prefix, so
// command error texts propagate verbatim (Redis semantics — the
// rt_sea_raise_nopos analog).

import (
	"errors"
	"fmt"

	lua "github.com/pschlump/gopher-lua"
)

// ValueError lets a HostFunc raise a non-string Lua error value — e.g.
// a table {err="..."}, the error class redis.call raises in Redis (the
// script's pcall then sees a table and the daemon renders the text
// verbatim, instead of the error gaining an engine-added prefix).
type ValueError struct{ V Value }

func (e *ValueError) Error() string { return "host function error: " + e.V.String() }

// trampoline builds the Lua-visible closure for a registered host
// function. Conversions use the seam caps (value.go): >4096 arguments
// or results is a clean, pcall-catchable error; a panicking HostFunc is
// contained here — the guest sees a clean Lua error, never a Go panic
// (glasm's wasm-boundary containment, restated in Go).
func (vm *VM) trampoline(fn HostFunc) lua.LGFunction {
	return func(L *lua.LState) int {
		vm.hostDepth.Add(1)
		defer vm.hostDepth.Add(-1)
		n := L.GetTop()
		if n > maxFrameVals {
			bareRaise(L, lua.LString(fmt.Sprintf("host function call: too many hostfn arguments (%d)", n)))
		}
		// engine-owned scratch, refilled per crossing (see VM.argsScratch:
		// the A9 law makes reuse safe; HostFuncs copy if they retain)
		if cap(vm.argsScratch) < n {
			vm.argsScratch = make([]Value, n, max(n, 8))
		}
		args := vm.argsScratch[:n]
		for i := 1; i <= n; i++ {
			a, err := fromLValue(L.Get(i))
			if err != nil {
				bareRaise(L, lua.LString("host function call: "+err.Error()))
			}
			args[i-1] = a
		}
		vals, err := callHost(fn, vm, args)
		if err != nil {
			var ve *ValueError
			if errors.As(err, &ve) {
				lv, cerr := toLValue(L, ve.V)
				if cerr != nil {
					bareRaise(L, lua.LString(err.Error()))
				}
				bareRaise(L, lv)
			}
			bareRaise(L, lua.LString(err.Error()))
		}
		if len(vals) > maxFrameVals {
			bareRaise(L, lua.LString(fmt.Sprintf("host function call: too many hostfn results (%d)", len(vals))))
		}
		for _, v := range vals {
			lv, cerr := toLValue(L, v)
			if cerr != nil {
				bareRaise(L, lua.LString("host function call: "+cerr.Error()))
			}
			L.Push(lv)
		}
		return len(vals)
	}
}

// callHost invokes the closure with panic containment.
func callHost(fn HostFunc, vm *VM, args []Value) (vals []Value, err error) {
	defer func() {
		if p := recover(); p != nil {
			vals = nil
			err = fmt.Errorf("host function panic: %v", p)
		}
	}()
	return fn(vm, args)
}

// bareRaise is lua_error: raise the finished value with no position
// added anywhere (L.Error's level 0 adds no prefix for strings and
// passes non-strings through untouched).
func bareRaise(L *lua.LState, lv lua.LValue) {
	L.Error(lv, 0)
}

// printTrampoline replaces the global print when an event sink is
// installed: the sink observes every guest print's arguments as seam
// Values. Tables cross as bare KindTable markers — print never expands
// tables (glasm's g_print posture, expand=0); natively the marker is
// decodable instead of dropping the whole frame, which is what the wire
// decode of a bare table tag effectively did.
func (vm *VM) printTrampoline(L *lua.LState) int {
	if vm.eventSink == nil {
		return 0
	}
	n := L.GetTop()
	args := make([]Value, n)
	for i := 1; i <= n; i++ {
		lv := L.Get(i)
		if _, isTable := lv.(*lua.LTable); isTable {
			args[i-1] = Value{Kind: KindTable}
			continue
		}
		a, err := fromLValue(lv)
		if err != nil { // unreachable: only tables can hit the budget
			a = Value{Kind: KindUserdata}
		}
		args[i-1] = a
	}
	vm.eventSink(vm, args)
	return 0
}

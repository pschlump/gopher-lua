package host

// Globals protection (WithGlobalsProtection): the Redis-classic script
// environment lockdown, ported from the wasm engine's runtime
// deps/lua-parity patch. Two mechanisms over the interpreter's native
// seams:
//
//   - the missing-global guard: an __index Go function on the globals
//     table's metatable. The interpreter's field read RawGets first and
//     consults __index only on a miss, so the guard fires exactly on
//     nonexistent globals. Byte texts are the glasm-blob contract
//     (probed against Redis 7.2.7): "Script attempted to access
//     nonexistent global variable '<name>'" for string/number keys,
//     "Second argument to luaProtectedTableError must be a string or
//     number" otherwise. RaiseError from a G function takes level 1 —
//     the calling Lua frame's position — so the message carries the
//     "<chunk>:<line>: " prefix (pcall sees the same prefixed string).
//
//   - the readonly walk: _G and every table reachable from it (values,
//     nested tables, metatables, the string metatable) are marked with
//     LTable.SetReadonly — a one-way guard the bytecode write paths
//     (setField/setFieldString, covering SETTABUP/SETTABLE/SETI/
//     SETFIELD, LState.SetTable/SetField, and through them rawset and
//     the table library's lua_seti writes) enforce with "Attempt to
//     modify a readonly table". Raw Go writes (LState.RawSet, the
//     LTable RawSet*/Insert/Append methods) stay exempt — that is the
//     host staging seam: KEYS/ARGV are staged through it after the
//     walk (glasm staged them after the protect walk too, which is why
//     they are guest-writable by contract).
//
// Protection is installed once, at NewVM, AFTER all host staging
// (host functions, registered values, the print override) — the glasm
// order: sandbox → host staging → protection. There is no unprotect.

import (
	lua "github.com/pschlump/gopher-lua"
)

// installProtection installs both mechanisms on L. One-way.
func installProtection(L *lua.LState) {
	globals, ok := L.GetGlobal("_G").(*lua.LTable)
	if !ok || globals == nil {
		return // no _G (never happens with the base library open)
	}

	// the missing-global guard: __index on the globals table's
	// metatable, ahead of any metatable the walk would find (a
	// metatable set by a library on _G is preserved beneath — getmetatable
	// (_G) keeps answering non-nil).
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(protectedIndex))
	L.SetMetatable(globals, mt)

	// the readonly walk
	seen := map[*lua.LTable]bool{}
	protectWalk(L, lua.LValue(globals), seen)
	// the string metatable is reachable from no global table but is
	// shared by every string: mark it too (string.foo = 1 raises).
	if smt := L.GetMetatable(lua.LString("")); smt != nil {
		if smtt, ok := smt.(*lua.LTable); ok {
			protectWalk(L, smtt, seen)
		}
	}
}

// protectedIndex is the __index guard: fires only on globals-table
// misses (the interpreter RawGets before consulting it).
func protectedIndex(L *lua.LState) int {
	switch k := L.Get(2).(type) {
	case lua.LString:
		L.RaiseError("Script attempted to access nonexistent global variable '%s'", string(k))
	case lua.LNumber:
		L.RaiseError("Script attempted to access nonexistent global variable '%s'", lua.LVAsString(k))
	case lua.LInteger:
		L.RaiseError("Script attempted to access nonexistent global variable '%s'", lua.LVAsString(k))
	default:
		L.RaiseError("Second argument to luaProtectedTableError must be a string or number")
	}
	return 0
}

// protectWalk marks v (when a table) readonly and recurses through
// values and metatables. Functions, userdatas and threads are not
// tables and hold no indexed state to protect.
func protectWalk(L *lua.LState, v lua.LValue, seen map[*lua.LTable]bool) {
	tb, ok := v.(*lua.LTable)
	if !ok || seen[tb] {
		return
	}
	seen[tb] = true
	tb.SetReadonly()
	if mt := L.GetMetatable(tb); mt != nil {
		if mtt, ok := mt.(*lua.LTable); ok {
			protectWalk(L, mtt, seen)
		}
	}
	tb.ForEach(func(k, val lua.LValue) {
		protectWalk(L, val, seen)
	})
}

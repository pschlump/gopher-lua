// Package luawasm is the Lua→wasm backend (design doc §4, milestone M4):
// FunctionProto trees from the gopher-lua frontend become wasm modules
// that share linear memory with the C runtime and call down through the
// frozen rt_* ABI (runtime/rt_abi.h).
//
// v1 shape: one wasm function for the main proto, registers as TValue
// cells in a frame allocated from the runtime heap, flattened control
// flow (loop + br_table over basic blocks), every opcode lowered via the
// ABI with inline fast paths for number arithmetic and the numeric for
// loop. Calls (OP_CALL/OP_TAILCALL) go through rt_call — the runtime's
// lvm executes the callee — so correctness never depends on the compiled
// path (the M3 architecture law).
//
// Not yet lowered (v1): OP_CLOSURE (needs the Proto-struct emission +
// closures ABI, carried to M5) and OP_VARARG; encountering them fails
// the compile with a clear error.
package luawasm

import (
	"fmt"

	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/wasm"
)

const (
	cellSize = 16 // sizeof(TValue) on wasm32 (static-asserted in rt_abi.c)
	rtNumTag = 3  // LUA_TNUMBER in TValue.tt
)

// Compile emits a wasm module for the FunctionProto tree rooted at main.
func Compile(main *lua.FunctionProto, chunkName string) ([]byte, error) {
	protos := collectProtos(main)
	for _, p := range protos {
		for _, inst := range p.Code {
			switch int(inst >> 26) {
			case lua.OP_CLOSURE:
				return nil, fmt.Errorf("luawasm: OP_CLOSURE not supported in backend v1 (closures arrive with the closures ABI)")
			case lua.OP_VARARG:
				return nil, fmt.Errorf("luawasm: OP_VARARG not supported in backend v1")
			case lua.OP_GETUPVAL, lua.OP_SETUPVAL:
				return nil, fmt.Errorf("luawasm: upvalue opcodes require closures (backend v1)")
			}
		}
	}

	b := &backend{m: wasm.NewModule(), main: main, chunkName: chunkName}
	b.declareImports()
	b.emitInit()
	fe := b.newFuncEmitter()
	fe.emitBody()
	fe.f.Export("lua_main")
	return b.m.Encode(), nil
}

func collectProtos(p *lua.FunctionProto) []*lua.FunctionProto {
	out := []*lua.FunctionProto{p}
	for _, c := range p.FunctionPrototypes {
		out = append(out, collectProtos(c)...)
	}
	return out
}

type backend struct {
	m         *wasm.Module
	main      *lua.FunctionProto
	chunkName string
	imports   map[string]uint32
	gKCells   uint32 // mutable global: constants cells base
}

func (b *backend) imp(name string) uint32 { return b.imports[name] }

func (b *backend) declareImports() {
	m := b.m
	i32v := []wasm.ValueType{wasm.I32}
	ii := []wasm.ValueType{wasm.I32, wasm.I32}
	iii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32}
	iiii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32}
	iiiii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32}
	i32f64 := []wasm.ValueType{wasm.I32, wasm.F64}

	b.imports = map[string]uint32{
		"rt_newtable":      m.ImportFunc("rt", "rt_newtable", iii, i32v),
		"rt_gettable":      m.ImportFunc("rt", "rt_gettable", iiii, i32v),
		"rt_settable":      m.ImportFunc("rt", "rt_settable", iiii, i32v),
		"rt_arith":         m.ImportFunc("rt", "rt_arith", iiiii, i32v),
		"rt_len":           m.ImportFunc("rt", "rt_len", iii, i32v),
		"rt_eq":            m.ImportFunc("rt", "rt_eq", iiii, i32v),
		"rt_lt":            m.ImportFunc("rt", "rt_lt", iiii, i32v),
		"rt_le":            m.ImportFunc("rt", "rt_le", iiii, i32v),
		"rt_concat":        m.ImportFunc("rt", "rt_concat", iiii, i32v),
		"rt_call":          m.ImportFunc("rt", "rt_call", iiiii, i32v),
		"rt_forprep":       m.ImportFunc("rt", "rt_forprep", ii, i32v),
		"rt_error":         m.ImportFunc("rt", "rt_error", iii, nil),
		"rt_frame_alloc":   m.ImportFunc("rt", "rt_frame_alloc", i32v, i32v),
		"rt_getglobal":     m.ImportFunc("rt", "rt_getglobal", iii, i32v),
		"rt_setglobal":     m.ImportFunc("rt", "rt_setglobal", iii, i32v),
		"rt_intern":        m.ImportFunc("rt", "rt_intern", iii, i32v),
		"rt_mknumber":      m.ImportFunc("rt", "rt_mknumber", i32f64, nil),
		"rt_mknil":         m.ImportFunc("rt", "rt_mknil", i32v, nil),
		"rt_set_chunkname": m.ImportFunc("rt", "rt_set_chunkname", ii, nil),
		"rt_err_pending":   m.ImportFunc("rt", "rt_err_pending", nil, i32v),
		"rt_call_count":    m.ImportFunc("rt", "rt_call_count", i32v, i32v),
	}
	b.gKCells = m.GlobalI32(0, true)
	m.ExportGlobal("gKCells", b.gKCells)
	m.ImportMemory("rt", "memory", 1, 0)
}

// emitInit writes luawasm_init: interns all constants into a cells area
// (base stored in the gKCells global) and installs the chunk name. All
// bytes are written with i64 stores into one rt_frame_alloc buffer, so
// the module needs no data segments and cannot collide with the runtime.
func (b *backend) emitInit() {
	m := b.m
	f := m.NewFunction([]wasm.ValueType{wasm.I32}, nil)
	// local 0 is the step PARAM; declared locals follow it
	const lStep uint32 = 0
	const (lS, lK, lC uint32 = 1, 2, 3)
	f.Local(wasm.I32).Local(wasm.I32).Local(wasm.I32)

	main := b.main
	nc := len(main.Constants)
	nsc := len(main.StringConstants())
	total := nc + nsc
	strBytes := len(b.chunkName)
	for _, s := range main.StringConstants() {
		strBytes += len(s)
	}
	for _, cv := range main.Constants {
		if s, ok := cv.(lua.LString); ok {
			strBytes += len(s)
		}
	}

	f.I32Const(int32(strBytes)).Call(b.imp("rt_frame_alloc")).LocalSet(lS)
	f.I32Const(int32(cellSize * total)).Call(b.imp("rt_frame_alloc")).LocalSet(lK)
	f.LocalGet(lK).GlobalSet(b.gKCells)
	f.I32Const(0).LocalSet(lC)

	// staged by the step parameter (0 = allocs only, 1 = +chunkname,
	// 2 = +interns) — a compile-time debug aid kept for triage
	f.LocalGet(lStep).I32Const(2).I32GeS().If(wasm.Void)

	// chunk name
	writeStrBytes(f, lS, lC, b.chunkName)
	f.LocalGet(lS).I32Const(int32(len(b.chunkName))).Call(b.imp("rt_set_chunkname"))

	// string constants (cells at offset nc+i). NOTE: writeStrBytes
	// advances the cursor past s, so the pointer is cursor-len(s)
	for i, s := range main.StringConstants() {
		writeStrBytes(f, lS, lC, s)
		f.LocalGet(lK).I32Const(int32(cellSize*(nc+i))).I32Add()
		f.LocalGet(lS).LocalGet(lC).I32Const(int32(len(s))).I32Sub().I32Add()
		f.I32Const(int32(len(s))).Call(b.imp("rt_intern")).Drop()
	}
	// value constants
	for i, cv := range main.Constants {
		switch v := cv.(type) {
		case lua.LNumber:
			f.LocalGet(lK).I32Const(int32(cellSize*i)).I32Add().F64Const(float64(v)).
				Call(b.imp("rt_mknumber"))
		case lua.LString:
			writeStrBytes(f, lS, lC, string(v))
			f.LocalGet(lK).I32Const(int32(cellSize*i)).I32Add()
			f.LocalGet(lS).LocalGet(lC).I32Const(int32(len(v))).I32Sub().I32Add()
			f.I32Const(int32(len(v))).Call(b.imp("rt_intern")).Drop()
		default:
			panic(fmt.Sprintf("luawasm: unsupported constant type %T", cv))
		}
	}
	f.End()
	f.End() // close the step gate
	f.Export("luawasm_init")
}

// writeStrBytes appends s to the sbuf (base local lS, cursor local lC),
// advancing the cursor: full 8-byte chunks via i64.const stores, the
// tail via single-byte stores.
func writeStrBytes(f *wasm.Function, lS, lC uint32, s string) {
	addr := func(off int) {
		f.LocalGet(lS).LocalGet(lC).I32Add()
		if off > 0 {
			f.I32Const(int32(off)).I32Add()
		}
	}
	i := 0
	for ; i+8 <= len(s); i += 8 {
		var v uint64
		for j := 7; j >= 0; j-- {
			v = v<<8 | uint64(s[i+j])
		}
		addr(i)
		f.I64Const(int64(v)).I64Store(0)
	}
	for ; i < len(s); i++ {
		addr(i)
		f.I32Const(int32(s[i])).I32Store8(0)
	}
	f.LocalGet(lC).I32Const(int32(len(s))).I32Add().LocalSet(lC)
}

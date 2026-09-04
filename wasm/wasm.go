// Package wasm is a minimal, dependency-free emitter for WebAssembly 1.0/2.0
// core binary modules. It is the code-generation layer of the Lua→wasm
// backend (docs/Lua-Wasm-Design-and-Test-Plan.md, milestone M0).
//
// The emitter produces the 2.0 (bulk-memory) forms of element and data
// segments, which wazero and every current runtime accept.
package wasm

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ValueType is a wasm value type byte.
type ValueType byte

const (
	I32 ValueType = 0x7F
	I64 ValueType = 0x7E
	F32 ValueType = 0x7D
	F64 ValueType = 0x7C
)

// BlockType is the blocktype immediate of block/loop/if.
type BlockType byte

// Void is the empty blocktype.
const Void BlockType = 0x40

// Result returns the blocktype producing one value of type vt.
func Result(vt ValueType) BlockType { return BlockType(vt) }

type funcType struct {
	params, results []ValueType
}

type localGroup struct {
	count uint32
	typ   ValueType
}

// Function is a locally-defined function under construction.
type Function struct {
	m          *Module
	idx        uint32 // index in the module-wide function index space
	typeIdx    uint32
	locals     []localGroup
	body       []byte
	depth      int  // open block/loop/if constructs
	terminated bool // final End emitted
}

type importFunc struct {
	module, name string
	typeIdx      uint32
}

type limitsDecl struct{ min, max uint32 } // max==0 means unbounded

type globalDecl struct {
	typ     ValueType
	mutable bool
	init    []byte // init expression including the trailing 0x0B end
}

type exportEntry struct {
	name string
	kind byte
	idx  uint32
}

type elemSegment struct {
	offset uint32
	funcs  []uint32
}

type dataSegment struct {
	offset uint32
	bytes  []byte
}

// Export kinds.
const (
	ExportKindFunc   byte = 0
	ExportKindTable  byte = 1
	ExportKindMem    byte = 2
	ExportKindGlobal byte = 3
)

// Module is a wasm module under construction.
type Module struct {
	types    []funcType
	typeIdx  map[string]uint32
	imports  []importFunc
	nImports uint32 // number of imported functions
	funcs    []*Function
	table    *limitsDecl
	memory   *limitsDecl
	globals  []globalDecl
	exports  []exportEntry
	elems    []elemSegment
	datas    []dataSegment
	start    uint32
	hasStart bool
}

// NewModule returns an empty module.
func NewModule() *Module {
	return &Module{typeIdx: map[string]uint32{}}
}

// TypeIndex returns the type-section index of the signature
// params→results, adding it if not present. Signatures are deduplicated.
func (m *Module) TypeIndex(params, results []ValueType) uint32 {
	var key []byte
	for _, p := range params {
		key = append(key, byte(p))
	}
	key = append(key, '|')
	for _, r := range results {
		key = append(key, byte(r))
	}
	if i, ok := m.typeIdx[string(key)]; ok {
		return i
	}
	i := uint32(len(m.types))
	m.types = append(m.types, funcType{
		params:  append([]ValueType(nil), params...),
		results: append([]ValueType(nil), results...),
	})
	m.typeIdx[string(key)] = i
	return i
}

// ImportFunc declares an imported function and returns its function index.
// Imported functions occupy the first indices of the function index space.
func (m *Module) ImportFunc(module, name string, params, results []ValueType) uint32 {
	idx := m.nImports
	m.imports = append(m.imports, importFunc{module, name, m.TypeIndex(params, results)})
	m.nImports++
	return idx
}

// NewFunction appends a new locally-defined function and returns it for
// code emission. The function is not usable until its final End has been
// emitted (Encode checks balance).
func (m *Module) NewFunction(params, results []ValueType) *Function {
	idx := m.nImports + uint32(len(m.funcs))
	f := &Function{m: m, idx: idx, typeIdx: m.TypeIndex(params, results)}
	m.funcs = append(m.funcs, f)
	return f
}

// Table declares the (single) funcref table. max==0 means unbounded.
func (m *Module) Table(min, max uint32) { m.table = &limitsDecl{min, max} }

// Memory declares linear memory. max==0 means unbounded.
func (m *Module) Memory(min, max uint32) { m.memory = &limitsDecl{min, max} }

// GlobalI32 appends a global with an i32 initializer and returns its index.
func (m *Module) GlobalI32(v int32, mutable bool) uint32 {
	init := append([]byte{0x41}, AppendI32(nil, v)...)
	return m.addGlobal(I32, mutable, append(init, 0x0B))
}

// GlobalI64 appends a global with an i64 initializer and returns its index.
func (m *Module) GlobalI64(v int64, mutable bool) uint32 {
	init := append([]byte{0x42}, AppendS64(nil, v)...)
	return m.addGlobal(I64, mutable, append(init, 0x0B))
}

// GlobalF64 appends a global with an f64 initializer and returns its index.
func (m *Module) GlobalF64(v float64, mutable bool) uint32 {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	return m.addGlobal(F64, mutable, append(append([]byte{0x44}, b[:]...), 0x0B))
}

func (m *Module) addGlobal(typ ValueType, mutable bool, init []byte) uint32 {
	idx := uint32(len(m.globals))
	m.globals = append(m.globals, globalDecl{typ, mutable, init})
	return idx
}

// Element appends an active element segment placing funcs at consecutive
// table slots starting at offset.
func (m *Module) Element(offset uint32, funcs ...uint32) {
	m.elems = append(m.elems, elemSegment{offset, append([]uint32(nil), funcs...)})
}

// Data appends an active data segment (memory 0) at offset. The bytes are
// copied; declaring data requires Memory to be declared.
func (m *Module) Data(offset uint32, b []byte) {
	if m.memory == nil {
		panic("wasm: Data segment requires a declared Memory")
	}
	m.datas = append(m.datas, dataSegment{offset, append([]byte(nil), b...)})
}

// ExportFunc exports the function idx under name.
func (m *Module) ExportFunc(name string, idx uint32) {
	m.exports = append(m.exports, exportEntry{name, ExportKindFunc, idx})
}

// ExportMemory exports linear memory under name.
func (m *Module) ExportMemory(name string) {
	m.exports = append(m.exports, exportEntry{name, ExportKindMem, 0})
}

// Export exports this function under name.
func (f *Function) Export(name string) { f.m.ExportFunc(name, f.idx) }

// Start sets the start function.
func (m *Module) Start(idx uint32) { m.start, m.hasStart = idx, true }

// Idx returns the function's index in the module-wide function index space.
func (f *Function) Idx() uint32 { return f.idx }

// Encode assembles and returns the binary module.
func (m *Module) Encode() []byte {
	for _, f := range m.funcs {
		if f.depth != 0 {
			panic(fmt.Sprintf("wasm: function %d has %d unclosed block(s)", f.idx, f.depth))
		}
		if !f.terminated {
			panic(fmt.Sprintf("wasm: function %d not terminated by End", f.idx))
		}
	}
	out := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}

	// 1 Type
	if len(m.types) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.types)))
		for _, t := range m.types {
			c = append(c, 0x60)
			c = AppendU32(c, uint32(len(t.params)))
			for _, p := range t.params {
				c = append(c, byte(p))
			}
			c = AppendU32(c, uint32(len(t.results)))
			for _, r := range t.results {
				c = append(c, byte(r))
			}
		}
		out = appendSection(out, 1, c)
	}

	// 2 Import
	if len(m.imports) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.imports)))
		for _, im := range m.imports {
			c = AppendName(c, im.module)
			c = AppendName(c, im.name)
			c = append(c, 0x00) // func import
			c = AppendU32(c, im.typeIdx)
		}
		out = appendSection(out, 2, c)
	}

	// 3 Function
	if len(m.funcs) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.funcs)))
		for _, f := range m.funcs {
			c = AppendU32(c, f.typeIdx)
		}
		out = appendSection(out, 3, c)
	}

	// 4 Table
	if m.table != nil {
		var c []byte
		c = AppendU32(c, 1)
		c = append(c, 0x70) // funcref
		c = appendLimits(c, m.table.min, m.table.max)
		out = appendSection(out, 4, c)
	}

	// 5 Memory
	if m.memory != nil {
		var c []byte
		c = AppendU32(c, 1)
		c = appendLimits(c, m.memory.min, m.memory.max)
		out = appendSection(out, 5, c)
	}

	// 6 Global
	if len(m.globals) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.globals)))
		for _, g := range m.globals {
			c = append(c, byte(g.typ))
			if g.mutable {
				c = append(c, 0x01)
			} else {
				c = append(c, 0x00)
			}
			c = append(c, g.init...)
		}
		out = appendSection(out, 6, c)
	}

	// 7 Export
	if len(m.exports) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.exports)))
		for _, e := range m.exports {
			c = AppendName(c, e.name)
			c = append(c, e.kind)
			c = AppendU32(c, e.idx)
		}
		out = appendSection(out, 7, c)
	}

	// 8 Start
	if m.hasStart {
		out = appendSection(out, 8, AppendU32(nil, m.start))
	}

	// 9 Element
	if len(m.elems) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.elems)))
		for _, e := range m.elems {
			c = AppendU32(c, 0) // flags: active, table 0
			c = append(c, 0x41)
			c = AppendI32(c, int32(e.offset))
			c = append(c, 0x0B)
			c = AppendU32(c, uint32(len(e.funcs)))
			for _, f := range e.funcs {
				c = AppendU32(c, f)
			}
		}
		out = appendSection(out, 9, c)
	}

	// 10 Code
	if len(m.funcs) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.funcs)))
		for _, f := range m.funcs {
			var body []byte
			body = AppendU32(body, uint32(len(f.locals)))
			for _, g := range f.locals {
				body = AppendU32(body, g.count)
				body = append(body, byte(g.typ))
			}
			body = append(body, f.body...)
			c = AppendU32(c, uint32(len(body)))
			c = append(c, body...)
		}
		out = appendSection(out, 10, c)
	}

	// 11 Data
	if len(m.datas) > 0 {
		var c []byte
		c = AppendU32(c, uint32(len(m.datas)))
		for _, d := range m.datas {
			c = AppendU32(c, 0) // flags: active, memory 0
			c = append(c, 0x41)
			c = AppendI32(c, int32(d.offset))
			c = append(c, 0x0B)
			c = AppendU32(c, uint32(len(d.bytes)))
			c = append(c, d.bytes...)
		}
		out = appendSection(out, 11, c)
	}

	return out
}

func appendSection(out []byte, id byte, content []byte) []byte {
	out = append(out, id)
	out = AppendU32(out, uint32(len(content)))
	return append(out, content...)
}

func appendLimits(dst []byte, min, max uint32) []byte {
	if max == 0 {
		dst = append(dst, 0x00)
		return AppendU32(dst, min)
	}
	dst = append(dst, 0x01)
	dst = AppendU32(dst, min)
	return AppendU32(dst, max)
}

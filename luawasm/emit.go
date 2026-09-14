package luawasm

// The function emitter: lowers one FunctionProto's bytecode to a wasm
// function with flattened control flow (design doc §4.3/§4.4). Registers
// are TValue cells at frame+16*k; the frame is caller-allocated (the
// host allocates for lua_main via rt_frame_alloc, sized from the
// exported gFrameCells global).

import (
	"fmt"

	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/wasm"
)

const opMaxArgSbx = (1<<18 - 1) >> 1

type funcEmitter struct {
	b       *backend
	f       *wasm.Function
	proto   *lua.FunctionProto
	nregs   int
	code    []uint32

	// local indices (param 0 = frame)
	lTop, lBlk, lSt, lT0, lT1 uint32 // i32
	lVlo, lVhi                 uint32 // i64
	lF0                        uint32 // f64

	blocks  map[int]int // pc -> block id
	blockPC []int       // block id -> start pc
}

func (b *backend) newFuncEmitter() *funcEmitter {
	p := b.main
	f := b.m.NewFunction([]wasm.ValueType{wasm.I32}, []wasm.ValueType{wasm.I32})
	f.Local(wasm.I32).Local(wasm.I32).Local(wasm.I32).Local(wasm.I32).Local(wasm.I32) // 1..5
	f.Local(wasm.I64).Local(wasm.I64)                                               // 6,7
	f.Local(wasm.F64)                                                               // 8
	return &funcEmitter{
		b: b, f: f, proto: p,
		nregs: int(p.NumUsedRegisters),
		code:  p.Code,
		lTop: 1, lBlk: 2, lSt: 3, lT0: 4, lT1: 5,
		lVlo: 6, lVhi: 7, lF0: 8,
	}
}

/* ---------- small emission helpers ---------- */

func (fe *funcEmitter) cellAddr(k int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * k)).I32Add()
}

// scratch cell k — above the registers AND the 2-cell register margin
// (see the gFrameCells note in emitBody)
func (fe *funcEmitter) scratchAddr(k int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 2 + k))).I32Add()
}

// constant cell: i in Constants
func (fe *funcEmitter) kcell(i int) *wasm.Function {
	return fe.f.GlobalGet(fe.b.gKCells).I32Const(int32(cellSize * i)).I32Add()
}

// string-constant cell: i in stringConstants (offset by len(Constants))
func (fe *funcEmitter) kscell(i int) *wasm.Function {
	return fe.f.GlobalGet(fe.b.gKCells).I32Const(int32(cellSize * (len(fe.proto.Constants) + i))).I32Add()
}

// RK operand address: register or constant cell
func (fe *funcEmitter) rkAddr(v int) *wasm.Function {
	if v&0x100 != 0 {
		return fe.kcell(v &^ 0x100)
	}
	return fe.cellAddr(v)
}

// rkString operand address (the *KS opcodes): K bit → string-constant
// cell, else the register holds the string
func (fe *funcEmitter) rksAddr(v int) *wasm.Function {
	if v&0x100 != 0 {
		return fe.kscell(v &^ 0x100)
	}
	return fe.cellAddr(v)
}

// copyCell: 16-byte TValue copy (dst, src are address emitters)
func (fe *funcEmitter) copyCell(dst func(), src func()) {
	f := fe.f
	src()
	f.I64Load(0).LocalSet(fe.lVlo)
	src()
	f.I64Load(8).LocalSet(fe.lVhi)
	dst()
	f.LocalGet(fe.lVlo).I64Store(0)
	dst()
	f.LocalGet(fe.lVhi).I64Store(8)
}

// dynamic cell copy: base register + 16*t (t in lT1)
func (fe *funcEmitter) dynCellAddr(baseReg int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * baseReg)).I32Add().
		LocalGet(fe.lT1).I32Const(16).I32Mul().I32Add()
}

func (fe *funcEmitter) frameDynCell() *wasm.Function {
	return fe.f.LocalGet(0).LocalGet(fe.lT1).I32Const(16).I32Mul().I32Add()
}

// bumpTop: if top < n { top = n } (interpreter registry semantics)
func (fe *funcEmitter) bumpTop(n int) {
	f := fe.f
	f.LocalGet(fe.lTop).I32Const(int32(n)).I32LtS().
		If(wasm.Void).
		I32Const(int32(n)).LocalSet(fe.lTop).
		End()
}

// truth: leaves 1/0 on the stack — Lua truthiness of register k
// (false iff nil, or boolean with value.b == 0)
func (fe *funcEmitter) truth(reg int) {
	f := fe.f
	fe.cellAddr(reg)
	f.I32Load8U(8).I32Eqz() // tag == nil(0)
	fe.cellAddr(reg)
	f.I32Load8U(8).I32Const(1).I32Eq()
	fe.cellAddr(reg)
	f.I32Load(0).I32Eqz()
	f.I32And()
	f.I32Or()
}

// setBoolCellA: R(A) := boolean; cond is an address-less emitter leaving
// the 0/1 i32 on the stack
func (fe *funcEmitter) setBoolCellA(a int, cond func()) {
	f := fe.f
	cond()
	f.LocalSet(fe.lT0)
	fe.cellAddr(a)
	f.LocalGet(fe.lT0).I32Store(0)
	fe.cellAddr(a)
	f.I32Const(1).I32Store8(8)
}

// checkStatus: st := <status on stack>; if st == RT_ERR return 1
func (fe *funcEmitter) checkStatus() {
	f := fe.f
	f.LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().
		If(wasm.Void).
		I32Const(1).Return().
		End()
}

func (fe *funcEmitter) setBlk(id int) {
	fe.f.I32Const(int32(id)).LocalSet(fe.lBlk)
}

func (fe *funcEmitter) line(pc int) int32 {
	return int32(fe.proto.DbgSourcePositions[pc])
}

/* ---------- body ---------- */

func (fe *funcEmitter) emitBody() {
	f := fe.f

	// exported frame size: registers + 2 margin + 2 scratch cells. The
	// interpreter can write 2 cells past NumUsedRegisters (TFORLOOP stages
	// its triple at R(A+3..A+5), and the compiler doesn't always reserve
	// those); the margin absorbs that, and scratch lives above it so the
	// two windows can never alias.
	fe.b.m.ExportGlobal("gFrameCells", fe.b.m.GlobalI32(int32(fe.nregs+4), false))

	// nil-fill all registers (v1 entry: no arguments)
	for k := 0; k < fe.nregs; k++ {
		fe.cellAddr(k)
		f.I32Const(0).I32Store8(8)
	}
	f.I32Const(int32(fe.nregs)).LocalSet(fe.lTop)

	fe.partitionBlocks()

	// flattened dispatch. N+1 blocks: the innermost is a dummy so EVERY
	// body is enclosed by a dispatch block — copy-loops inside a body exit
	// with BrIf(1) to "the rest of this body", which must never resolve to
	// the dispatch LOOP itself (that would re-dispatch with a stale lBlk
	// and loop forever — the {1,2,3} constructor OOM).
	f.Loop(wasm.Void)
	for i := 0; i <= len(fe.blockPC); i++ {
		f.Block(wasm.Void)
	}
	depths := make([]uint32, len(fe.blockPC))
	for i := range depths {
		depths[i] = uint32(i)
	}
	f.LocalGet(fe.lBlk).BrTable(depths, uint32(len(fe.blockPC)-1))
	for k := 0; k < len(fe.blockPC); k++ {
		f.End() // close the dummy (k=0) / dispatch block N-1-k (k>0)
		start := fe.blockPC[k]
		end := len(fe.code)
		if k+1 < len(fe.blockPC) {
			end = fe.blockPC[k+1]
		}
		fe.emitBlockBody(start, end)
		f.Br(uint32(len(fe.blockPC) - k)) // → the dispatch loop (past B0..B(N-1-k))
	}
	f.End() // close B0
	f.End() // close loop
	f.I32Const(0)
	f.End() // unreachable; wasm requires a result
}

func (fe *funcEmitter) partitionBlocks() {
	leaders := map[int]bool{0: true}
	code := fe.code
	for pc := 0; pc < len(code); pc++ {
		inst := code[pc]
		switch int(inst >> 26) {
		case lua.OP_JMP, lua.OP_FORLOOP, lua.OP_FORPREP:
			leaders[fe.jumpTarget(pc)] = true
			leaders[pc+1] = true
		case lua.OP_EQ, lua.OP_LT, lua.OP_LE, lua.OP_TEST, lua.OP_TESTSET, lua.OP_TFORLOOP:
			leaders[pc+2] = true
			if pc+1 < len(code) && int(code[pc+1]>>26) == lua.OP_JMP {
				leaders[fe.jumpTarget(pc+1)] = true
			}
		case lua.OP_RETURN, lua.OP_TAILCALL:
			leaders[pc+1] = true
		case lua.OP_LOADBOOL:
			if int(inst>>9)&0x1ff != 0 { // C
				leaders[pc+2] = true
			}
		case lua.OP_MOVEN:
			pc += int(inst>>9) & 0x1ff // consume the C fused MOVEs
		}
	}
	ids := []int{}
	for pc := range leaders {
		if pc < len(code) {
			ids = append(ids, pc)
		}
	}
	sortInts(ids)
	fe.blocks = make(map[int]int, len(ids))
	fe.blockPC = ids
	for id, pc := range ids {
		fe.blocks[pc] = id
	}
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// jumpTarget: the VM increments Pc past the instruction, then pc += sBx.
func (fe *funcEmitter) jumpTarget(pc int) int {
	sbx := int(fe.code[pc]&0x3ffff) - opMaxArgSbx
	return pc + 1 + sbx
}

func (fe *funcEmitter) blockOf(pc int) int {
	if id, ok := fe.blocks[pc]; ok {
		return id
	}
	return len(fe.blockPC) - 1
}

func (fe *funcEmitter) emitBlockBody(start, end int) {
	code := fe.code
	terminal := false
	pc := start
	for pc < end {
		inst := code[pc]
		op := int(inst >> 26)
		A := int(inst>>18) & 0xff
		B := int(inst) & 0x1ff
		C := int(inst>>9) & 0x1ff
		Bx := int(inst) & 0x3ffff
		_ = Bx
		switch op {
		case lua.OP_MOVE:
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.cellAddr(B) })
			fe.bumpTop(A + 1)
		case lua.OP_MOVEN:
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.cellAddr(B) })
			fe.bumpTop(A + 1)
			for i := 0; i < C; i++ {
				pc++
				m := code[pc]
				fe.copyCell(func() { fe.cellAddr(int(m>>18) & 0xff) }, func() { fe.cellAddr(int(m) & 0x1ff) })
				fe.bumpTop(int(m>>18)&0xff + 1)
			}
		case lua.OP_LOADK:
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.kcell(Bx) })
			fe.bumpTop(A + 1)
		case lua.OP_LOADBOOL:
			fe.setBoolCellA(A, func() { fe.f.I32Const(int32(B)) })
			fe.bumpTop(A + 1)
			if C != 0 {
				fe.setBlk(fe.blockOf(pc + 2))
				terminal = true
			}
		case lua.OP_LOADNIL:
			for k := A; k <= B; k++ {
				fe.cellAddr(k)
				fe.f.I32Const(0).I32Store8(8)
			}
			fe.bumpTop(B + 1)
		case lua.OP_GETGLOBAL:
			fe.cellAddr(A)
			fe.kscell(Bx) // the interpreter keys globals from stringConstants
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_getglobal"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SETGLOBAL:
			fe.kscell(Bx)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_setglobal"))
			fe.checkStatus()
		case lua.OP_GETTABLE:
			// rt_gettable(tblcell, keycell, dstcell, line) — table first
			fe.cellAddr(B)
			fe.rkAddr(C)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_GETTABLEKS:
			// rt_gettable(tblcell, keycell, dstcell, line) — table first
			fe.cellAddr(B)
			fe.rksAddr(C) // rkString semantics: K bit → stringConstants, else register
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SETTABLE:
			fe.cellAddr(A)
			fe.rkAddr(B)
			fe.rkAddr(C)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_SETTABLEKS:
			fe.cellAddr(A)
			fe.rksAddr(B) // rkString semantics
			fe.rkAddr(C)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_NEWTABLE:
			fe.cellAddr(A)
			fe.f.I32Const(0).I32Const(0)
			fe.f.Call(fe.b.imp("rt_newtable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SELF:
			fe.copyCell(func() { fe.cellAddr(A + 1) }, func() { fe.cellAddr(B) })
			fe.cellAddr(B)
			fe.rkAddr(C)
			fe.scratchAddr(0)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.scratchAddr(0) })
			fe.bumpTop(A + 2)
		case lua.OP_ADD, lua.OP_SUB, lua.OP_MUL, lua.OP_DIV, lua.OP_MOD, lua.OP_POW:
			fe.emitArith(op, A, B, C, pc)
		case lua.OP_UNM:
			fe.emitUnm(A, B, pc)
		case lua.OP_NOT:
			// not = truth inverted
			fe.setBoolCellA(A, func() { fe.truth(B) })
			fe.bumpTop(A + 1)
		case lua.OP_LEN:
			fe.cellAddr(B)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_len"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_CONCAT:
			fe.cellAddr(B) // cells base
			fe.f.I32Const(int32(C - B + 1))
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_concat"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_JMP:
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
			terminal = true
		case lua.OP_EQ, lua.OP_LT, lua.OP_LE:
			fe.emitCompare(op, A, B, C, pc)
			terminal = true
		case lua.OP_TEST:
			// if truth(R(A)) == C, the following JMP executes
			fe.truth(A)
			fe.f.LocalSet(fe.lT0)
			fe.f.LocalGet(fe.lT0).I32Const(int32(C)).I32Eq().If(wasm.Void)
			// falsiness == C ⟺ truthiness == (C==0): pc++ (skip the JMP)
			fe.setBlk(fe.blockOf(pc + 2))
			fe.f.Else()
			// the JMP runs → its target
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc + 1)))
			fe.f.End()
			terminal = true
		case lua.OP_TESTSET:
			fe.truth(B)
			fe.f.LocalSet(fe.lT0)
			fe.f.LocalGet(fe.lT0).I32Const(int32(C)).I32Ne().If(wasm.Void)
			// falsiness != C ⟺ truthiness != (C==0): R(A) := R(B), JMP runs
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.cellAddr(B) })
			fe.bumpTop(A + 1)
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc + 1)))
			fe.f.Else()
			// falsiness == C: pc++ (skip the JMP) → pc+2
			fe.setBlk(fe.blockOf(pc + 2))
			fe.f.End()
			terminal = true
		case lua.OP_CALL:
			fe.emitCall(A, B, C, pc, false)
		case lua.OP_TAILCALL:
			fe.emitCall(A, B, 0, pc, true)
			terminal = true
		case lua.OP_RETURN:
			fe.emitReturn(A, B)
			terminal = true
		case lua.OP_FORLOOP:
			fe.emitForloop(A, pc)
			terminal = true
		case lua.OP_FORPREP:
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_forprep"))
			fe.checkStatus()
			// init -= step
			fe.cellAddr(A)
			fe.cellAddr(A).F64Load(0)
			fe.cellAddr(A + 2).F64Load(0)
			fe.f.F64Sub().F64Store(0)
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
			terminal = true
		case lua.OP_TFORLOOP:
			fe.emitTForloop(A, C, pc)
			terminal = true
		case lua.OP_SETLIST:
			fe.emitSetlist(A, B, C, pc)
		case lua.OP_CLOSE, lua.OP_NOP:
			// no closures in v1: CLOSE is a no-op
		default:
			panic(fmt.Sprintf("luawasm: unhandled opcode %d", op))
		}
		pc++
		if terminal {
			// the instruction routed all successors itself (JMP, branches,
			// TFORLOOP's pseudo-JMP, returns) — emitting anything after it
			// would overwrite lBlk at runtime
			break
		}
	}
	if !terminal {
		fe.setBlk(fe.blockOf(end))
	}
}

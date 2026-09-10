package luawasm

// Opcode emitters for the trickier lowers: arithmetic (inline number
// fast path + ABI fallback), comparisons, calls, returns, the numeric
// for loop, TFORLOOP and SETLIST (design doc §4.4).

import (
	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/wasm"
)

// emitArith: R(A) := RK(B) <op> RK(C). Inline fast path when both
// operands are registers holding numbers; ABI otherwise (string
// coercion, metamethods, errors).
func (fe *funcEmitter) emitArith(op, A, B, C, pc int) {
	f := fe.f
	bothRegs := B&0x100 == 0 && C&0x100 == 0
	fastable := op == lua.OP_ADD || op == lua.OP_SUB || op == lua.OP_MUL || op == lua.OP_DIV
	if bothRegs && fastable {
		fe.cellAddr(B)
		f.I32Load8U(8).I32Const(rtNumTag).I32Eq()
		fe.cellAddr(C)
		f.I32Load8U(8).I32Const(rtNumTag).I32Eq()
		f.I32And().If(wasm.Void)
		// fast: f64 op inline into R(A)
		fe.cellAddr(A)
		fe.cellAddr(B).F64Load(0)
		fe.cellAddr(C).F64Load(0)
		fe.arithF64(op)
		f.F64Store(0)
		fe.cellAddr(A)
		f.I32Const(rtNumTag).I32Store8(8)
		fe.bumpTop(A + 1)
		f.Else()
		f.I32Const(int32(op))
		fe.rkAddr(B)
		fe.rkAddr(C)
		fe.cellAddr(A)
		f.I32Const(fe.line(pc))
		f.Call(fe.b.imp("rt_arith"))
		fe.checkStatus()
		fe.bumpTop(A + 1)
		f.End()
		return
	}
	f.I32Const(int32(op))
	fe.rkAddr(B)
	fe.rkAddr(C)
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(A + 1)
}

func (fe *funcEmitter) arithF64(op int) {
	f := fe.f
	switch op {
	case lua.OP_ADD:
		f.F64Add()
	case lua.OP_SUB:
		f.F64Sub()
	case lua.OP_MUL:
		f.F64Mul()
	case lua.OP_DIV:
		f.F64Div()
	}
}

// emitUnm: R(A) := -R(B)
func (fe *funcEmitter) emitUnm(A, B, pc int) {
	f := fe.f
	fe.cellAddr(B)
	f.I32Load8U(8).I32Const(rtNumTag).I32Eq().If(wasm.Void)
	fe.cellAddr(A)
	fe.cellAddr(B).F64Load(0)
	f.F64Neg().F64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtNumTag).I32Store8(8)
	fe.bumpTop(A + 1)
	f.Else()
	f.I32Const(int32(lua.OP_UNM))
	fe.cellAddr(B)
	fe.cellAddr(B)
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(A + 1)
	f.End()
}

// emitCompare: EQ/LT/LE fused with the following JMP. cond via the ABI
// (handles mixed types, strings, metamethods) into a scratch cell.
func (fe *funcEmitter) emitCompare(op, A, B, C, pc int) {
	f := fe.f
	var fn string
	switch op {
	case lua.OP_EQ:
		fn = "rt_eq"
	case lua.OP_LT:
		fn = "rt_lt"
	default:
		fn = "rt_le"
	}
	fe.scratchAddr(0)
	fe.rkAddr(B)
	fe.rkAddr(C)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp(fn))
	fe.checkStatus()
	// if (cond ~= A) then pc++ (skip the JMP) else take the JMP
	fe.scratchAddr(0)
	f.I32Load(0)
	f.I32Const(int32(A)).I32Ne().If(wasm.Void)
	fe.setBlk(fe.blockOf(pc + 2))
	f.Else()
	fe.setBlk(fe.blockOf(fe.jumpTarget(pc + 1)))
	f.End()
}

// emitCall: OP_CALL (tailcall=false) and OP_TAILCALL (tailcall=true).
// The callee executes through rt_call — the runtime's lvm runs Lua
// closures and C functions alike.
func (fe *funcEmitter) emitCall(A, B, C, pc int, tail bool) {
	f := fe.f
	// nargs
	if B == 0 {
		f.LocalGet(fe.lTop).I32Const(int32(A + 1)).I32Sub()
	} else {
		f.I32Const(int32(B - 1))
	}
	f.LocalSet(fe.lT0)
	// want
	if C == 0 {
		f.I32Const(-1)
	} else {
		f.I32Const(int32(C - 1))
	}
	f.LocalSet(fe.lT1)
	// rt_call(&R(A), &R(A+1), nargs, want, line): the callee occupies R(A),
	// arguments start at R(A+1), and results are written back over the
	// argument cells (then the caller moves them where they belong)
	fe.cellAddr(A)
	fe.cellAddr(A + 1)
	f.LocalGet(fe.lT0)
	f.LocalGet(fe.lT1)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_call")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(1).Return()
	f.End()

	if tail {
		// results (count = rt_call_count(status)) copied to the frame
		// base, then returned — a real tailcall optimization lands with
		// the v2 structured emitter
		f.LocalGet(fe.lSt).Call(fe.b.imp("rt_call_count")).LocalSet(fe.lT0)
		f.I32Const(0).LocalSet(fe.lT1)
		f.Loop(wasm.Void)
		f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
		fe.copyDyn(A)
		f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
		f.Br(0)
		f.End()
		f.LocalGet(fe.lT0).Return()
		return
	}

	// adjust top: want<0 → A + count; else A + (C-1)
	if C == 0 {
		f.LocalGet(fe.lSt).Call(fe.b.imp("rt_call_count"))
		f.I32Const(int32(A)).I32Add().LocalSet(fe.lTop)
	} else {
		f.I32Const(int32(A + C - 1)).LocalSet(fe.lTop)
	}
}

// copyDyn: full 16-byte copy of dynCellAddr(A) → frameDynCell()
func (fe *funcEmitter) copyDyn(baseReg int) {
	f := fe.f
	fe.dynCellAddr(baseReg)
	f.I64Load(0).LocalSet(fe.lVlo)
	fe.dynCellAddr(baseReg)
	f.I64Load(8).LocalSet(fe.lVhi)
	fe.frameDynCell()
	f.LocalGet(fe.lVlo).I64Store(0)
	fe.frameDynCell()
	f.LocalGet(fe.lVhi).I64Store(8)
}

// emitReturn: results at R(A); B==0 → up to top.
func (fe *funcEmitter) emitReturn(A, B int) {
	f := fe.f
	if B >= 2 {
		n := B - 1
		for i := 0; i < n; i++ {
			fe.frameCellAddrConst(i)
			fe.cellAddr(A + i).I64Load(0)
			f.I64Store(0)
			fe.frameCellAddrConst(i)
			fe.cellAddr(A + i).I64Load(8)
			f.I64Store(8)
		}
		f.I32Const(int32(n)).Return()
		return
	}
	if B == 1 {
		f.I32Const(0).Return()
		return
	}
	// B == 0: count = top - A, dynamic copy
	f.LocalGet(fe.lTop).I32Const(int32(A)).I32Sub().LocalSet(fe.lT0)
	f.I32Const(0).LocalSet(fe.lT1)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
	fe.copyDyn(A)
	f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
	f.Br(0)
	f.End()
	f.LocalGet(fe.lT0).Return()
}

func (fe *funcEmitter) frameCellAddrConst(i int) {
	fe.f.I32Const(int32(cellSize * i))
}

// emitForloop: init += step (cells); if (step>0 && init<=limit) ||
// (step<=0 && init>=limit) then R(A+3)=init, jump back; else fall through.
func (fe *funcEmitter) emitForloop(A, pc int) {
	f := fe.f
	// f0 = init + step; store to R(A)
	fe.cellAddr(A)
	fe.cellAddr(A).F64Load(0)
	fe.cellAddr(A + 2).F64Load(0)
	f.F64Add().LocalSet(fe.lF0)
	fe.cellAddr(A)
	f.LocalGet(fe.lF0).F64Store(0)
	// cond
	fe.cellAddr(A + 2).F64Load(0).F64Const(0).F64Gt()
	fe.cellAddr(A).F64Load(0)
	fe.cellAddr(A + 1).F64Load(0)
	f.F64Le()
	f.I32And()
	fe.cellAddr(A + 2).F64Load(0).F64Const(0).F64Le()
	fe.cellAddr(A).F64Load(0)
	fe.cellAddr(A + 1).F64Load(0)
	f.F64Ge()
	f.I32And()
	f.I32Or().If(wasm.Void)
	fe.cellAddr(A + 3)
	f.LocalGet(fe.lF0).F64Store(0)
	fe.cellAddr(A + 3)
	f.I32Const(3).I32Store8(8) // loop var is a number: set the tag too
	fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
	f.Else()
	fe.setBlk(fe.blockOf(pc + 1))
	f.End()
}

// emitTForloop: results of the iterator call at R(A+3)..; on non-nil
// control, skip the following JMP (loop body runs).
func (fe *funcEmitter) emitTForloop(A, C, pc int) {
	f := fe.f
	// call R(A)(R(A+1), R(A+2)) → C results at R(A+3)
	fe.cellAddr(A)
	fe.cellAddr(A + 1)
	f.I32Const(2)
	f.I32Const(int32(C))
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_call")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(1).Return()
	f.End()
	// results landed at R(A+1); move C cells to R(A+3)
	for i := 0; i < C; i++ {
		fe.copyCell(func() { fe.cellAddr(A + 3 + i) }, func() { fe.cellAddr(A + 1 + i) })
	}
	// if R(A+3) ~= nil → pc++ (skip JMP) and R(A+2)=R(A+3)
	fe.cellAddr(A + 3)
	f.I32Load8U(8).I32Const(0).I32Ne().If(wasm.Void)
	fe.copyCell(func() { fe.cellAddr(A + 2) }, func() { fe.cellAddr(A + 3) })
	fe.setBlk(fe.blockOf(pc + 2))
	f.Else()
	fe.setBlk(fe.blockOf(fe.jumpTarget(pc + 1)))
	f.End()
}

// emitSetlist: R(A)[base+i+1] = R(A+1+i) for i in 0..count-1
func (fe *funcEmitter) emitSetlist(A, B, C, pc int) {
	f := fe.f
	base := (C - 1) * 50
	if B == 0 {
		f.LocalGet(fe.lTop).I32Const(int32(A + 1)).I32Sub().LocalSet(fe.lT0)
	} else {
		f.I32Const(int32(B)).LocalSet(fe.lT0)
	}
	f.I32Const(0).LocalSet(fe.lT1)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
	// key = mknumber(base + i + 1) into scratch 0
	fe.scratchAddr(0)
	f.LocalGet(fe.lT1).I32Const(int32(base + 1)).I32Add().F64ConvertI32S()
	f.Call(fe.b.imp("rt_mknumber"))
	// rt_settable(R(A), scratch0, R(A+1+i), line)
	fe.cellAddr(A)
	fe.scratchAddr(0)
	fe.dynCellAddr(A + 1)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_settable")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(1).Return()
	f.End()
	f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
	f.Br(0)
	f.End()
}

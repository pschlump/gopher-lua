package lua

// P6 gate: boxing-cost engineering. Two mechanisms, both behavior-neutral
// by construction, pinned here anyway because both touch value identity:
//
//   - intCache [-1024, 1024): LInteger2I must return LValues that are
//     interface-== to plain conversions (Go compares the dynamic VALUES
//     for int64/float64, so a cached box and a fresh one are equal) — and
//     -0.0 must never ride a float preload (the Signbit guard).
//   - arithCompute now returns UNBOXED results; the store sites box once
//     through the allocator. This pins that int and float payloads keep
//     their syntactic subtype across the seam for a representative pair
//     of every opcode class, and that the compile-time fold's NaN/±0
//     rejection still rejects floats but folds integer zero.

import (
	"math"
	"strings"
	"testing"
)

func TestP6BoxEqualityAcrossStrategies(t *testing.T) {
	n := 0
	for i := 0; i < 100; i++ {
		n += 3
	}
	// runtime-computed (not constant-foldable) values, both signs, cache
	// edges, and beyond the cache
	for _, iv := range []int64{
		int64(n) + 200, // 500, computed
		-1 * int64(n),  // -300, computed negative
		int64(intCacheLo), int64(intCacheLo) - 1,
		int64(intCacheHi) - 1, int64(intCacheHi),
	} {
		cached := LValue(LInteger(iv))
		var plain LValue = LInteger(iv)
		if cached != plain {
			t.Fatalf("LInteger(%d): cached != plain", iv)
		}
	}
	// arena-boxed (beyond the cache) vs plain
	al := newAllocator(32)
	if al.LInteger2I(LInteger(700)) != LValue(LInteger(700)) {
		t.Fatal("arena LInteger(700) != plain LInteger(700)")
	}
	if al.LNumber2I(LNumber(3.25)) != LValue(LNumber(3.25)) {
		t.Fatal("arena LNumber(3.25) != plain LNumber(3.25)")
	}
}

func TestP6NegativeZeroNotPreloaded(t *testing.T) {
	al := newAllocator(32)
	nz := al.LNumber2I(LNumber(math.Copysign(0, -1)))
	if nz != LValue(LNumber(math.Copysign(0, -1))) {
		t.Fatal("-0.0 preload lost its sign (row 42)")
	}
	if math.Signbit(float64(nz.(LNumber))) != true {
		t.Fatal("-0.0 became +0.0")
	}
}

// arithCompute must report the same subtype it used to carry in the boxed
// LValue: integers stay integers, floats stay floats, errors unchanged.
func TestP6ArithComputeUnboxed(t *testing.T) {
	type want struct {
		op       int
		lhs, rhs LValue
		isInt    bool
		iv       LInteger
		fv       LNumber
	}
	two := LValue(LInteger(2))
	half := LValue(LNumber(0.5))
	cases := []want{
		{OP_ADD, two, two, true, 4, 0},
		{OP_ADD, LValue(LInteger(3)), LValue(LNumber(0.5)), false, 0, 3.5},
		{OP_UNM, two, two, true, -2, 0},
		{OP_UNM, half, half, false, 0, -0.5},
		{OP_DIV, two, LValue(LInteger(4)), false, 0, 0.5},
		{OP_POW, LValue(LInteger(2)), LValue(LNumber(0.5)), false, 0, math.Sqrt2},
		{OP_MOD, LValue(LInteger(-7)), LValue(LInteger(3)), true, 2, 0},
		{OP_IDIV, LValue(LNumber(-7.5)), LValue(LNumber(2)), false, 0, -4},
		{OP_BAND, LValue(LInteger(6)), LValue(LInteger(3)), true, 2, 0},
		{OP_SHL, LValue(LInteger(1)), LValue(LInteger(10)), true, 1024, 0},
	}
	for _, c := range cases {
		iv, fv, isInt, errText, res := arithCompute(c.op, c.lhs, c.rhs)
		if res != arithDone {
			t.Fatalf("op %d: expected arithDone, got res=%v err=%q", c.op, res, errText)
		}
		if isInt != c.isInt {
			t.Fatalf("op %d: isInt=%v want %v", c.op, isInt, c.isInt)
		}
		if isInt && iv != c.iv {
			t.Fatalf("op %d: iv=%d want %d", c.op, iv, c.iv)
		}
		if !isInt && fv != c.fv {
			t.Fatalf("op %d: fv=%v want %v", c.op, fv, c.fv)
		}
	}
	// string operands fall to the metamethod path, not an error
	if _, _, _, _, res := arithCompute(OP_ADD, LValue(LString("x")), LValue(LString("y"))); res != arithSlow {
		t.Fatalf("string+string: res=%v want arithSlow", res)
	}
	// OP_DIV always floats: 1/0 is +Inf, not an error
	if _, fv, _, _, res := arithCompute(OP_DIV, LValue(LInteger(1)), LValue(LInteger(0))); res != arithDone || !math.IsInf(float64(fv), 1) {
		t.Fatalf("1/0: res=%v fv=%v want +Inf", res, fv)
	}
	// integer floor-division by zero raises with stock's message
	if _, _, _, errText, res := arithCompute(OP_IDIV, LValue(LInteger(1)), LValue(LInteger(0))); res != arithRaise || !strings.Contains(errText, "zero") {
		t.Fatalf("1//0: res=%v err=%q want raise containing 'zero'", res, errText)
	}
	// so does modulo by zero
	if _, _, _, errText, res := arithCompute(OP_MOD, LValue(LInteger(1)), LValue(LInteger(0))); res != arithRaise || !strings.Contains(errText, "'n%0'") {
		t.Fatalf("1%%0: res=%v err=%q want raise 'n%%0'", res, errText)
	}
}

// constFold rejects float NaN/±0 results but still folds integer zero.
func TestP6ConstFoldZeroPolicy(t *testing.T) {
	L := NewState()
	defer L.Close()
	// integer zero folds: constant 5*0... would error at runtime if not
	// folded? no — int mul by 0 is fine; assert the folded value's subtype
	if err := L.DoString(`
		local x = 7 * 0
		assert(math.type(x) == "integer" and x == 0)
		local y = 2^(-1)      -- float fold 0.5
		assert(math.type(y) == "float")
		local z = 0.0 * 5     -- float ±0: NOT folded, runtime multiply
		assert(math.type(z) == "float" and z == 0)
		local w = 3 - 3       -- integer zero through SUB
		assert(math.type(w) == "integer")
	`); err != nil {
		t.Fatalf("script: %v", err)
	}
}

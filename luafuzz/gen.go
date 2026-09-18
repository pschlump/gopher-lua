// Package luafuzz is the M6e differential fuzzer: a deterministic Lua
// program generator (design doc §8.4 L5) plus an interruptible,
// chunk-resumable soak runner (m6 plan §4.5/D6).
//
// Generator contract: Generate(masterSeed, idx) is a pure function —
// the same (seed, idx) always produces byte-identical source. A soak
// therefore never needs to store generated programs; resuming is
// "continue at the next index".
//
// Programs are valid Lua 5.1 by construction (one statement per line,
// block keywords balanced) and are additionally gated through the
// repo's own parser before reaching any engine; a parse failure is a
// GENBUG finding (generator bug), never an engine result.
//
// Ledger-driven avoidance list — corners the generator deliberately
// does NOT emit, each pinned to its divergence-ledger row. Extending
// the surface means extending this list's counterpart rows first:
//
//	row 20: recursion depth ≤ 50 (the wasm adapter guard fires at 150
//	        frames vs the interp harness's 1024 — depth itself diverges)
//	row 22: no coroutines
//	row 23: no debug library
//	row 24: no load/loadstring/dofile/loadfile/require
//	row 25: library calls are valid-by-construction only (%d values
//	        < 2^31, tonumber over plain-integer strings only, no
//	        zero-arg math.max, no string.dump, select index ≥ 1)
//	row 33: no io/os surface (prod sandbox nils it; dev shim covers it)
//	row 38: no non-finite number→string coercion (tostring/.. of
//	        ±inf/nan; math.huge is finite in the fork, infinite in C)
//	row 39: pairs only over dense sequences (hash-part iteration order
//	        differs: interp sorted vs C ltable slot order)
//	§8.2:   # only over dense sequences (border semantics unspecified
//	        with holes/trailing nils)
//
// The M6d deadline is armed on every run as the safety net: guarded
// loops should never need it, a both-engine expiry is DL-TIE (neutral),
// a one-engine expiry is a HANG finding.
package luafuzz

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"

	"github.com/pschlump/gopher-lua/parse"
)

// caseRNG derives a per-case deterministic RNG stream from the soak's
// master seed and the case index (splitmix64-style mixing). math/rand
// streams are stable for a given Go toolchain; replaying a seed on a
// different major Go version may shift cases (documented, acceptable —
// findings persist their .lua regardless).
func caseRNG(master int64, idx int) *rand.Rand {
	z := uint64(master) + uint64(idx)*0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return rand.New(rand.NewSource(int64(z ^ (z >> 31))))
}

// CaseName is the chunkname for a case (it appears inside error
// position prefixes, so it is part of the differential contract).
func CaseName(idx int) string { return fmt.Sprintf("c%07d.lua", idx) }

// Generate produces the source of case idx under masterSeed.
func Generate(masterSeed int64, idx int) []byte {
	g := &gen{
		rnd:     caseRNG(masterSeed, idx),
		seed:    masterSeed,
		idx:     idx,
		budget:  18 + caseRNG(masterSeed, idx^0x51).Intn(20), // 18–37 statements
		maxLine: 120,
	}
	g.linef("-- luafuzz case=%d seed=%d (valid Lua 5.1 by construction)", idx, masterSeed)
	g.linef("-- repro: luafuzz -replay -seed %d -from %d -to %d", masterSeed, idx, idx)
	g.defs()
	for g.budget > 0 {
		g.stmt(true)
	}
	// Occasionally end with a bare uncaught core error: drives the
	// ERROR event + uncaught-unwind path with a full preceding log.
	if g.rnd.Intn(100) < 12 {
		g.bareRisk()
	}
	return g.render()
}

type tblInfo struct {
	name  string
	dense bool // dense sequence: constructor 1..n or insert-built (#, pairs, ipairs safe)
	hasMT bool
}

type fnInfo struct {
	name   string
	arity  int
	vararg bool
	multi  bool // returns 2+ values (or passthrough) → multret callsites
}

type gen struct {
	rnd     *rand.Rand
	seed    int64
	idx     int
	out     []string
	ind     string
	budget  int
	maxLine int

	nScalar int
	nTable  int
	nFunc   int
	nGuard  int
	nMT     int
	nFor    int

	scalars []string
	tables  []tblInfo
	funcs   []fnInfo
}

// ---- emit helpers ----

func (g *gen) linef(format string, args ...any) {
	g.out = append(g.out, g.ind+fmt.Sprintf(format, args...))
}

func (g *gen) render() []byte {
	return []byte(strings.Join(g.out, "\n") + "\n")
}

func (g *gen) push() { g.ind += "  " }
func (g *gen) pop()  { g.ind = g.ind[:len(g.ind)-2] }

// ---- lexical scoping ----
//
// The name registries must mirror Lua's blocks: a local declared
// inside a block is invisible after its `end`. Referencing an
// out-of-scope name compiles fine but reads a nil GLOBAL — which is
// how the first wasm-mini leg found `pairs(nil)` and
// math.max(nil)-shaped library arg errors (row 25 wording territory).
// Blocks snapshot the registries and truncate on exit; names are
// globally unique counters, so truncation is exact restoration.

type scopeMark struct{ ns, nt, nf int }

func (g *gen) mark() scopeMark {
	return scopeMark{len(g.scalars), len(g.tables), len(g.funcs)}
}

func (g *gen) restore(m scopeMark) {
	g.scalars = g.scalars[:m.ns]
	g.tables = g.tables[:m.nt]
	g.funcs = g.funcs[:m.nf]
}

// ---- pools (slices only — iteration order feeds output; never maps) ----

var numPool = []string{
	"0", "1", "2", "3", "5", "7", "10", "42", "100", "255", "256", "1000",
	"65536", "0.5", "-1", "-2", "-2.5", "1e3", "1e15", "1e-9",
	"0.1", "3.5", "1e308", "9007199254740993",
	// -0.0 removed (row 42): any -0.0 constant in the chunk corrupts
	// metamethod-arith constant reads until the backend fix lands
	// (pinned by _wasm-tests/nz00.lua)
}

// intPool: %d-format-safe integers (< 2^31, row 25) and loop bounds.
var intPool = []string{"0", "1", "2", "3", "4", "5", "7", "10", "12", "20", "42", "99", "255"}

var numStrPool = []string{`"10"`, `"2"`, `"100"`, `"-3"`, `"3.5"`} // arith-coercible strings

var strPool = []string{`"abc"`, `"x"`, `""`, `"hello"`, `"A"`, `"hello world"`, `"10"`, `"lua"`}

var identPool = []string{"zz", "k1", "name", "Q"} // table string keys

// posIntPool: math.random bounds (Intn(≤0) panics Go's rand).
var posIntPool = []string{"1", "2", "3", "6", "10", "42"}

// safeNumStrPool: numeric literals whose number→string coercion is
// byte-identical across engines (empirically probed, M6e bring-up):
// the fork prints integer-valued floats as full integers while C's
// %.14g switches to exponent form past 14 significant digits, and
// int64(-0.0) drops the sign — so 1e15, 2^53, 1e308 and -0.0 stay out
// of `..`/tostring operand position (row 38).
var safeNumStrPool = []string{
	"0", "1", "2", "3", "5", "7", "10", "42", "100", "255", "256", "1000",
	"65536", "0.5", "-1", "-2", "-2.5", "0.1", "3.5", "1e-9",
}

// lit: a non-nil literal. Sequence-critical table values (array parts,
// table.insert args) must be non-nil BY CONSTRUCTION — a nil would
// open a hole and #/pairs/ipairs would cross into unspecified border
// semantics (§8.2) or hash-order divergence (row 39). Numbers come
// from the coercion-safe pool: table values reach tostring() via
// str()'s tostring(t[k]) case.
func (g *gen) lit() string {
	switch g.rnd.Intn(3) {
	case 0:
		return g.pick(safeNumStrPool)
	case 1:
		return g.pick(strPool)
	default:
		return g.pick([]string{"true", "false"})
	}
}

func (g *gen) pick(pool []string) string { return pool[g.rnd.Intn(len(pool))] }

// fmtSpec pairs a format string with matching argument count (row 25:
// a missing format arg is undefined behavior in C).
type fmtSpec struct {
	f string
	n int
}

var fmtSpecs = []fmtSpec{{"%s", 1}, {"%s-%s", 2}, {"[%s]", 1}, {"%q", 1}, {"%5s", 1}}

// ---- names ----

func (g *gen) newScalar() string { g.nScalar++; return fmt.Sprintf("v%d", g.nScalar-1) }
func (g *gen) newTableN() string { g.nTable++; return fmt.Sprintf("t%d", g.nTable-1) }
func (g *gen) newFunc() string   { g.nFunc++; return fmt.Sprintf("f%d", g.nFunc-1) }
func (g *gen) newGuard() string  { g.nGuard++; return fmt.Sprintf("gd%d", g.nGuard-1) }
func (g *gen) newMT() string     { g.nMT++; return fmt.Sprintf("mt%d", g.nMT-1) }
func (g *gen) newFor() string    { g.nFor++; return fmt.Sprintf("i%d", g.nFor-1) }

// ---- expressions ----

// num: a number-valued (or arithmetic-coercible) expression.
func (g *gen) num(d int) string {
	if d <= 0 {
		switch r := g.rnd.Intn(10); {
		case r < 7:
			return g.pick(numPool)
		case r < 9:
			return g.pick(numStrPool)
		default:
			if len(g.scalars) > 0 {
				return g.pick(g.scalars)
			}
			return g.pick(numPool)
		}
	}
	switch r := g.rnd.Intn(20); {
	case r < 6:
		op := g.pick([]string{"+", "-", "*", "/", "%", "^"})
		l, rr := g.arithOperand(), g.arithOperand()
		l, rr = noFoldNegZero(op, l, rr)
		return fmt.Sprintf("(%s %s %s)", l, op, rr)
	case r < 8:
		// operand parenthesized: a pool literal like "-2" after the
		// unary minus would render "(--2)" — a comment, not an expr;
		// and a zero operand would fold to a -0.0 constant (row 42)
		for {
			inner := g.num(d - 1)
			if inner != "0" && inner != "0.0" {
				return fmt.Sprintf("(-(%s))", inner)
			}
		}
	case r < 10:
		if len(g.tables) > 0 {
			t := g.pickT()
			return fmt.Sprintf("%s[%s]", t.name, g.pick(intPool))
		}
		return g.pick(numPool)
	case r < 12:
		fn := g.pickF()
		if fn == nil {
			return g.pick(numPool)
		}
		return g.call(fn)
	case r < 14:
		return fmt.Sprintf("math.%s(%s, %s)", g.pick([]string{"max", "min"}),
			g.pick(numPool), g.pick(numPool))
	case r < 15:
		return fmt.Sprintf("math.%s(%s)", g.pick([]string{"floor", "ceil", "abs"}), g.pick(numPool))
	case r < 16:
		return fmt.Sprintf("math.fmod(%s, %s)", g.pick(numPool), g.pick([]string{"1", "2", "3", "7"}))
	case r < 17:
		return fmt.Sprintf("math.random(%s)", g.pick(posIntPool))
	case r < 18:
		return fmt.Sprintf("tonumber(%s)", g.pick([]string{`"10"`, `"-3"`, `"abc"`, `""`}))
	default:
		return g.pick(numPool)
	}
}

// arithOperand: binary-arith operands are restricted to numeric
// literals, coercible-string literals, and always-numeric lib calls —
// never plain scalar locals / t[k] reads / arbitrary calls, which can
// hold non-coercible strings. The error-text corner "non-coercible
// string LHS × coercible string RHS" (interp names the RHS after its
// coercion, C names the raw type — row 40) is unreachable this way;
// arith error paths keep coverage through riskyExpr's deterministic
// raises ("x" + 1, {} + 1, …).
func (g *gen) arithOperand() string {
	switch g.rnd.Intn(10) {
	case 0:
		return g.pick(numStrPool)
	case 1:
		return fmt.Sprintf("math.%s(%s, %s)", g.pick([]string{"max", "min"}),
			g.pick(numPool), g.pick(numPool))
	case 2:
		return fmt.Sprintf("math.%s(%s)", g.pick([]string{"floor", "ceil", "abs"}), g.pick(numPool))
	case 3:
		return fmt.Sprintf("math.random(%s)", g.pick(posIntPool))
	default:
		return g.pick(numPool)
	}
}

// noFoldNegZero: constFold (compile.go) evaluates constant arith at
// compile time — `0 * -1` / `0 % -1` would fold to a -0.0 CONSTANT,
// and any -0.0 constant in the pool trips row 42. Pairing a zero
// literal with a negative literal under * / % / ^ is rewritten to a
// positive operand (folding to +0 instead).
func noFoldNegZero(op, l, r string) (string, string) {
	switch op {
	case "*", "%", "^":
		zr := func(s string) bool { return s == "0" || s == "0.0" || s == "-0" }
		if zr(l) && strings.HasPrefix(r, "-") {
			return l, strings.TrimPrefix(r, "-")
		}
		if zr(r) && strings.HasPrefix(l, "-") {
			return strings.TrimPrefix(l, "-"), r
		}
	}
	return l, r
}

// str: a string-valued expression.
func (g *gen) str(d int) string {
	if d <= 0 || g.rnd.Intn(3) == 0 {
		return g.pick(strPool)
	}
	switch r := g.rnd.Intn(12); {
	case r < 3:
		return fmt.Sprintf("(%s .. %s)", g.concatOperand(), g.concatOperand())
	case r < 4:
		sp := g.pickFmt()
		args := make([]string, sp.n)
		for i := range args {
			args[i] = g.pick(strPool)
		}
		return fmt.Sprintf("string.format(%q, %s)", sp.f, strings.Join(args, ", "))
	case r < 5:
		return fmt.Sprintf("string.sub(%s, %s)", g.pick(strPool), g.pick(intPool))
	case r < 6:
		return fmt.Sprintf("string.rep(%s, %s)", g.pick(strPool), g.pick([]string{"1", "2", "3"}))
	case r < 7:
		return fmt.Sprintf("string.upper(%s)", g.pick(strPool))
	case r < 8:
		return fmt.Sprintf("string.reverse(%s)", g.pick(strPool))
	case r < 9:
		if len(g.tables) > 0 {
			t := g.pickT()
			return fmt.Sprintf("tostring(%s[%s])", t.name, g.pick(intPool))
		}
		return g.pick(strPool)
	case r < 10:
		return fmt.Sprintf("tostring(%s)", g.pick(safeNumStrPool))
	case r < 11:
		return fmt.Sprintf("string.char(%s, %s)", g.pick(smallInts), g.pick(smallInts))
	default:
		return g.pick(strPool)
	}
}

var smallInts = []string{"65", "66", "97", "98", "48", "10"}

func (g *gen) pickFmt() fmtSpec { return fmtSpecs[g.rnd.Intn(len(fmtSpecs))] }

// concatOperand: only literals participate in `..`, numbers from the
// coercion-safe pool (row 38: non-finite formatting AND the
// integer-valued/precision-cutoff corner).
func (g *gen) concatOperand() string {
	if g.rnd.Intn(2) == 0 {
		return g.pick(strPool)
	}
	return g.pick(safeNumStrPool)
}

// boolExpr: condition-shaped.
func (g *gen) boolExpr(d int) string {
	if d <= 0 || g.rnd.Intn(6) == 0 {
		switch r := g.rnd.Intn(8); {
		case r < 2:
			return "true"
		case r < 3:
			return "false"
		default:
			return g.anyScalar(0)
		}
	}
	switch r := g.rnd.Intn(10); {
	case r < 4:
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		return fmt.Sprintf("(%s %s %s)", g.num(d-1), op, g.num(d-1))
	case r < 5:
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		return fmt.Sprintf("(%s %s %s)", g.str(d-1), op, g.str(d-1))
	case r < 6:
		return fmt.Sprintf("(not %s)", g.anyScalar(d-1))
	case r < 8:
		// row 41: and/or operands never bare literals — constant
		// chains mislower in condition position (skipped loop bodies,
		// silent early exits)
		return fmt.Sprintf("(%s and %s)", g.nonConstBool(d-1), g.nonConstBool(d-1))
	default:
		return fmt.Sprintf("(%s or %s)", g.nonConstBool(d-1), g.nonConstBool(d-1))
	}
}

// nonConstBool: a boolean expression with at least one runtime value —
// scalar ref, comparison, or not-of-those. Literal-only shapes fold to
// constants and hit the row-41 dead-TEST mislowering in condition
// position; pinned by _wasm-tests/whlc00-03 until the backend fix.
func (g *gen) nonConstBool(d int) string {
	switch g.rnd.Intn(4) {
	case 0:
		if len(g.scalars) > 0 {
			return g.pick(g.scalars)
		}
		fallthrough
	case 1:
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		if g.rnd.Intn(4) == 0 {
			return fmt.Sprintf("(%s %s %s)", g.str(d-1), op, g.str(d-1))
		}
		return fmt.Sprintf("(%s %s %s)", g.num(d-1), op, g.num(d-1))
	case 2:
		inner := g.nonConstBool(d - 1)
		return fmt.Sprintf("(not %s)", inner)
	default:
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		return fmt.Sprintf("(%s %s %s)", g.num(d-1), op, g.num(d-1))
	}
}

// anyScalar: number/string/bool/nil-ish scalar expression.
func (g *gen) anyScalar(d int) string {
	switch r := g.rnd.Intn(10); {
	case r < 4:
		return g.num(d)
	case r < 7:
		return g.str(d)
	case r < 8:
		return g.pick([]string{"true", "false", "nil"})
	default:
		if len(g.scalars) > 0 {
			return g.pick(g.scalars)
		}
		return g.num(d)
	}
}

func (g *gen) pickT() tblInfo { return g.tables[g.rnd.Intn(len(g.tables))] }

func (g *gen) pickF() *fnInfo {
	if len(g.funcs) == 0 {
		return nil
	}
	f := &g.funcs[g.rnd.Intn(len(g.funcs))]
	return f
}

// call renders a well-formed call of fn (arity- and spread-aware).
// A vararg fn's arity counts the "..." pseudo-param: fixed args are
// arity-1, plus a spread beyond them.
func (g *gen) call(fn *fnInfo) string {
	nfixed := fn.arity
	if fn.vararg {
		nfixed--
	}
	var args []string
	for i := 0; i < nfixed; i++ {
		args = append(args, g.anyScalar(1))
	}
	if fn.vararg {
		args = append(args, "1", `"s"`) // extra spread beyond fixed arity
	}
	return fmt.Sprintf("%s(%s)", fn.name, strings.Join(args, ", "))
}

// multretCall: a call placed to expand (last arg of print, RHS of a
// multi-assign).
func (g *gen) multretCall(d int) string {
	fn := g.pickF()
	if fn == nil || !fn.multi {
		return g.num(d)
	}
	return g.call(fn)
}

// ---- statements ----

func (g *gen) spend(n int) {
	g.budget -= n
	if g.budget < 0 {
		g.budget = 0
	}
}

// stmt emits one statement; top=true at chunk body level (bare risky
// statements are chunk-level only — uncaught raises inside function
// bodies would truncate every callsite; bodies use pcall wraps).
func (g *gen) stmt(top bool) {
	if len(g.out) >= g.maxLine-12 {
		g.spend(g.budget)
		return
	}
	r := g.rnd.Intn(100)
	switch {
	case r < 13:
		g.stmtLocalDecl()
	case r < 26:
		g.stmtPrint()
	case r < 34:
		g.stmtAssign()
	case r < 42:
		g.stmtIf()
	case r < 51:
		g.stmtNumFor()
	case r < 58:
		g.stmtGenFor()
	case r < 64:
		g.stmtWhile()
	case r < 68:
		g.stmtRepeat()
	case r < 75:
		g.stmtPcall()
	case r < 81:
		g.stmtFuncDef()
	case r < 86:
		g.stmtTableDef()
	case r < 91:
		g.stmtMetatable()
	case r < 96:
		g.stmtLib()
	default:
		if top {
			g.bareRisk()
		} else {
			g.stmtPrint()
		}
	}
}

func (g *gen) defs() {
	n := 2 + g.rnd.Intn(5)
	for i := 0; i < n && g.budget > 0; i++ {
		switch g.rnd.Intn(3) {
		case 0:
			g.stmtTableDef()
		case 1:
			g.stmtFuncDef()
		default:
			g.stmtLocalDecl()
		}
	}
}

func (g *gen) stmtLocalDecl() {
	g.spend(1)
	// RHS evaluated against the OUTER scope, names registered after —
	// `local v0 = f(v0)` must read the outer v0, not itself
	if g.rnd.Intn(5) == 0 {
		name := g.newScalar()
		g.scalars = append(g.scalars, name)
		g.linef("local %s", name)
		return
	}
	if g.rnd.Intn(3) == 0 {
		n1, n2 := g.newScalar(), g.newScalar()
		r1, r2 := g.anyScalar(1), g.anyScalar(1)
		g.scalars = append(g.scalars, n1, n2)
		g.linef("local %s, %s = %s, %s", n1, n2, r1, r2)
		return
	}
	name := g.newScalar()
	rhs := g.anyScalar(2)
	g.scalars = append(g.scalars, name)
	g.linef("local %s = %s", name, rhs)
}

func (g *gen) stmtPrint() {
	g.spend(1)
	n := 1 + g.rnd.Intn(3)
	args := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		args = append(args, g.anyScalar(1))
	}
	// last arg: multret expansion one time in three
	if g.rnd.Intn(3) == 0 && len(g.funcs) > 0 {
		args = append(args, g.multretCall(1))
	} else {
		args = append(args, g.anyScalar(1))
	}
	g.linef("print(%s)", strings.Join(args, ", "))
}

func (g *gen) stmtAssign() {
	g.spend(1)
	switch r := g.rnd.Intn(10); {
	case r < 5:
		if len(g.scalars) == 0 {
			g.stmtLocalDecl()
			return
		}
		g.linef("%s = %s", g.pick(g.scalars), g.anyScalar(1))
	case r < 7:
		if len(g.tables) == 0 {
			g.stmtLocalDecl()
			return
		}
		t := g.pickT()
		if g.rnd.Intn(2) == 0 {
			g.linef("%s[%s] = %s", t.name, g.pick(intPool), g.anyScalar(1))
		} else {
			g.linef("%s.%s = %s", t.name, g.pick(identPool), g.anyScalar(1))
		}
	default:
		g.linef("gx%d = %s", g.nScalar, g.anyScalar(1))
	}
}

func (g *gen) stmtIf() {
	g.spend(2)
	hasElseIf := g.rnd.Intn(3) == 0
	hasElse := g.rnd.Intn(3) != 0
	g.linef("if %s then", g.boolExpr(2))
	m := g.mark()
	g.push()
	g.blockBody(1 + g.rnd.Intn(2))
	if hasElseIf {
		g.pop()
		g.restore(m)
		g.linef("elseif %s then", g.boolExpr(1))
		m = g.mark()
		g.push()
		g.blockBody(1)
	}
	if hasElse {
		g.pop()
		g.restore(m)
		g.linef("else")
		m = g.mark()
		g.push()
		g.blockBody(1)
	}
	g.pop()
	g.restore(m)
	g.linef("end")
}

func (g *gen) blockBody(n int) {
	for i := 0; i < n && g.budget > 0; i++ {
		g.stmt(false)
	}
}

func (g *gen) stmtNumFor() {
	g.spend(2)
	iv := g.newFor()
	a := g.pick(intPool)
	k := g.pick([]string{"0", "3", "5", "10", "20"})
	if g.rnd.Intn(6) == 0 { // float flavor
		g.linef("for %s = 0, 2.5, 0.5 do", iv)
	} else if g.rnd.Intn(2) == 0 {
		g.linef("for %s = %s, %s, -1 do", iv, a, k) // downward
	} else {
		g.linef("for %s = %s, %s do", iv, a, k)
	}
	m := g.mark()
	g.push()
	g.blockBody(1 + g.rnd.Intn(2))
	g.linef("print(%s, %s)", iv, iv)
	g.pop()
	g.restore(m)
	g.linef("end")
}

func (g *gen) stmtGenFor() {
	g.spend(2)
	if len(g.tables) == 0 {
		g.stmtTableDef()
	}
	t := g.pickT()
	if !t.dense {
		// row 39: build a fresh dense sequence instead
		name := g.newTableN()
		g.tables = append(g.tables, tblInfo{name: name, dense: true})
		g.linef("local %s = {%s, %s, %s}", name, g.lit(), g.lit(), g.lit())
		t = tblInfo{name: name, dense: true}
	}
	fn := "ipairs"
	if g.rnd.Intn(3) == 0 {
		fn = "pairs" // dense sequence → identical order across engines
	}
	g.linef("for kk%d, vv%d in %s(%s) do", g.nFor, g.nFor, fn, t.name)
	m := g.mark()
	g.push()
	g.linef("print(kk%d, vv%d)", g.nFor, g.nFor)
	g.blockBody(1)
	g.pop()
	g.restore(m)
	g.linef("end")
	g.nFor++
}

func (g *gen) stmtWhile() {
	g.spend(2)
	guard := g.newGuard()
	limit := g.pick([]string{"3", "5", "7", "12", "30"})
	g.linef("local %s = 0", guard)
	g.linef("while %s do", g.boolExpr(1))
	m := g.mark()
	g.push()
	g.linef("%s = %s + 1", guard, guard)
	g.blockBody(1 + g.rnd.Intn(2))
	g.linef("if %s > %s then break end", guard, limit)
	g.pop()
	g.restore(m)
	g.linef("end")
	g.linef("print(%s)", guard)
}

func (g *gen) stmtRepeat() {
	g.spend(2)
	guard := g.newGuard()
	limit := g.pick([]string{"2", "4", "6"})
	g.linef("local %s = 0", guard)
	g.linef("repeat")
	m := g.mark()
	g.push()
	g.linef("%s = %s + 1", guard, guard)
	g.blockBody(1)
	g.linef("if %s > %s then break end", guard, limit)
	g.pop()
	g.restore(m)
	g.linef("until %s or %s > %s", g.boolExpr(0), guard, limit)
	g.linef("print(%s)", guard)
}

// stmtPcall: pcall-wrapped core-error corner; prints ok + caught value.
func (g *gen) stmtPcall() {
	g.spend(1)
	risky := g.riskyExpr()
	g.linef("do")
	m := g.mark()
	g.push()
	g.linef("local ok, e1, e2 = pcall(function() return %s, %s end)", risky, g.anyScalar(0))
	g.linef("print(ok, e1, e2)")
	g.pop()
	g.restore(m)
	g.linef("end")
}

// riskyExpr: expressions that raise core errors (row 9 families).
func (g *gen) riskyExpr() string {
	switch g.rnd.Intn(10) {
	case 0:
		return `(nil).x`
	case 1:
		return `{} + 1`
	case 2:
		return `1 < "2"`
	case 3:
		return `"x" + 1`
	case 4:
		return `{} .. "s"`
	case 5:
		return `(5)()`
	case 6:
		return `#missing_global_q`
	case 7:
		return `(-{})`
	case 8:
		if len(g.tables) > 0 {
			t := g.pickT()
			return fmt.Sprintf("%s.missing_method_q()", t.name)
		}
		return `(5)()`
	default:
		return `error("boom")`
	}
}

// bareRisk: an unwrapped raise at chunk level — the uncaught ERROR
// event ends the log deterministically on every engine.
func (g *gen) bareRisk() {
	g.spend(1)
	switch g.rnd.Intn(6) {
	case 0:
		g.linef("error(%q)", strings.Trim(g.pick(strPool), `"`))
	case 1:
		g.linef("error(%s)", g.pick(intPool))
	case 2:
		g.linef("error()")
	case 3:
		g.linef("print(%s)", g.riskyExpr())
	case 4:
		g.linef("assert(false)")
	default:
		g.linef("assert(nil, %q)", strings.Trim(g.pick(strPool), `"`))
	}
}

func (g *gen) stmtFuncDef() {
	g.spend(2)
	name := g.newFunc()
	arity := g.rnd.Intn(3)
	vararg := g.rnd.Intn(4) == 0
	rec := g.rnd.Intn(6) == 0 // guarded recursion (row 20: depth ≤ 50)

	var params []string
	if rec {
		params = []string{"n"}
	} else {
		pnames := []string{"a", "b", "c"}
		params = append([]string{}, pnames[:arity]...)
		if vararg {
			params = append(params, "...")
		}
	}
	// vararg fns always return a passthrough/multi shape — set before
	// the copy lands in g.funcs (mutating fi after append is lost).
	// fi is appended AFTER the body is emitted: a body must only call
	// previously-defined functions (a DAG) — self/mutual recursion
	// would be unbounded, walking straight into the row-20 depth
	// divergence (wasm adapter guard 150 vs interp harness 1024) and,
	// with a pcall in each frame, a catch-and-recurse ping-pong that
	// spins to the deadline (found by the self-diff gate, case 71).
	fi := fnInfo{name: name, arity: len(params), vararg: vararg,
		multi: vararg || g.rnd.Intn(2) == 0}

	g.linef("local function %s(%s)", name, strings.Join(params, ", "))
	m := g.mark()
	g.push()
	if !rec { // params (a, b, c / ...) are body-locals
		for _, p := range params {
			if p != "..." {
				g.scalars = append(g.scalars, p)
			}
		}
	}
	if rec {
		g.linef("if n <= 0 then return %s end", g.pick(intPool))
		if g.rnd.Intn(2) == 0 {
			g.linef("return %s(n - 1) + n", name) // non-tail recursion
		} else {
			g.linef("return %s(n - 1)", name) // tailcall (M5c trampoline)
		}
	} else {
		g.blockBody(1 + g.rnd.Intn(2))
		if vararg {
			switch g.rnd.Intn(3) {
			case 0:
				g.linef("return select('#', ...), ...")
			case 1:
				g.linef("return %s, ...", g.anyScalar(0))
			default:
				g.linef("return select(%s, ...)", g.pick([]string{"1", "2", "3"}))
			}
		} else if fi.multi {
			g.linef("return %s, %s", g.anyScalar(0), g.anyScalar(0))
		} else if g.rnd.Intn(2) == 0 {
			g.linef("return %s", g.anyScalar(0))
		}
	}
	g.pop()
	g.restore(m) // params go out of scope with the body
	g.linef("end")

	// now (body fully emitted) the function becomes callable — a body
	// referencing itself could only recurse unboundedly (see note above).
	// Guarded-recursive functions are NEVER registered: a general
	// call site would pass an unbounded n (f1(255)) straight into the
	// row-20 depth divergence; their dedicated ≤50-literal callsite is
	// the only invocation.
	if !rec {
		g.funcs = append(g.funcs, fi)
	}

	if rec {
		g.linef("print(%s(%s))", name, g.pick([]string{"7", "12", "23", "40", "50"}))
		return
	}
	// a callsite so the function is exercised
	switch g.rnd.Intn(3) {
	case 0:
		g.linef("print(%s)", g.call(&fi))
	case 1:
		if fi.multi {
			g.linef("local r1, r2, r3 = %s", g.call(&fi))
			g.linef("print(r1, r2, r3)")
		} else {
			g.stmtPrint()
		}
	default:
		g.linef("%s", g.call(&fi))
	}
}

func (g *gen) stmtTableDef() {
	g.spend(1)
	name := g.newTableN()
	dense := false
	switch g.rnd.Intn(3) {
	case 0: // dense sequence
		n := 2 + g.rnd.Intn(4)
		vals := make([]string, n)
		for i := range vals {
			vals[i] = g.lit()
		}
		g.linef("local %s = {%s}", name, strings.Join(vals, ", "))
		dense = true
	case 1: // hash-only string keys (never pairs/#-ed)
		g.linef("local %s = {[%q] = %s, [%q] = %s}", name,
			strings.Trim(g.pick(identPool), `"`), g.lit(),
			strings.Trim(g.pick(identPool), `"`), g.lit())
	default: // mixed: dense array part + string extras
		g.linef("local %s = {%s, %s, %s = %s}", name,
			g.lit(), g.lit(),
			strings.Trim(g.pick(identPool), `"`), g.lit())
		dense = true
	}
	g.tables = append(g.tables, tblInfo{name: name, dense: dense})
	if dense {
		switch g.rnd.Intn(3) {
		case 0:
			g.linef("print(#%s, %s[1])", name, name)
		case 1:
			g.linef("table.insert(%s, %s)", name, g.lit())
			g.linef("print(#%s, %s[#%s])", name, name, name)
		default:
			g.linef("print(%s[%s])", name, g.pick(intPool))
		}
	} else {
		g.linef("print(%s[%q])", name, strings.Trim(g.pick(identPool), `"`))
	}
}

// mmMeta: one metamethod (name + body source).
type mmMeta struct {
	name string
	body string
}

var metamethods = []mmMeta{
	{"__index", `__index = function(t, k) return "IDX" end`},
	{"__newindex", `__newindex = function(t, k, v) rawset(t, k, v * 2) end`},
	{"__call", `__call = function(s, x) return x * 2 end`},
	{"__unm", `__unm = function(a) return -7 end`},
	{"__len", `__len = function(a) return 9 end`},
	{"__concat", `__concat = function(a, b) return "cc" end`},
	{"__eq", `__eq = function(a, b) return true end`},
	{"__lt", `__lt = function(a, b) return false end`},
	{"__le", `__le = function(a, b) return true end`},
	{"__add", `__add = function(a, b) return 100 end`},
	{"__sub", `__sub = function(a, b) return 50 end`},
	{"__mul", `__mul = function(a, b) return 7 end`},
}

// stmtMetatable: build a metatable with a random metamethod subset,
// then exercise exactly those ops (r2-probe shapes, all core-parity).
func (g *gen) stmtMetatable() {
	g.spend(2)
	mt := g.newMT()
	n := 2 + g.rnd.Intn(4)
	chosen := make([]mmMeta, 0, n)
	used := map[string]bool{}
	for len(chosen) < n {
		c := metamethods[g.rnd.Intn(len(metamethods))]
		if used[c.name] {
			continue
		}
		used[c.name] = true
		chosen = append(chosen, c)
	}
	parts := make([]string, len(chosen))
	for i, c := range chosen {
		parts[i] = c.body
	}
	g.linef("local %s = { %s }", mt, strings.Join(parts, ", "))
	t1 := g.newTableN()
	t2 := g.newTableN()
	g.linef("local %s = setmetatable({%s, %s}, %s)", t1, g.lit(), g.lit(), mt)
	g.linef("local %s = setmetatable({}, %s)", t2, mt)
	g.tables = append(g.tables, tblInfo{name: t1, dense: false, hasMT: true},
		tblInfo{name: t2, dense: false, hasMT: true})

	var ops []string
	for _, c := range chosen {
		switch c.name {
		case "__index":
			ops = append(ops, fmt.Sprintf("%s.q_key", t1))
		case "__newindex":
			ops = append(ops, fmt.Sprintf("(function() %s.brand_new_q = 21 return %s.brand_new_q end)()", t1, t1))
		case "__call":
			ops = append(ops, fmt.Sprintf("%s(%s)", t1, g.pick(intPool)))
		case "__unm":
			ops = append(ops, fmt.Sprintf("(-%s)", t1))
		case "__len":
			ops = append(ops, fmt.Sprintf("#%s", t1))
		case "__concat":
			ops = append(ops, fmt.Sprintf("(%q .. %s)", strings.Trim(g.pick(strPool), `"`), t1))
		case "__eq":
			ops = append(ops, fmt.Sprintf("(%s == %s)", t1, t2))
		case "__lt":
			ops = append(ops, fmt.Sprintf("(%s < %s)", t1, t2))
		case "__le":
			ops = append(ops, fmt.Sprintf("(%s <= %s)", t1, t2))
		case "__add":
			ops = append(ops, fmt.Sprintf("(%s + %s)", t1, t2))
		case "__sub":
			ops = append(ops, fmt.Sprintf("(%s - %s)", t1, t2))
		case "__mul":
			ops = append(ops, fmt.Sprintf("(%s * %s)", t1, t2))
		}
	}
	g.linef("print(%s)", strings.Join(ops, ", "))
}

// stmtLib: valid-by-construction library exercise (row 25 rules).
func (g *gen) stmtLib() {
	g.spend(1)
	switch g.rnd.Intn(6) {
	case 0:
		g.linef("print(string.format(\"%%d %%s %%5.2f %%q\", %s, %s, %s, %s))",
			g.pick(intPool), g.pick(strPool),
			g.pick([]string{"1.5", "2.25", "-3.75", "0.125"}), g.pick(strPool))
	case 1:
		g.linef("print((%s):sub(%s, %s), (%s):rep(%s), (%s):upper())",
			g.pick(strPool), g.pick(intPool), g.pick(intPool), g.pick(strPool),
			g.pick([]string{"2", "3"}), g.pick(strPool))
	case 2:
		g.linef("print(string.find(%s, %s), string.byte(%s), string.len(%s))",
			g.pick(strPool), g.pick(strPool), g.pick(strPool), g.pick(strPool))
	case 3:
		g.linef("print(math.floor(%s), math.sqrt(%s), math.abs(%s))",
			g.pick([]string{"-3.5", "2.9"}), g.pick([]string{"2", "16", "0.25"}), g.pick(intPool))
	case 4:
		name := g.newTableN()
		g.tables = append(g.tables, tblInfo{name: name, dense: true})
		// safe pool: table.concat renders numbers engine-natively
		// (row 38) — 1e15 et al. must not reach it
		g.linef("local %s = {%s, %s, %s}", name, g.pick(safeNumStrPool), g.pick(safeNumStrPool), g.pick(safeNumStrPool))
		switch g.rnd.Intn(3) {
		case 0:
			g.linef("table.sort(%s)", name)
			g.linef("print(%s[1], %s[2], %s[3])", name, name, name)
		case 1:
			g.linef("print(table.concat(%s, %s))", name, g.pick([]string{`","`, `"-"`, `""`}))
		default:
			g.linef("print(table.remove(%s), #%s, unpack(%s))", name, name, name)
		}
	default:
		g.linef("print(math.random(6), math.random(2, 9), math.fmod(%s, %s))",
			g.pick(intPool), g.pick([]string{"2", "3", "5"}))
	}
}

// ValidSource parses src with the repo's frontend; the soak uses it as
// the GENBUG gate before any engine sees the program.
func ValidSource(src []byte, name string) error {
	_, err := parse.Parse(bytes.NewReader(src), name)
	return err
}

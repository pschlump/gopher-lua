package lua

import (
	"fmt"
	"math/rand"
	"testing"
)

// P2 engine gate: random set/delete/re-insert churn against a
// first-insertion-order reference model, checked through the public
// boundary (Next / RawGetH / RawGetString). The model encodes the
// pinned semantics: a key's iteration position is fixed at first
// insertion, deletion removes it from iteration without freeing its
// position, and re-insertion restores the original position. Growth
// (rehash) must not reorder anything, so the check runs across sizes
// well past every doubling.
type p2ref struct {
	order []LValue
	pos   map[LValue]int
	live  map[LValue]bool
	val   map[LValue]LValue
}

func newP2ref() *p2ref {
	return &p2ref{pos: map[LValue]int{}, live: map[LValue]bool{}, val: map[LValue]LValue{}}
}

func (r *p2ref) set(k, v LValue) {
	if _, ok := r.pos[k]; !ok {
		r.pos[k] = len(r.order)
		r.order = append(r.order, k)
	}
	r.live[k] = true
	r.val[k] = v
}

func (r *p2ref) del(k LValue) { r.live[k] = false }

func (r *p2ref) iter() []LValue {
	out := []LValue{}
	for _, k := range r.order {
		if r.live[k] {
			out = append(out, k, r.val[k])
		}
	}
	return out
}

func TestTableP2ChurnAgainstModel(t *testing.T) {
	// key pool: strings, negative/zero/oversize integers, non-integral
	// floats, booleans, pointers. Small positive integers stay out —
	// they belong to the array part (RawSetH would park them in the
	// hash part, where next(k)'s array→hash jump can re-yield k
	// itself; the old engine loops identically on that misuse).
	strs := []LValue{}
	for i := 0; i < 24; i++ {
		strs = append(strs, LString(fmt.Sprintf("k%d", i)))
	}
	pool := append([]LValue{}, strs...)
	pool = append(pool,
		LInteger(-5), LInteger(-3), LInteger(0), LInteger(1<<30),
		LInteger(MaxArrayIndex), LInteger(MaxArrayIndex+1),
		LNumber(1.5), LNumber(-0.25), LNumber(2.5),
		LTrue, LFalse,
		&LTable{}, &LTable{}, newLTable(0, 0),
	)
	vals := []LValue{LTrue, LFalse, LInteger(7), LNumber(1.25), LString("v")}

	rng := rand.New(rand.NewSource(20261101))
	ref := newP2ref()
	tb := newLTable(0, 0)

	for step := 0; step < 30000; step++ {
		k := pool[rng.Intn(len(pool))]
		switch rng.Intn(3) {
		case 0, 1: // set (covers re-insert after delete)
			v := vals[rng.Intn(len(vals))]
			ref.set(k, v)
			tb.RawSetH(k, v)
		case 2: // delete (often of absent keys)
			ref.del(k)
			tb.RawSetH(k, LNil)
		}

		if step%97 == 0 {
			got := []LValue{}
			for k, v := tb.Next(LNil); k != LNil; k, v = tb.Next(k) {
				got = append(got, k, v)
			}
			want := ref.iter()
			if len(got) != len(want) {
				t.Fatalf("step %d: iter len %d want %d", step, len(got), len(want))
			}
			for i := range want {
				// pointer keys: same identity required (== on LValue)
				if got[i] != want[i] {
					t.Fatalf("step %d: iter[%d] = %v want %v", step, i, got[i], want[i])
				}
			}
			// spot reads agree with the model on every live key
			for _, k := range pool {
				var wantv LValue = LNil
				if ref.live[k] {
					wantv = ref.val[k]
				}
				if gotv := tb.RawGetH(k); gotv != wantv {
					t.Fatalf("step %d: RawGetH(%v) = %v want %v", step, k, gotv, wantv)
				}
			}
		}
	}

	// a final full-order check after heavy churn (past several rehashes)
	if n := len(ref.order); n < 12 {
		t.Fatalf("churn produced only %d distinct keys — pool too small to stress rehash", n)
	}
}

package lua

import (
	"hash/maphash"
	"strings"
	"testing"

	"github.com/pschlump/gopher-lua/parse"
)

// P3 gate: the per-proto constant-hash side table must agree exactly
// with hashValue's string hashing (same seed, same truncation) — a
// mismatch silently probes the wrong slot.
func TestP3StrConstHashMatchesEngine(t *testing.T) {
	chunk, err := parse.Parse(strings.NewReader(
		`local t = {}; t.alpha = 1; t["beta"] = 2; return t.gamma, "delta", 3`), "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(chunk, "")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	var walk func(p *FunctionProto)
	walk = func(p *FunctionProto) {
		if len(p.strConstHash) != len(p.Constants) {
			t.Fatalf("proto %s: strConstHash len %d != Constants len %d",
				p.SourceName, len(p.strConstHash), len(p.Constants))
		}
		for i, c := range p.Constants {
			if s, ok := c.(LString); ok {
				seen++
				want := uint32(maphash.String(mhseed, string(s)))
				if p.strConstHash[i] != want {
					t.Fatalf("constant %q: side hash %d != engine hash %d", string(s), p.strConstHash[i], want)
				}
			}
		}
		for _, sp := range p.FunctionPrototypes {
			walk(sp)
		}
	}
	walk(p)
	if seen < 3 {
		t.Fatalf("expected several string constants, saw %d", seen)
	}
}

// P3 gate: undump fills the side table (maphash's seed is per-process,
// so the hashes must be derived at load, never carried).
func TestP3UndumpFillsSideTable(t *testing.T) {
	L := NewState()
	defer L.Close()
	fn, err := L.LoadString(`local t = {}; t.alpha = 1; return t.beta, "x"`)
	if err != nil {
		t.Fatal(err)
	}
	data := dumpProto(fn.Proto, false)
	p2, err := undumpProto(data, "=?")
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.strConstHash) != len(p2.Constants) {
		t.Fatalf("undumped side table: len %d != %d", len(p2.strConstHash), len(p2.Constants))
	}
	for i, c := range p2.Constants {
		if s, ok := c.(LString); ok {
			want := uint32(maphash.String(mhseed, string(s)))
			if p2.strConstHash[i] != want {
				t.Fatalf("undumped constant %q: %d != %d", string(s), p2.strConstHash[i], want)
			}
		}
	}
}

// P3 gate: the h==0 fallback (hand-built proto without a side table, and
// the public API string-field paths) still resolves keys — including a
// key whose true hash is NOT zero, exercising the recompute branch.
func TestP3ZeroHashFallback(t *testing.T) {
	tb := newLTable(0, 0)
	tb.RawSetString("format", LString("v"))
	if v := tb.rawGetStringH("format", 0); v != LString("v") {
		t.Fatalf("rawGetStringH h=0 fallback: got %v", v)
	}
	if v := tb.RawGetString("format"); v != LString("v") {
		t.Fatalf("RawGetString: got %v", v)
	}
	L := NewState()
	defer L.Close()
	got, st := L.getFieldString(LValue(tb), "format", 0, varinfoHint{})
	if st != mmSync || got != LString("v") {
		t.Fatalf("getFieldString h=0: got %v st=%d", got, st)
	}
	// miss falls through to __index-absent → LNil, no raise
	if v, st := L.getFieldString(LValue(tb), "absent", 0, varinfoHint{}); st != mmSync || v != LNil {
		t.Fatalf("miss: got %v st=%d", v, st)
	}
	// a hand-built proto (nil side table) executes through the VM path
	// and must behave identically: reuse a compiled proto's code with a
	// stripped side table and run a constant-key access
	chunk, _ := parse.Parse(strings.NewReader(`local s = "k"; return string.rep(s, 2)`), "")
	p, err := Compile(chunk, "")
	if err != nil {
		t.Fatal(err)
	}
	p.strConstHash = nil
	L2 := NewState()
	defer L2.Close()
	fn2 := L2.newMainFunction(p, nil)
	L2.Push(fn2)
	if err2 := L2.PCall(0, 1, nil); err2 != nil {
		t.Fatal(err2)
	}
	if ret := L2.Get(-1); ret != LString("kk") {
		t.Fatalf("nil side table run: got %v", ret)
	}
	L2.Pop(1)
}

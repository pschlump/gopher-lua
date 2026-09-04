package wasm

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func decodeU32(b []byte) (uint32, int) {
	var v uint32
	var shift uint
	for i, c := range b {
		v |= uint32(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
	}
	panic("unterminated u32")
}

func TestLEB128U32(t *testing.T) {
	// Vectors from the wasm spec's leb128 tests.
	cases := []struct {
		v    uint32
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{624485, []byte{0xe5, 0x8e, 0x26}},
		{0xFFFFFFFF, []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
	}
	for _, c := range cases {
		got := AppendU32(nil, c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("AppendU32(%d) = %x, want %x", c.v, got, c.want)
		}
		back, n := decodeU32(got)
		if back != c.v || n != len(got) {
			t.Errorf("round-trip AppendU32(%d): got %d after %d bytes", c.v, back, n)
		}
	}
}

func TestLEB128Signed(t *testing.T) {
	cases := []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{-1, []byte{0x7f}},
		{63, []byte{0x3f}},
		{64, []byte{0xc0, 0x00}},
		{-64, []byte{0x40}},
		{-123456, []byte{0xc0, 0xbb, 0x78}},
		{624485, []byte{0xe5, 0x8e, 0x26}},
		{-624485, []byte{0x9b, 0xf1, 0x59}},
		{int64(-2147483648), []byte{0x80, 0x80, 0x80, 0x80, 0x78}}, // min int32
	}
	for _, c := range cases {
		got := AppendS64(nil, c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("AppendS64(%d) = %x, want %x", c.v, got, c.want)
		}
	}
}

// parseSections walks the section framing of a module and verifies that
// every section's declared size matches its contents and that section ids
// appear in canonical order. This is the structural half of the emitter's
// round-trip gate (wazero's compile is the semantic half).
func parseSections(t *testing.T, mod []byte) []byte {
	t.Helper()
	if !bytes.HasPrefix(mod, []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}) {
		t.Fatal("bad magic/version header")
	}
	pos := 8
	lastID := byte(0)
	nFuncs := -1
	var codeContent []byte
	for pos < len(mod) {
		id := mod[pos]
		pos++
		size, n := decodeU32(mod[pos:])
		pos += n
		if id != 0 { // custom sections may appear anywhere
			if id <= lastID {
				t.Fatalf("section id %d out of order (previous %d)", id, lastID)
			}
			lastID = id
		}
		if pos+int(size) > len(mod) {
			t.Fatalf("section %d declares %d bytes but only %d remain", id, size, len(mod)-pos)
		}
		content := mod[pos : pos+int(size)]
		pos += int(size)
		// Every non-custom section (except start) begins with a vector
		// count; check it fits within the section.
		if id != 8 {
			count, n := decodeU32(content)
			if n > len(content) || int(count) > len(content)-n {
				t.Fatalf("section %d: implausible vector count %d", id, count)
			}
		}
		if id == 3 {
			count, _ := decodeU32(content)
			nFuncs = int(count)
		}
		if id == 10 {
			codeContent = content
		}
	}
	if nFuncs >= 0 {
		count, _ := decodeU32(codeContent)
		if int(count) != nFuncs {
			t.Fatalf("function section has %d entries but code section has %d", nFuncs, count)
		}
	}
	return codeContent
}

func TestFramingFullModule(t *testing.T) {
	m := NewModule()
	m.ImportFunc("env", "abs", []ValueType{I32}, []ValueType{I32})
	m.Table(8, 0)
	m.Memory(1, 4)
	g := m.GlobalI32(7, true)
	m.Data(16, []byte("data-segment"))

	add := m.NewFunction([]ValueType{I32, I32}, []ValueType{I32})
	add.LocalGet(0).LocalGet(1).I32Add().End()
	add.Export("add")

	// switch lowering: br_table targets a nested void block per case, the
	// case body sits after that block's end, then branches past the rest.
	pick := m.NewFunction([]ValueType{I32}, []ValueType{I32})
	pick.Local(I32).
		Block(Void).Block(Void).Block(Void).
		LocalGet(0).
		BrTable([]uint32{0, 1, 2}, 2).
		End().
		I32Const(100).LocalSet(1).Br(1).
		End().
		I32Const(200).LocalSet(1).Br(0).
		End().
		LocalGet(1).End()
	pick.Export("pick")

	_ = g
	bin := m.Encode()
	code := parseSections(t, bin)
	if len(code) == 0 {
		t.Fatal("no code section in full module")
	}
}

// TestGoldenStability pins the exact bytes of a small module so that any
// unintentional emitter change is caught. Update the golden deliberately.
func TestGoldenStability(t *testing.T) {
	m := NewModule()
	m.Memory(1, 0)
	f := m.NewFunction([]ValueType{I32, I32}, []ValueType{I32})
	f.LocalGet(0).LocalGet(1).I32Add().I32Const(1).I32Add().End()
	f.Export("add")
	got := hex.EncodeToString(m.Encode())
	const want = "0061736d0100000001070160027f7f017f0302010005030100010707010361" +
		"646400000a0c010a00200020016a41016a0b"
	if got != want {
		t.Errorf("module bytes changed:\n got %s\nwant %s", got, want)
	}
}

func TestEncodePanicsOnUnbalancedBlocks(t *testing.T) {
	m := NewModule()
	f := m.NewFunction(nil, nil)
	f.Block(Void)
	defer func() {
		if recover() == nil {
			t.Fatal("Encode should panic on unclosed block")
		}
	}()
	m.Encode()
}

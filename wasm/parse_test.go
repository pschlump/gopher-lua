package wasm

import (
	"testing"
)

// TestParseImportsRoundTrip builds a module with the emitter (func imports,
// memory import, several exports) and checks the reader decodes exactly
// what was encoded — the M6c artifact gates ride on this parser.
func TestParseImportsRoundTrip(t *testing.T) {
	m := NewModule()
	m.ImportFunc("host", "event", []ValueType{I32, I32, I32}, nil)
	m.ImportFunc("host", "random01", nil, []ValueType{F64})
	m.ImportMemory("env", "memory", 1, 4)
	g := m.GlobalI32(9, false)
	m.ExportGlobal("gFrameCells", g)
	m.ExportMemory("memory")

	fn := m.NewFunction([]ValueType{I32}, nil)
	fn.I32Const(1).Drop().End()
	fn.Export("lua_dispatch")

	bin := m.Encode()

	imps, err := Imports(bin)
	if err != nil {
		t.Fatalf("imports: %v", err)
	}
	if len(imps) != 3 {
		t.Fatalf("import count = %d, want 3", len(imps))
	}
	want := []ImportDesc{
		{"host", "event", 0},
		{"host", "random01", 0},
		{"env", "memory", 2},
	}
	for i, w := range want {
		if imps[i] != w {
			t.Errorf("import[%d] = %+v, want %+v", i, imps[i], w)
		}
	}

	exports, err := Exports(bin)
	if err != nil {
		t.Fatalf("exports: %v", err)
	}
	if len(exports) != 3 {
		t.Fatalf("export count = %d, want 3", len(exports))
	}
	names := map[string]bool{}
	for _, e := range exports {
		names[e.Name] = true
	}
	for _, want := range []string{"lua_dispatch", "gFrameCells", "memory"} {
		if !names[want] {
			t.Errorf("export %q missing (have %v)", want, names)
		}
	}

	if wasi, err := HasWASIImports(bin); err != nil || wasi {
		t.Errorf("HasWASIImports = %v, %v; want false, nil", wasi, err)
	}
	// sanity: garbage after the header is an error, not a parse
	if _, err := Imports([]byte("\x00asm\x02\x00\x00\x00garbage")); err == nil {
		t.Error("truncated section parsed without error")
	}
}

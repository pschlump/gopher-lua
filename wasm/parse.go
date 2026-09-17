package wasm

// Minimal wasm-binary READER for host-side artifact self-checks (M6c): the
// package is otherwise an emitter. The production runtime blob must carry
// zero wasi_snapshot_preview1 imports and the expected host.* set — the
// sandbox gates parse the embedded artifact's own section bytes rather than
// trusting the build (design A8: self-checking artifact).

import (
	"errors"
	"fmt"
)

// ImportDesc is one import-section entry (only what the gates need:
// module/name and the kind byte).
type ImportDesc struct {
	Module string
	Name   string
	Kind   byte // 0=func 1=table 2=mem 3=global
}

// ExportDesc is one export-section entry.
type ExportDesc struct {
	Name string
	Kind byte // 0=func 1=table 2=mem 3=global
	Idx  uint32
}

type reader struct {
	b   []byte
	pos int
}

func (r *reader) u32() (uint32, error) {
	var v uint32
	for shift := uint(0); ; shift += 7 {
		if r.pos >= len(r.b) || shift > 28 {
			return 0, errors.New("wasm: truncated LEB128")
		}
		c := r.b[r.pos]
		r.pos++
		v |= uint32(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
	}
}

func (r *reader) name() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	if uint64(n) > uint64(len(r.b)-r.pos) {
		return "", errors.New("wasm: truncated name")
	}
	s := string(r.b[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}

func (r *reader) byte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errors.New("wasm: truncated")
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func (r *reader) skip(n uint32) error {
	if uint64(n) > uint64(len(r.b)-r.pos) {
		return errors.New("wasm: section overruns binary")
	}
	r.pos += int(n)
	return nil
}

// section walks to section id and returns its body reader. Sections are
// [id][size][body]; skipping by size covers custom sections too.
func section(bin []byte, id byte) (*reader, error) {
	if len(bin) < 8 || string(bin[0:4]) != "\x00asm" {
		return nil, errors.New("wasm: bad magic")
	}
	r := &reader{b: bin, pos: 8}
	for {
		sid, err := r.byte()
		if err != nil {
			return nil, err
		}
		size, err := r.u32()
		if err != nil {
			return nil, err
		}
		if sid == id {
			return &reader{b: bin, pos: r.pos}, nil
		}
		if err := r.skip(size); err != nil {
			return nil, err
		}
	}
}

// Imports decodes the import section (nil if the module has none).
func Imports(bin []byte) ([]ImportDesc, error) {
	r, err := section(bin, 2)
	if err != nil {
		return nil, fmt.Errorf("no import section: %w", err)
	}
	count, err := r.u32()
	if err != nil {
		return nil, err
	}
	out := make([]ImportDesc, 0, count)
	for i := uint32(0); i < count; i++ {
		var d ImportDesc
		if d.Module, err = r.name(); err != nil {
			return nil, err
		}
		if d.Name, err = r.name(); err != nil {
			return nil, err
		}
		if d.Kind, err = r.byte(); err != nil {
			return nil, err
		}
		switch d.Kind {
		case 0: // func: typeidx
			if _, err = r.u32(); err != nil {
				return nil, err
			}
		case 1: // table: reftype + limits
			if _, err = r.byte(); err != nil {
				return nil, err
			}
			if err = skipLimits(r); err != nil {
				return nil, err
			}
		case 2: // memory: limits
			if err = skipLimits(r); err != nil {
				return nil, err
			}
		case 3: // global: valtype + mut
			if _, err = r.byte(); err != nil {
				return nil, err
			}
			if _, err = r.byte(); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("wasm: import kind %d", d.Kind)
		}
		out = append(out, d)
	}
	return out, nil
}

func skipLimits(r *reader) error {
	flags, err := r.byte()
	if err != nil {
		return err
	}
	if _, err = r.u32(); err != nil {
		return err
	}
	if flags&1 != 0 {
		if _, err = r.u32(); err != nil {
			return err
		}
	}
	return nil
}

// Exports decodes the export section's entry names.
func Exports(bin []byte) ([]ExportDesc, error) {
	r, err := section(bin, 7)
	if err != nil {
		return nil, fmt.Errorf("no export section: %w", err)
	}
	count, err := r.u32()
	if err != nil {
		return nil, err
	}
	out := make([]ExportDesc, 0, count)
	for i := uint32(0); i < count; i++ {
		var d ExportDesc
		if d.Name, err = r.name(); err != nil {
			return nil, err
		}
		if d.Kind, err = r.byte(); err != nil {
			return nil, err
		}
		if d.Idx, err = r.u32(); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// HasWASIImports reports whether the binary imports anything from a
// wasi_snapshot_preview1 module (the production-blob gate).
func HasWASIImports(bin []byte) (bool, error) {
	imps, err := Imports(bin)
	if err != nil {
		return false, err
	}
	for _, im := range imps {
		if im.Module == "wasi_snapshot_preview1" {
			return true, nil
		}
	}
	return false, nil
}

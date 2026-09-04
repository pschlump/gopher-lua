package wasm

// LEB128 encoding for wasm binary immediates (u32, u64, s33/s64, names).
// See docs/Lua-Wasm-Design-and-Test-Plan.md §M0.

// AppendU32 appends v to dst as an unsigned LEB128 (the wasm u32).
func AppendU32(dst []byte, v uint32) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if v == 0 {
			return dst
		}
	}
}

// AppendU64 appends v to dst as an unsigned LEB128 (the wasm u64).
func AppendU64(dst []byte, v uint64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if v == 0 {
			return dst
		}
	}
}

// AppendS64 appends v to dst as a signed LEB128. This is the encoding used
// by i32.const (s33) and i64.const (s64) immediates.
func AppendS64(dst []byte, v int64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7 // arithmetic shift keeps the sign
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(dst, b)
		}
		dst = append(dst, b|0x80)
	}
}

// AppendI32 appends an int32 as the wasm s33 signed LEB128
// (i32.const immediate).
func AppendI32(dst []byte, v int32) []byte {
	return AppendS64(dst, int64(v))
}

// AppendName appends a wasm name: length-prefixed UTF-8.
func AppendName(dst []byte, s string) []byte {
	dst = AppendU32(dst, uint32(len(s)))
	return append(dst, s...)
}

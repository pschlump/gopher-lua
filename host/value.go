package host

// Value: one Lua value as it crosses the embedder seam. This is glasm
// host's wire Value, kept API-identical (the daemon-side conversion code
// ports unchanged), but natively there is no wire: the conversions below
// walk lua.LValue trees directly. What the wire protocol enforced with
// its staging buffer is enforced here as explicit caps, inherited from
// the retired encoder (runtime/luawasm55.c): expansion depth 8
// (ENC_MAX_DEPTH), 512 entries per table (ENC_MAX_ENTRIES) with a More
// sentinel, cycle/deep sentinels, 4096 values per call frame. The
// staging buffer's total-size bound becomes the per-conversion value
// budget (maxExpandVals).
//
// The conversions COPY: a converted Value owns nothing that aliases the
// VM's state, so results stay valid after the VM is reused or closed —
// the property the wire encode bought by construction.

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	lua "github.com/pschlump/gopher-lua"
)

// ValueKind mirrors the retired wire tags (PT_* in glasm's
// runtime/luawasm55.c); the numbering is kept for familiarity.
type ValueKind uint8

const (
	KindNil      ValueKind = 0
	KindFalse    ValueKind = 1
	KindTrue     ValueKind = 2
	KindNumber   ValueKind = 3
	KindString   ValueKind = 4
	KindTable    ValueKind = 5
	KindFunction ValueKind = 6
	KindThread   ValueKind = 7
	KindUserdata ValueKind = 9
	KindCycle    ValueKind = 10
	KindDeep     ValueKind = 11
	KindInteger  ValueKind = 13
)

// KV is one table entry.
type KV struct {
	Key, Val Value
}

// Value is one Lua value as it crosses the seam.
type Value struct {
	Kind  ValueKind
	Num   float64 // KindNumber
	Int   int64   // KindInteger (lossless, not narrowed through Num — U6e)
	Str   string  // KindString
	Pairs []KV    // KindTable
	More  bool    // KindTable: the expansion was truncated at 512 entries
}

// Convenience constructors.
func Nil() Value { return Value{Kind: KindNil} }
func Bool(b bool) Value {
	if b {
		return Value{Kind: KindTrue}
	}
	return Value{Kind: KindFalse}
}
func Number(f float64) Value { return Value{Kind: KindNumber, Num: f} }
func Int(n int64) Value      { return Value{Kind: KindInteger, Int: n} }
func String(s string) Value  { return Value{Kind: KindString, Str: s} }
func Table(pairs ...KV) Value {
	return Value{Kind: KindTable, Pairs: pairs}
}

// BoolVal reports the boolean of a KindTrue/KindFalse value.
func (v Value) BoolVal() bool { return v.Kind == KindTrue }

// Equal is the deep structural compare over seam values.
func (v Value) Equal(o Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case KindNumber:
		return v.Num == o.Num
	case KindInteger:
		return v.Int == o.Int // integers cross lossless — compare the payload
	case KindString:
		return v.Str == o.Str
	case KindTable:
		if len(v.Pairs) != len(o.Pairs) || v.More != o.More {
			return false
		}
		for i := range v.Pairs {
			if !v.Pairs[i].Key.Equal(o.Pairs[i].Key) || !v.Pairs[i].Val.Equal(o.Pairs[i].Val) {
				return false
			}
		}
	}
	return true
}

// String renders a value for error messages and logs (NOT the Redis
// reply conversion — that is the daemon's job, host-side and
// locale-free).
func (v Value) String() string {
	switch v.Kind {
	case KindNil:
		return "nil"
	case KindFalse:
		return "false"
	case KindTrue:
		return "true"
	case KindNumber:
		return numRepr(v.Num)
	case KindInteger:
		return strconv.FormatInt(v.Int, 10)
	case KindString:
		return v.Str
	case KindTable:
		return "table"
	case KindFunction:
		return "function"
	case KindThread:
		return "thread"
	case KindUserdata:
		return "userdata"
	case KindCycle:
		return "table (cycle)"
	case KindDeep:
		return "table (too deep)"
	}
	return "?"
}

// numRepr formats a FLOAT the way stock Lua 5.5 renders floats in
// tostring (lobject.c tostringbuffFloat): %.15g, re-read, %.17g when the
// round-trip loses the value, ".0" appended when the result looks like a
// plain integer (2.0 stays "2.0"), inf/nan in the libc spellings.
// Integers (KindInteger) never come through here — they print bare via
// FormatInt. Log-rendering only; the interpreter's own tostring is
// unaffected (glasm host carried the same copy).
func numRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		if math.Signbit(f) {
			return "-nan"
		}
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	if v, err := strconv.ParseFloat(s, 64); err != nil || v != f {
		s = strconv.FormatFloat(f, 'g', 17, 64)
	}
	if strings.Trim(s, "-0123456789") == "" {
		s += ".0"
	}
	return s
}

// The seam caps, inherited from the retired wire encoder
// (runtime/luawasm55.c): the staging buffer is gone but the contract it
// implied is kept — bounded expansion, bounded frames — so a pathological
// value can never pin the daemon in conversion.
const (
	encMaxDepth   = 8    // ENC_MAX_DEPTH: deeper tables cross as KindDeep
	encMaxEntries = 512  // ENC_MAX_ENTRIES: larger tables cross truncated (More)
	maxFrameVals  = 4096 // values per hostfn call frame, either direction
	// maxExpandVals bounds the TOTAL values produced by one conversion —
	// the wire staging buffer's size bound, restated as a value count.
	maxExpandVals = 65536
	// decMaxDepth bounds host→guest nesting (the guest decoder's
	// HF_DEC_MAX_DEPTH): a host-built tree deeper than this is a clean
	// refusal, never unbounded Go recursion.
	decMaxDepth = 64
)

var errExpansionTooLarge = errors.New("host: value expansion too large")

// fromLValue converts a Lua value into an owning seam Value: scalars
// directly, tables expanded (Next order — the wire encoder's lua_next
// walk), functions/userdata/threads as opaque markers, cycles and
// over-deep tables as sentinels.
func fromLValue(lv lua.LValue) (Value, error) {
	budget := maxExpandVals
	v, err := convertFromLua(lv, 0, nil, &budget)
	if err != nil {
		return Value{}, err
	}
	return v, nil
}

// convertFromLua is fromLValue's recursion. anc is the table-ancestor
// path (cycle detection is path-based, like the encoder's — a DAG
// expands each occurrence). budget counts every value minted.
func convertFromLua(lv lua.LValue, depth int, anc []*lua.LTable, budget *int) (Value, error) {
	*budget--
	if *budget < 0 {
		return Value{}, errExpansionTooLarge
	}
	switch v := lv.(type) {
	case *lua.LNilType:
		return Nil(), nil
	case lua.LBool:
		return Bool(bool(v)), nil
	case lua.LNumber:
		return Number(float64(v)), nil
	case lua.LInteger:
		return Int(int64(v)), nil
	case lua.LString:
		return String(string(v)), nil
	case *lua.LTable:
		for _, a := range anc {
			if a == v {
				return Value{Kind: KindCycle}, nil
			}
		}
		if depth >= encMaxDepth {
			return Value{Kind: KindDeep}, nil
		}
		child := make([]*lua.LTable, len(anc), len(anc)+1)
		copy(child, anc)
		child = append(child, v)
		out := Value{Kind: KindTable}
		prev := lua.LNil
		for i := 0; i < encMaxEntries; i++ {
			k, val := v.Next(prev)
			if k == lua.LNil {
				break
			}
			kk, err := convertFromLua(k, depth+1, child, budget)
			if err != nil {
				return Value{}, err
			}
			vv, err := convertFromLua(val, depth+1, child, budget)
			if err != nil {
				return Value{}, err
			}
			out.Pairs = append(out.Pairs, KV{Key: kk, Val: vv})
			prev = k
		}
		if k, _ := v.Next(prev); k != lua.LNil {
			out.More = true
		}
		return out, nil
	case *lua.LFunction:
		return Value{Kind: KindFunction}, nil
	case *lua.LUserData:
		return Value{Kind: KindUserdata}, nil
	case lua.LChannel:
		// channels are opaque here (channellib ships only where the
		// embedder opens it); they cross as userdata-class markers
		return Value{Kind: KindUserdata}, nil
	default:
		// unknown LValue implementations (host-minted customs): opaque
		return Value{Kind: KindUserdata}, nil
	}
}

// toLValue builds a fresh Lua value from a seam Value (the host→guest
// direction: hostfn returns, RegisterValue constants). Only the kinds
// the guest can accept — scalars and complete tables — mirroring the old
// guest decoder's rejection of the opaque/sentinel kinds and its
// nesting-depth guard.
func toLValue(L *lua.LState, v Value) (lua.LValue, error) {
	return toLValueDepth(L, v, 0)
}

func toLValueDepth(L *lua.LState, v Value, depth int) (lua.LValue, error) {
	if depth > decMaxDepth {
		return nil, errors.New("host: value nesting too deep")
	}
	switch v.Kind {
	case KindNil:
		return lua.LNil, nil
	case KindFalse:
		return lua.LFalse, nil
	case KindTrue:
		return lua.LTrue, nil
	case KindNumber:
		return lua.LNumber(v.Num), nil
	case KindInteger:
		return lua.LInteger(v.Int), nil
	case KindString:
		return lua.LString(v.Str), nil
	case KindTable:
		if v.More {
			return nil, errors.New("host: cannot send a truncated table to the guest")
		}
		if len(v.Pairs) > encMaxEntries {
			return nil, fmt.Errorf("host: hostfn table has too many entries (>%d)", encMaxEntries)
		}
		tb := L.NewTable()
		for _, kv := range v.Pairs {
			k, err := toLValueDepth(L, kv.Key, depth+1)
			if err != nil {
				return nil, err
			}
			if k == lua.LNil {
				return nil, errors.New("host: cannot send a nil table key to the guest")
			}
			val, err := toLValueDepth(L, kv.Val, depth+1)
			if err != nil {
				return nil, err
			}
			tb.RawSet(k, val)
		}
		return tb, nil
	default:
		return nil, fmt.Errorf("host: cannot send a %s value to the guest", v)
	}
}

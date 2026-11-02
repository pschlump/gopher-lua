package lua

import (
	"hash/maphash"
	"math"
	"sync/atomic"
	"unsafe"
)

const defaultArrayCap = 32
const defaultHashCap = 32

// initialNodesCap is the node-array size at first materialization — the
// small-table regime of the P2 plan: one mask and a probe that almost
// always terminates on the first slot at this size, 320 bytes per table
// against the old strdict preallocation.
const initialNodesCap = 8

type lValueArraySorter struct {
	L      *LState
	Fn     *LFunction
	Values []LValue
}

func (lv lValueArraySorter) Len() int {
	return len(lv.Values)
}

func (lv lValueArraySorter) Swap(i, j int) {
	lv.Values[i], lv.Values[j] = lv.Values[j], lv.Values[i]
}

func (lv lValueArraySorter) Less(i, j int) bool {
	if lv.Fn != nil {
		lv.L.Push(lv.Fn)
		lv.L.Push(lv.Values[i])
		lv.L.Push(lv.Values[j])
		lv.L.Call(2, 1)
		return LVAsBool(lv.L.reg.Pop())
	}
	// Go context (sort's call region): the dispatch state is always mmSync
	v, _ := lessThan(lv.L, lv.Values[i], lv.Values[j])
	return v
}

// gcMemBytes is the synthetic Lua-heap accounting behind
// collectgarbage("count"): the fork has no compacting GC to interrogate,
// so bytes are charged at the big allocation sites (a table's array/hash
// preallocation). It is monotonic — freed tables are not refunded — and
// only deltas are observable (sort.lua:19's memdiff battery); the value
// is never printed by the official suite. Atomic: independent LStates
// (the embed's parallel-VM posture) charge it concurrently.
var gcMemBytes atomic.Int64

func gcCharge(bytes int64) { gcMemBytes.Add(bytes) }

func newLTable(acap int, hcap int) *LTable {
	if acap < 0 {
		acap = 0
	}
	if hcap < 0 {
		hcap = 0
	}
	tb := &LTable{}
	tb.Metatable = LNil
	tb.first, tb.tail = -1, -1
	if acap != 0 {
		tb.array = make([]LValue, 0, acap)
	}
	if hcap != 0 {
		// the old code preallocated strdict here, so such a table's
		// first RawSetString never fired the materialization charge —
		// spend the seam at birth to keep the charges identical (OP_
		// VARARG's table packs depend on this; the nodes stay lazy)
		tb.strdCharged = true
	}
	gcCharge(tableCharge(acap, hcap))
	return tb
}

// newLTableG is the accounted constructor: the table's later growth
// charges (the RawSet*/Insert/Append seams below) go to g's per-VM
// memory budget, and its creation estimate is charged up front. The
// registry seed tables use newLTable (nil accountant — they predate
// their Global).
func newLTableG(g *Global, acap int, hcap int) *LTable {
	tb := newLTable(acap, hcap)
	tb.acct = g
	g.memCharge(tableCharge(acap, hcap))
	return tb
}

// SetReadonly marks this table readonly — the N2 sandbox's one-way
// guard (the embed's WithGlobalsProtection; the analog of the C
// engine's interposed luaH_* setters). Lua-visible writes raise
// "Attempt to modify a readonly table" (the bytecode set paths and
// everything that funnels into them — LState.SetTable/SetField,
// rawset, the table library's lua_seti writes; see setField /
// setFieldString / baseRawSet). Raw Go writes (the LTable methods
// below, LState.RawSet*) are exempt: that is the host staging seam.
// One-way by design — there is no unmark.
func (tb *LTable) SetReadonly() { tb.readonly = true }

// Readonly reports the one-way write guard (SetReadonly).
func (tb *LTable) Readonly() bool { return tb.readonly }

// tbCharge accounts table growth at the write seams (the same
// per-slot estimates as creation; see membudget.go).
func (tb *LTable) tbCharge(n int64) {
	if tb.acct != nil {
		tb.acct.memCharge(n)
	}
}

// arrayLen reports the size of the array part (0 when absent) — the
// analogue of stock's t->asize for next()'s keyinarray range test.
func (tb *LTable) arrayLen() int {
	return len(tb.array)
}

// Len returns length of this LTable without using __len.
func (tb *LTable) Len() int {
	if tb.array == nil {
		return 0
	}
	var prev LValue = LNil
	for i := len(tb.array) - 1; i >= 0; i-- {
		v := tb.array[i]
		if prev == LNil && v != LNil {
			return i + 1
		}
		prev = v
	}
	return 0
}

// Append appends a given LValue to this LTable.
func (tb *LTable) Append(value LValue) {
	if value == LNil {
		return
	}
	if tb.array == nil {
		tb.array = make([]LValue, 0, defaultArrayCap)
		tb.tbCharge(defaultArrayCap * 16)
	}
	if len(tb.array) == 0 || tb.array[len(tb.array)-1] != LNil {
		tb.array = append(tb.array, value)
		tb.tbCharge(16)
	} else {
		i := len(tb.array) - 2
		for ; i >= 0; i-- {
			if tb.array[i] != LNil {
				break
			}
		}
		tb.array[i+1] = value
	}
}

// Insert inserts a given LValue at position `i` in this LTable.
func (tb *LTable) Insert(i int, value LValue) {
	if tb.array == nil {
		tb.array = make([]LValue, 0, defaultArrayCap)
		tb.tbCharge(defaultArrayCap * 16)
	}
	if i > len(tb.array) {
		tb.RawSetInt(i, value)
		return
	}
	if i <= 0 {
		tb.RawSet(LNumber(i), value)
		return
	}
	i -= 1
	tb.array = append(tb.array, LNil)
	tb.tbCharge(16)
	copy(tb.array[i+1:], tb.array[i:])
	tb.array[i] = value
}

// MaxN returns a maximum number key that nil value does not exist before it.
func (tb *LTable) MaxN() int {
	if tb.array == nil {
		return 0
	}
	for i := len(tb.array) - 1; i >= 0; i-- {
		if tb.array[i] != LNil {
			return i + 1
		}
	}
	return 0
}

// Remove removes from this table the element at a given position.
func (tb *LTable) Remove(pos int) LValue {
	if tb.array == nil {
		return LNil
	}
	larray := len(tb.array)
	if larray == 0 {
		return LNil
	}
	i := pos - 1
	oldval := LNil
	switch {
	case i >= larray:
		// nothing to do
	case i == larray-1 || i < 0:
		oldval = tb.array[larray-1]
		tb.array = tb.array[:larray-1]
	default:
		oldval = tb.array[i]
		copy(tb.array[i:], tb.array[i+1:])
		tb.array[larray-1] = nil
		tb.array = tb.array[:larray-1]
	}
	return oldval
}

// normalizeNumberKey maps a numeric key to its canonical table-key form
// (5.3+ rule): a float with an exact integer value becomes that integer —
// t[2.0] and t[2] name the same slot — while integers and non-integral
// floats are unchanged. Non-number keys pass through untouched. Every
// table boundary (RawSet/RawGet/RawSetH/RawGetH) normalizes so the
// node array never holds two spellings of one number (Next does NOT —
// stock 5.5 findindex hashes the raw key).
func normalizeNumberKey(key LValue) LValue {
	switch v := key.(type) {
	case LInteger:
		return v
	case LNumber:
		if i, ok := floatToIntExact(float64(v)); ok {
			return LInteger(i)
		}
	}
	return key
}

// mhseed seeds string-key hashing (hash/maphash): random per process,
// high avalanche, and never observable — iteration order rides the ins
// links, not the hashes — so only hash quality matters.
var mhseed = maphash.MakeSeed()

// hashValue returns key's bucket hash, consistent with LValue equality:
// equal values must hash equal (callers normalize numeric spellings
// first), while unequal values only collide by accident. The salts keep
// the numeric kinds from cross-colliding; pointer keys hash the
// pointer. The LNil default is safe: all equal LNil values share it.
func hashValue(key LValue) uint32 {
	switch v := key.(type) {
	case LString:
		return uint32(maphash.String(mhseed, string(v)))
	case LInteger:
		return mix64(uint64(v) ^ 0x9e3779b97f4a7c15)
	case LNumber:
		bits := uint64(math.Float64bits(float64(v)))
		if v == 0 {
			bits = 0 // -0.0 == 0.0 must hash equal
		}
		return mix64(bits ^ 0xa24baed4963ee407)
	case LBool:
		if v {
			return 0x85ebca6b
		}
		return 0xc2b2ae35
	case *LTable:
		return mix64(uint64(uintptr(unsafe.Pointer(v))) ^ 0xe0852a9b4f3d13c5)
	case *LFunction:
		return mix64(uint64(uintptr(unsafe.Pointer(v))) ^ 0x4b8d9e2f1a6c5773)
	case *LUserData:
		return mix64(uint64(uintptr(unsafe.Pointer(v))) ^ 0x1d6ab3f52c9170ad)
	case *LState:
		return mix64(uint64(uintptr(unsafe.Pointer(v))) ^ 0x27d4eb2f9a1c85b5)
	case LChannel:
		// a chan is a single pointer word; read it (channels do not
		// convert to unsafe.Pointer directly)
		return mix64(uint64(uintptr(*(*unsafe.Pointer)(unsafe.Pointer(&v)))) ^ 0x165667b19e3749f3)
	}
	return 0x9e3779b9
}

// mix64 is the splitmix64 finalizer, folded to 32 bits.
func mix64(x uint64) uint32 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return uint32(x ^ (x >> 32))
}

// lookup returns the slot of key's node — live or tombstone — or -1
// when key was never inserted. Callers pass the stored spelling.
func (tb *LTable) lookup(key LValue, h uint32) int32 {
	if tb.nodes == nil {
		return -1
	}
	i := h & tb.mask
	for {
		n := &tb.nodes[i]
		if n.key == nil {
			return -1
		}
		if n.hash == h && n.key == key {
			return int32(i)
		}
		i = (i + 1) & tb.mask
	}
}

// hashInsert stores val under key. An existing node — live or tombstone
// — is reused in place, which is what keeps a deleted-then-reinserted
// key at its original iteration position (the order the suite pins). A
// never-seen key appends to the chain tail and pays the per-key charge
// (the old k2i-miss +48). The load factor is held at ≤ 3/4, which also
// guarantees every probe finds a nil slot.
func (tb *LTable) hashInsert(key, val LValue) {
	if tb.nodes == nil {
		tb.nodes = make([]hnode, initialNodesCap)
		tb.mask = initialNodesCap - 1
	}
	h := hashValue(key)
	i := h & tb.mask
	for {
		n := &tb.nodes[i]
		if n.key == nil {
			break
		}
		if n.hash == h && n.key == key {
			if n.val == LNil {
				if _, ok := key.(LString); ok {
					tb.strKeys++
				}
			}
			n.val = val
			return
		}
		i = (i + 1) & tb.mask
	}
	if (tb.used+1)*4 > int32(len(tb.nodes))*3 {
		tb.rehash(len(tb.nodes) * 2)
		i = h & tb.mask
		for tb.nodes[i].key != nil {
			i = (i + 1) & tb.mask
		}
	}
	n := &tb.nodes[i]
	n.key, n.val, n.hash = key, val, h
	n.ins = -1
	if tb.tail >= 0 {
		tb.nodes[tb.tail].ins = int32(i)
	} else {
		tb.first = int32(i)
	}
	tb.tail = int32(i)
	tb.used++
	if _, ok := key.(LString); ok {
		tb.strKeys++
	}
	tb.tbCharge(48)
}

// hashDelete tombstones key's node. Nothing is unlinked and no slot is
// freed: iteration order is frozen at first insertion, and next() skips
// tombstones (a deleted key is gone even though its node remains).
func (tb *LTable) hashDelete(key LValue) {
	i := tb.lookup(key, hashValue(key))
	if i < 0 {
		return
	}
	n := &tb.nodes[i]
	if n.val == LNil {
		return
	}
	n.val = LNil
	if _, ok := n.key.(LString); ok {
		tb.strKeys--
	}
}

// rehash re-slots every node — live and tombstone — into a bigger node
// array. Slots are storage and order rides the links: the chain is
// walked head to tail and rebuilt in that order, so growth at any size
// preserves iteration order (the 200-key growth probe pins it).
func (tb *LTable) rehash(nwcap int) {
	old := tb.nodes
	tb.nodes = make([]hnode, nwcap)
	tb.mask = uint32(nwcap) - 1
	f, t := int32(-1), int32(-1)
	for i := tb.first; i >= 0; i = old[i].ins {
		j := old[i].hash & tb.mask
		for tb.nodes[j].key != nil {
			j = (j + 1) & tb.mask
		}
		tb.nodes[j] = old[i]
		tb.nodes[j].ins = -1
		if t >= 0 {
			tb.nodes[t].ins = int32(j)
		} else {
			f = int32(j)
		}
		t = int32(j)
	}
	tb.first, tb.tail = f, t
}

// nextFrom yields the first live entry at or after chain position i
// (tombstones are skipped — exactly the old walk over keys with
// RawGetH != LNil, without the per-step map lookup).
func (tb *LTable) nextFrom(i int32) (LValue, LValue, bool) {
	for ; i >= 0; i = tb.nodes[i].ins {
		if v := tb.nodes[i].val; v != LNil {
			return tb.nodes[i].key, v, true
		}
	}
	return LNil, LNil, false
}

// RawSet sets a given LValue to a given index without the __newindex metamethod.
// It is recommended to use `RawSetString` or `RawSetInt` for performance
// if you already know the given LValue is a string or number.
func (tb *LTable) RawSet(key LValue, value LValue) {
	key = normalizeNumberKey(key)
	switch v := key.(type) {
	case LInteger:
		if v > 0 && v < LInteger(MaxArrayIndex) {
			if tb.array == nil {
				tb.array = make([]LValue, 0, defaultArrayCap)
				tb.tbCharge(defaultArrayCap * 16)
			}
			index := int(v) - 1
			alen := len(tb.array)
			switch {
			case index == alen:
				tb.array = append(tb.array, value)
				tb.tbCharge(16)
			case index > alen:
				for i := 0; i < (index - alen); i++ {
					tb.array = append(tb.array, LNil)
				}
				tb.array = append(tb.array, value)
				tb.tbCharge(int64(index-alen+1) * 16)
			case index < alen:
				tb.array[index] = value
			}
			return
		}
	case LString:
		tb.RawSetString(string(v), value)
		return
	}

	tb.RawSetH(key, value)
}

// RawSetInt sets a given LValue at a position `key` without the __newindex metamethod.
func (tb *LTable) RawSetInt(key int, value LValue) {
	if key < 1 || key >= MaxArrayIndex {
		tb.RawSetH(LInteger(key), value)
		return
	}
	if tb.array == nil {
		tb.array = make([]LValue, 0, 32)
		tb.tbCharge(32 * 16)
	}
	index := key - 1
	alen := len(tb.array)
	switch {
	case index == alen:
		tb.array = append(tb.array, value)
		tb.tbCharge(16)
	case index > alen:
		for i := 0; i < (index - alen); i++ {
			tb.array = append(tb.array, LNil)
		}
		tb.array = append(tb.array, value)
		tb.tbCharge(int64(index-alen+1) * 16)
	case index < alen:
		tb.array[index] = value
	}
}

// RawSetString sets a given LValue to a given string index without the __newindex metamethod.
// The one-shot charge mirrors the old strdict materialization (it fired
// on the first string write whatever the value, so it does here).
func (tb *LTable) RawSetString(key string, value LValue) {
	if !tb.strdCharged {
		tb.strdCharged = true
		tb.tbCharge(defaultHashCap * 48)
	}
	lkey := LString(key)
	if value == LNil {
		tb.hashDelete(lkey)
		return
	}
	tb.hashInsert(lkey, value)
}

// RawSetH sets a given LValue to a given index without the __newindex metamethod.
// The one-shot charge mirrors the old dict materialization: it fired on
// the first non-string hash write, sized by the live string keys at
// that moment (the old make(map, len(strdict))), whatever the value.
func (tb *LTable) RawSetH(key LValue, value LValue) {
	if s, ok := key.(LString); ok {
		tb.RawSetString(string(s), value)
		return
	}
	key = normalizeNumberKey(key)
	if !tb.dictCharged {
		tb.dictCharged = true
		tb.tbCharge(int64(tb.strKeys)*48 + 64)
	}
	if value == LNil {
		tb.hashDelete(key)
		return
	}
	tb.hashInsert(key, value)
}

// RawGet returns an LValue associated with a given key without __index metamethod.
func (tb *LTable) RawGet(key LValue) LValue {
	key = normalizeNumberKey(key)
	switch v := key.(type) {
	case LInteger:
		if v > 0 && v < LInteger(MaxArrayIndex) {
			if tb.array == nil {
				return LNil
			}
			index := int(v) - 1
			if index >= len(tb.array) {
				return LNil
			}
			return tb.array[index]
		}
	}
	return tb.RawGetH(key)
}

// RawGetInt returns an LValue at position `key` without __index metamethod.
// An out-of-array-range key reads the hash part (stock luaH_getint), so a
// {[math.mininteger]=10} table answers at negative indices too.
func (tb *LTable) RawGetInt(key int) LValue {
	if key >= 1 && key <= len(tb.array) {
		return tb.array[key-1]
	}
	return tb.RawGetH(LInteger(key))
}

// RawGetH returns an LValue associated with a given key without __index metamethod.
func (tb *LTable) RawGetH(key LValue) LValue {
	if tb.nodes == nil {
		return LNil
	}
	key = normalizeNumberKey(key)
	i := tb.lookup(key, hashValue(key))
	if i < 0 {
		return LNil
	}
	// a tombstone reads LNil — the right answer for a deleted key
	return tb.nodes[i].val
}

// RawGetString returns an LValue associated with a given key without __index metamethod.
// This is the engine's hottest table read (OP_GETFIELD/GETTABUP), so the
// probe loop lives here: hash, mask, compare — no LValue boxing of the
// query, no method-call layer.
func (tb *LTable) RawGetString(key string) LValue {
	return tb.rawGetStringH(key, 0)
}

// rawGetStringH is RawGetString taking the key's hash when the caller
// already has it (the VM's constant-key ops carry the per-proto
// precomputed hash). h==0 means "none supplied" — a constant whose true
// hash IS zero just recomputes it here, correctly.
func (tb *LTable) rawGetStringH(key string, h uint32) LValue {
	if tb.nodes == nil {
		return LNil
	}
	if h == 0 {
		h = uint32(maphash.String(mhseed, key))
	}
	i := h & tb.mask
	for {
		n := &tb.nodes[i]
		if n.key == nil {
			return LNil
		}
		if n.hash == h {
			if ks, ok := n.key.(LString); ok && string(ks) == key {
				return n.val
			}
		}
		i = (i + 1) & tb.mask
	}
}

// ForEach iterates over this table of elements, yielding each in turn to a given function.
// Hash entries yield in first-insertion order (deterministic; the old
// implementation iterated two Go maps in randomized order).
func (tb *LTable) ForEach(cb func(LValue, LValue)) {
	if tb.array != nil {
		for i, v := range tb.array {
			if v != LNil {
				cb(LNumber(i+1), v)
			}
		}
	}
	for i := tb.first; i >= 0; i = tb.nodes[i].ins {
		if v := tb.nodes[i].val; v != LNil {
			cb(tb.nodes[i].key, v)
		}
	}
}

// This function is equivalent to lua_next ( http://www.lua.org/manual/5.1/manual.html#lua_next ).
// Unlike RawGet/RawSet, the incoming key is NOT normalized: stock 5.5's
// findindex accepts array entries only for integer keys (keyinarray is
// ttisinteger-only) and hashes the raw key for the hash part, so the float
// spelling of an integer slot is an invalid key (baseNext screens it).
func (tb *LTable) Next(key LValue) (LValue, LValue) {
	init := false
	if key == LNil {
		key = LInteger(0)
		init = true
	}

	if init || key != LInteger(0) {
		if kv, ok := key.(LInteger); ok && int64(kv) >= 0 && kv < LInteger(MaxArrayIndex) {
			index := int(kv)
			if tb.array != nil {
				for ; index < len(tb.array); index++ {
					if v := tb.array[index]; v != LNil {
						return LInteger(index + 1), v
					}
				}
			}
			if tb.array == nil || index == len(tb.array) {
				// hash part: first insertion-order entry — a dead head
				// falls through the same chain walk (the old code probed
				// keys[0] then resumed its walk; nextFrom does both)
				if k, v, ok := tb.nextFrom(tb.first); ok {
					return k, v
				}
				return LNil, LNil
			}
		}
	}

	// hash walk from the entry after key's. The key arrives in its
	// stored spelling (baseNext screened it), so the probe finds its
	// node; a miss — unreachable through next() — resumes after the
	// head, matching the old k2i-miss walk from index 1.
	start := tb.first
	if i := tb.lookup(key, hashValue(key)); i >= 0 {
		start = tb.nodes[i].ins
	} else if start >= 0 {
		start = tb.nodes[start].ins
	}
	if k, v, ok := tb.nextFrom(start); ok {
		return k, v
	}
	return LNil, LNil
}

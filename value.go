package lua

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"weak"
)

type LValueType int

const (
	LTNil LValueType = iota
	LTBool
	LTNumber
	LTString
	LTFunction
	LTUserData
	LTThread
	LTTable
	LTChannel
	// LTInteger is the 5.3+ integer subtype (U2/D1). It is a distinct
	// LValueType so type switches can tell int from float, but it names
	// "number": Lua's type() must keep reporting the basic type.
	LTInteger
)

var lValueNames = [10]string{"nil", "boolean", "number", "string", "function", "userdata", "thread", "table", "channel", "number"}

func (vt LValueType) String() string {
	return lValueNames[int(vt)]
}

type LValue interface {
	String() string
	Type() LValueType
}

// LVIsFalse returns true if a given LValue is a nil or false otherwise false.
func LVIsFalse(v LValue) bool { return v == LNil || v == LFalse }

// LVIsFalse returns false if a given LValue is a nil or false otherwise true.
func LVAsBool(v LValue) bool { return v != LNil && v != LFalse }

// LVAsString returns string representation of a given LValue
// if the LValue is a string or number, otherwise an empty string.
func LVAsString(v LValue) string {
	switch sn := v.(type) {
	case LString, LNumber, LInteger:
		return sn.String()
	default:
		return ""
	}
}

// LVCanConvToString returns true if a given LValue is a string or number
// otherwise false.
func LVCanConvToString(v LValue) bool {
	switch v.(type) {
	case LString, LNumber, LInteger:
		return true
	default:
		return false
	}
}

// LVAsNumber tries to convert a given LValue to a number (float view).
// The integer subtype converts to its float value; use parseNumber when
// the syntactic subtype of a string must be preserved.
func LVAsNumber(v LValue) LNumber {
	switch lv := v.(type) {
	case LNumber:
		return lv
	case LInteger:
		return LNumber(float64(lv))
	case LString:
		if num, err := parseNumber(string(lv)); err == nil {
			return LVAsNumber(num)
		}
	}
	return LNumber(0)
}

type LNilType struct{}

func (nl *LNilType) String() string   { return "nil" }
func (nl *LNilType) Type() LValueType { return LTNil }

var LNil = LValue(&LNilType{})

type LBool bool

func (bl LBool) String() string {
	if bool(bl) {
		return "true"
	}
	return "false"
}
func (bl LBool) Type() LValueType { return LTBool }

var LTrue = LBool(true)
var LFalse = LBool(false)

// LInteger is the 64-bit integer subtype of "number" (5.3+, U2/D1).
// Integer-ness is observable: tostring(2) == "2" while tostring(2.0) ==
// "2.0" (once float printing re-pins at U5), table keys normalize
// t[2.0] to the integer 2, and //, shifts and integer for-loops require
// it. Arithmetic on mixed subtypes promotes to float.
type LInteger int64

func (nm LInteger) String() string {
	return strconv.FormatInt(int64(nm), 10)
}

func (nm LInteger) Type() LValueType { return LTInteger }

// fmt.Formatter interface
func (nm LInteger) Format(f fmt.State, c rune) {
	switch c {
	case 'q', 's':
		defaultFormat(nm.String(), f, c)
	case 'b', 'c', 'd', 'o', 'x', 'X', 'U', 'i':
		defaultFormat(int64(nm), f, c)
	case 'e', 'E', 'f', 'F', 'g', 'G':
		defaultFormat(float64(nm), f, c)
	default:
		defaultFormat(int64(nm), f, c)
	}
}

type LString string

func (st LString) String() string   { return string(st) }
func (st LString) Type() LValueType { return LTString }

// fmt.Formatter interface
func (st LString) Format(f fmt.State, c rune) {
	switch c {
	case 'd', 'i':
		if nm, err := parseNumber(string(st)); err != nil {
			defaultFormat(LVAsNumber(nm), f, 'd')
		} else {
			defaultFormat(string(st), f, 's')
		}
	default:
		defaultFormat(string(st), f, c)
	}
}

// String is the engine-wide float spelling: the stock tostringbuffFloat
// (%.15g, re-read, %.17g, ".0" on integer-looking results — U3i, the D7
// re-pin pulled forward because math.lua's numeral round-trip asserts
// tostring(1.0) == "1.0"). The old Go dialect printed "1" there.
func (nm LNumber) String() string {
	return numberToStringStock(float64(nm))
}

func (nm LNumber) Type() LValueType { return LTNumber }

// fmt.Formatter interface
func (nm LNumber) Format(f fmt.State, c rune) {
	switch c {
	case 'q', 's':
		defaultFormat(nm.String(), f, c)
	case 'b', 'c', 'd', 'o', 'x', 'X', 'U':
		defaultFormat(int64(nm), f, c)
	case 'e', 'E', 'f', 'F', 'g', 'G':
		defaultFormat(float64(nm), f, c)
	case 'i':
		defaultFormat(int64(nm), f, 'd')
	default:
		if isInteger(nm) {
			defaultFormat(int64(nm), f, c)
		} else {
			defaultFormat(float64(nm), f, c)
		}
	}
}

type LTable struct {
	Metatable LValue

	// readonly is the N2 sandbox's one-way write guard (the embed's
	// WithGlobalsProtection; the analog of the C engine interposing
	// luaH_* setters). Lua-side writes — the bytecode set paths, the
	// host-API SetTable/SetField, rawset, and the table library via
	// lua_seti — raise "Attempt to modify a readonly table". Raw Go
	// writes (the LTable methods, LState.RawSet*) bypass it: that is the
	// host staging seam. See SetReadonly.
	readonly bool
	// acct, when non-nil, is the per-VM memory accountant this table's
	// creation and growth charge (N2; nil = unaccounted — the registry
	// seed tables, which are built before their Global exists).
	acct *Global

	array []LValue

	// P2 hash part: one open-addressed node array holds every hash-part
	// key — strings and everything else share a single first-insertion
	// order (the invariant the old strdict/dict + keys/k2i quartet kept,
	// at four allocations and a per-write double bookkeeping). A slot is
	// storage, not order: iteration order rides the intrusive ins links
	// (first…tail), so rehashing never reorders anything. A nil key
	// marks a never-used slot; deletion writes LNil over the value (a
	// tombstone) and leaves key and link alone, so a deleted-then-
	// reinserted key lands back in its original position — the order
	// behavior the official suite and the P2 probe pin.
	nodes []hnode // len is a power of two; nil until the first hash write
	mask  uint32  // len(nodes)-1
	first int32   // insertion chain head; -1 when no hash entry ever
	tail  int32   // insertion chain tail; -1 likewise
	used  int32   // nodes holding a key (live + tombstones)
	// strKeys counts live string keys (the old len(strdict)); the two
	// *Charged flags mirror the old strdict==nil / dict==nil one-shot
	// materialization events so the per-VM budget charges (including
	// their firing order relative to the key kinds) stay identical.
	strKeys     int32
	strdCharged bool
	dictCharged bool
}

// hnode is one hash-part slot: two interface cells, the cached hash and
// the order link — 40 bytes. hash is stored so a rehash never re-hashes
// a string, and it filters the full interface compare while probing.
type hnode struct {
	key  LValue // nil interface = never-used; a deleted key stays (tombstone)
	val  LValue // LNil = tombstone; live entries never hold LNil
	hash uint32
	ins  int32 // insertion-order link to the next node; -1 = tail
}

func (tb *LTable) String() string   { return fmt.Sprintf("table: %p", tb) }
func (tb *LTable) Type() LValueType { return LTTable }

type LFunction struct {
	IsG       bool
	Env       *LTable
	Proto     *FunctionProto
	GFunction LGFunction
	Upvalues  []*Upvalue
}
type LGFunction func(*LState) int

func (fn *LFunction) String() string   { return fmt.Sprintf("function: %p", fn) }
func (fn *LFunction) Type() LValueType { return LTFunction }

type Global struct {
	MainThread    *LState
	CurrentThread *LState
	Registry      *LTable
	Global        *LTable

	// randState is the math.random xoshiro256** state (U3b). Stock keeps
	// it in a userdata upvalue shared by the math functions; the Go side
	// hangs it on the Global — one stream per lua_State, shared across
	// coroutines, observationally identical.
	randState rngState

	// resumeDepth counts nested coroutine resumes (U4d). Stock bounds
	// them with nCcalls >= LUAI_MAXCCALLS ("C stack overflow"); the fork
	// has no C-call counter, so coResume tracks the resume chain depth
	// here — without it, mutual resume recursion overflows the Go stack
	// fatally (official coroutine.lua's "infinite recursion" block).
	resumeDepth int

	// gcparams are the six collectgarbage("param", ...) tunables' codes
	// (U5c, stock g->gcparams + luaO_codeparam's floating-point byte):
	// minormul, majorminor, minormajor, pause, stepmul, stepsize.
	// Protocol-exact storage, no-op tuning — the fork has no incremental
	// GC to drive (upgrade plan D10).
	gcparams [6]uint8

	// builtinPcall/builtinXPCall/builtinPairs are the marker-intercept
	// identities (U5d, row 22): the VM's call dispatch compares the
	// RESOLVED callee against these — any alias of the builtin object
	// intercepts, a replaced global does not. pcall/xpcall become
	// protected marker frames (stock's lua_pcallk continuations) and
	// pairs' __pairs call a continuation marker (stock pairscont), so
	// yields crossing them suspend instead of unwinding the Go stack.
	builtinPcall  *LFunction
	builtinXPCall *LFunction
	builtinPairs  *LFunction

	// The __gc finalizer queue (U5f, stock g->tmudata). The fork's
	// collector is Go's GC: a dead object surfaces only through
	// runtime.SetFinalizer, whose callback runs on Go's finalizer
	// goroutine and must ONLY enqueue here (no LState touches). The
	// drain points — the luaC_checkGC analogs in the VM, the
	// collectgarbage full arm, and Close — run the queued __gc
	// metamethods on the running thread. finPending is the lock-free
	// gate for the per-instruction checks.
	finMu      sync.Mutex
	finQ       []LValue
	finPending atomic.Bool
	// finDepth suppresses nested drains: a finalizer's own allocation
	// sites do not re-enter (stock's GCSTPGC around GCTM) — their
	// objects stay queued for the next drain.
	finDepth int
	// finRegT/finRegU weakly track every object this state registered a
	// runtime.SetFinalizer on, so Close can unregister them (gc.go's
	// clearRegisteredFinalizers). The finalizer closure captures this
	// Global, which transitively references every registered object
	// through the globals table — runtime.SetFinalizer's documented
	// self-reference trap — so even one pending finalizer (the io
	// standard streams alone) pins its object and, with it, the whole
	// closed state graph forever. Weak, because a strong list would pin
	// the registered objects themselves and defeat mid-life finalizer
	// firing (the U5f drain semantics).
	finRegT []weak.Pointer[LTable]
	finRegU []weak.Pointer[LUserData]

	builtinMts map[int]LValue
	tempFiles  []*os.File
	gccount    int32

	// N2 per-VM memory budget (SetMemoryBudgetBytes): the accounted
	// allocation counter charged at the big allocation seams — table
	// creation/growth (the same per-slot estimates collectgarbage
	// "count" uses) and the string builders (concat, string.rep,
	// table.concat). Never refunded: the counter is monotone, the
	// stopped-GC watermark posture the wasm engine bought by
	// construction (Go's collector frees underneath; the figure is a
	// high-water budget gauge, not a live-heap readout). memLimit 0 =
	// unlimited. memOver is sticky once crossed; the raise points (the
	// seam sites and the per-instruction boundary check) surface it as a
	// pcall-catchable "not enough memory" — stock luaM_error's text.
	// Fields are written at setup time only (before the first run), on
	// the goroutine that then runs the loop; reads afterwards are
	// race-free by the happens-before of thread creation.
	memLimit int64
	memUsed  atomic.Int64
	memOver  atomic.Bool

	// N2 coroutine spawn cap (Options.MaxCoroutines): coroutines are
	// goroutines here, so unbounded coroutine.create needs a ceiling.
	// liveThreads counts threads created by coroutine.create/wrap and
	// not yet killed (every terminal path funnels through kill());
	// maxCoroutines 0 = unlimited.
	maxCoroutines int
	liveThreads   atomic.Int32
}

// luaMaxCCalls mirrors stock LUAI_MAXCCALLS (ldo.h): the nested C-call
// ceiling, applied here to nested resumes.
const luaMaxCCalls = 200

type LState struct {
	G       *Global
	Parent  *LState
	Env     *LTable
	Panic   func(*LState)
	Dead    bool
	Options Options

	stop         int32
	reg          *registry
	stack        callFrameStack
	alloc        *allocator
	currentFrame *callFrame
	wrapped      bool
	uvcache      *Upvalue
	hasErrorFunc bool
	mainLoop     func(*LState, *callFrame)
	ctx          context.Context
	ctxCancelFn  context.CancelFunc
	// N2 coroutine cap: coCounted marks a thread created through
	// coroutine.create/wrap (counted in G.liveThreads); kill() releases
	// it exactly once, via coReleased.
	coCounted  bool
	coReleased atomic.Bool
	// deadErr records the error object a coroutine died with (U3h:
	// coroutine.close returns it as false+err for an error-dead
	// coroutine, stock lua_closethread's status).
	deadErr LValue
	// tbclist is the to-be-closed stack (U4, stock L->tbclist): entries
	// keyed by the same absolute register indexes the upvalue registry
	// uses, innermost (highest) first, so cross-frame ordering holds and
	// closeTBC(level) reaches exactly the entries a block/function owns.
	tbclist *tbcEntry
	// noyieldDepth is stock's nny (U5d, row 22): every call driven from
	// a Go frame (ls.Call — sort comparators, gsub replacements, the
	// tostring/concat metamethod dispatch, __close under the error
	// paths) bumps it; coroutine.isyieldable answers false and a yield
	// attempt raises the catchable "attempt to yield across a C-call
	// boundary". Marker-frame bodies (pcall/xpcall/pairs/__close from
	// the VM) are pushed by the loop itself and never bump it.
	noyieldDepth int
	// Debug-hook state (U5f, stock's L->hook/hookcount/oldpc/allowhook,
	// per-thread since coroutines are *LState; a fresh thread inherits
	// nothing, like lua_newthread). hookMask's bits are hookMask* below
	// (stock LUA_MASK* numbers). hookOldPc is THREAD-global like stock's
	// L->oldpc: the index of the last instruction the line seam visited,
	// in ANY frame's proto — a running hook function's own traced
	// instructions advance it too (stock's cross-proto oldpc behavior,
	// which a byte-exact line trace reproduces). hookBusy is stock's
	// allowhook=0 while a hook runs. retHookPending marks a C function
	// that suspended mid-call (coroutine.yield): its RETURN event fires
	// when the thread resumes (stock finishCfunc's redo-poscall).
	// hookFTransfer/hookNTransfer are stock's L->transferinfo — the
	// register window being transferred by the event the hook was last
	// invoked for (call: (1, nparams), return: (firstres-func, nres),
	// line/count: (0,0)); thread-global and overwritten by every event,
	// they are what getinfo 'r' reports over a CIST_HOOKED frame (U5f).
	hookFn         *LFunction
	hookMask       uint8
	hookCount      int
	hookCountdown  int
	hookOldPc      int
	hookBusy       bool
	retHookPending bool
	hookFTransfer  int
	hookNTransfer  int
}

// tbcEntry is one to-be-closed variable registration (U4). The value
// lives in the register slot; it is read at close time (assignment to a
// <close> local is a compile error, so the slot is stable).
type tbcEntry struct {
	idx  int
	prev *tbcEntry
}

func (ls *LState) String() string   { return fmt.Sprintf("thread: %p", ls) }
func (ls *LState) Type() LValueType { return LTThread }

type LUserData struct {
	Value     interface{}
	Env       *LTable
	Metatable LValue
	// UserValue is the debug.getuservalue slot (U5c, stock uvalues[0]):
	// any Lua value, nil by default — distinct from Env, the legacy 5.1
	// environment table the fork's GetFEnv reads. UserValues is the slot
	// capacity (stock newuserdatauv's nuvalue): stock's Lua-visible
	// userdata — the io files — are created with ZERO slots, so
	// debug.get/setuservalue answer nil for them (db.lua:434 pins it);
	// NewUserData leaves 0 for the same posture.
	UserValue  LValue
	UserValues int8
	// Light marks a light userdata (U5c, stock LUA_VLIGHTUSERDATA): a
	// bare pointer in Lua's clothing. It still types as "userdata"
	// (5.5's typename table maps both tags to it), but full-userdata
	// argument checks reject it naming "light userdata" (luaL_typeerror
	// special case), base setmetatable refuses it, and its metatable is
	// the per-basic-type table (debug.setmetatable territory), never a
	// per-object one. debug.upvalueid is the only creator.
	Light bool
}

func (ud *LUserData) String() string   { return fmt.Sprintf("userdata: %p", ud) }
func (ud *LUserData) Type() LValueType { return LTUserData }

type LChannel chan LValue

func (ch LChannel) String() string   { return fmt.Sprintf("channel: %p", ch) }
func (ch LChannel) Type() LValueType { return LTChannel }

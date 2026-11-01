// Package host is the embedder's engine over the pure-Go Lua 5.5.1
// interpreter: the Ultima (pure-Go Redis clone) EVAL/SCRIPT posture. It
// mirrors glasm's host/ API where the mechanism transfers — Engine (the
// compile cache), VM pool, RegisterGlobal/RegisterValue, EventSink,
// Seed, Deadline — and deletes the wasm mechanisms outright: no runtime
// blob, no wire protocol, no hostfn staging buffers, no per-image module
// registries. A host function is a Go closure running on the VM's
// lock-holding goroutine; a script runs as a cached FunctionProto on a
// plain LState.
//
// Differences from glasm host/, all mechanism-born:
//
//   - NewEngine cannot fail (no blob to verify, no cache dir to open),
//     so it returns no error.
//   - There is no ErrScriptBound / one-script-per-image law: protos are
//     self-contained, so any VM can run any script, serially (the A9
//     one-run-at-a-time mutex still holds). The pool still keys warm VMs
//     by script identity because that is what makes reuse correct.
//   - There is no TrapError: no wasm traps exist. Engine-level failures
//     (interpreter panics outside the Lua error protocol, conversion
//     failures) return as ordinary Go errors, distinguishable from
//     *ScriptError.
//   - Deadline is the interpreter's native per-instruction context
//     check (LState.SetContext): pure-Go and preemptible at every
//     instruction, so the glasm row-99/100 STW-wedge class cannot occur.
//     Kill is a per-run context cancel.
//   - Pooled-VM recycling is runs-served, not a memory watermark: the
//     native LState is garbage-collected live (glasm froze guest GC by
//     necessity, making growth monotonic; here it is not). The memory
//     budget and its accounting arrive in N2.
//
// The sandbox posture (globals protection, stdlib selection, coroutine
// cap, memory budget) is N2 — until then engines open the full standard
// library, and the embedder is responsible for what scripts it runs.
package host

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	lua "github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/parse"
)

// HostFunc is a Lua-visible function implemented in Go (the redis.call
// mechanism). It executes on the goroutine that holds the VM's lock —
// the same goroutine running the script — so it may inspect VM state
// freely, but it must NOT call vm.Run (the lock is not reentrant;
// ErrReentrant is raised if it does). Arguments and returns are seam
// Values (owning copies — they alias nothing inside the VM).
type HostFunc func(vm *VM, args []Value) ([]Value, error)

// Option configures an Engine.
type Option func(*options)

type options struct {
	eventSink   func(vm *VM, args []Value) // print observer
	vmCacheSize int                        // warm-VM pool capacity (0 = fresh-VM-per-run)
	scriptCache int                        // script cache bound (<=0 = unbounded, the default)
}

// WithEventSink observes every guest print() (arguments as seam Values).
// Called on the lock-holding goroutine.
func WithEventSink(fn func(vm *VM, args []Value)) Option {
	return func(o *options) { o.eventSink = fn }
}

// WithVMCacheSize enables a bounded pool of warm VMs, keyed by script
// identity: Engine.Run reuses a pooled VM bound to the same source
// instead of building a fresh one per run — the LState build (stdlib
// open, host staging) is amortized to zero for repeat scripts. Each VM
// still serves one run at a time (A9), guest-level script errors keep
// the VM pooled (the stack unwinds to base), engine-level failures
// close it, and a VM that has served vmRecycleRuns runs is recycled
// (closed, not reused) — the native stand-in for glasm's frozen-GC
// watermark. n <= 0 (the default) keeps the fresh-VM-per-run posture.
func WithVMCacheSize(n int) Option {
	return func(o *options) { o.vmCacheSize = n }
}

// WithCompileMemCache bounds the script (proto) cache to n entries. The
// default is unbounded — the Redis SCRIPT LOAD posture (scripts
// accumulate until the embedder flushes); a daemon compiling unbounded
// distinct sources should bound it.
func WithCompileMemCache(n int) Option {
	return func(o *options) { o.scriptCache = n }
}

// Engine is the shared, process-wide scripting engine: the compile
// cache and the host-function registry. VMs own their LStates. Safe for
// concurrent use.
type Engine struct {
	opts options

	mu       sync.Mutex
	hostFns  []registeredFn
	hostVals []registeredVal
	vms      int // live VMs (RegisterGlobal freezes once the first exists)
	closed   bool

	// the script cache in one of two postures (NewEngine): an
	// unbounded map (the default) or a bounded LRU
	// (WithCompileMemCache). Both hold *Script pointers — one pointer
	// per (SHA-1, name) identity is the invariant (see Compile).
	cache       map[string]*Script
	scriptCache *lru[string, *Script]

	// pool is the warm-VM cache (WithVMCacheSize); nil when disabled.
	pool *lru[string, *VM]
}

type registeredFn struct {
	table, name string
	fn          HostFunc
}

type registeredVal struct {
	table, name string
	v           Value
}

// luaIdent reports whether s is a plain Lua identifier (glasm staged
// RegisterValue names as source; natively they go through SetField, but
// the identifier rule stays part of the API contract).
func luaIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// RegisterValue installs <table>.<name> as a constant Lua value (nil,
// booleans, finite numbers, strings) in every VM — the
// redis.LOG_WARNING / redis.REDIS_VERSION mechanism. Same freeze rule
// as RegisterGlobal.
func (e *Engine) RegisterValue(table, name string, v Value) error {
	if !luaIdent(table) || !luaIdent(name) {
		return errors.New("host: RegisterValue requires Lua-identifier table and name")
	}
	switch v.Kind {
	case KindNil, KindFalse, KindTrue, KindNumber, KindInteger, KindString:
	default:
		return errors.New("host: RegisterValue supports only nil/bool/number/string values")
	}
	if v.Kind == KindNumber && (math.IsInf(v.Num, 0) || math.IsNaN(v.Num)) {
		return errors.New("host: RegisterValue number must be finite")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.vms > 0 {
		return errors.New("host: RegisterValue after NewVM (the registry is frozen once VMs exist)")
	}
	for _, r := range e.hostVals {
		if r.table == table && r.name == name {
			return fmt.Errorf("host: RegisterValue duplicate %s.%s", table, name)
		}
	}
	e.hostVals = append(e.hostVals, registeredVal{table, name, v})
	return nil
}

// NewEngine returns a ready engine.
func NewEngine(opts ...Option) *Engine {
	o := options{}
	for _, f := range opts {
		f(&o)
	}
	e := &Engine{opts: o}
	if o.scriptCache > 0 {
		e.scriptCache = newLru[string, *Script](o.scriptCache)
	} else {
		e.cache = map[string]*Script{}
	}
	if o.vmCacheSize > 0 {
		e.pool = newLru[string, *VM](o.vmCacheSize)
	}
	return e
}

// RegisterGlobal installs <table>.<name> as a Lua function backed by fn
// (e.g. RegisterGlobal("redis", "call", ...) — the redis.call seam, now
// a direct Go closure). Must be called before the first NewVM — each VM
// snapshots the registry at creation.
func (e *Engine) RegisterGlobal(table, name string, fn HostFunc) error {
	if table == "" || name == "" {
		return errors.New("host: RegisterGlobal requires non-empty table and name")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.vms > 0 {
		return errors.New("host: RegisterGlobal after NewVM (the registry is frozen once VMs exist)")
	}
	for _, r := range e.hostFns {
		if r.table == table && r.name == name {
			return fmt.Errorf("host: RegisterGlobal duplicate %s.%s", table, name)
		}
	}
	e.hostFns = append(e.hostFns, registeredFn{table, name, fn})
	return nil
}

// Script is one compiled script: the durable proto plus its Redis
// identity. Deterministic for a given (source, name) — cacheable by
// SHA-1. The proto is immutable after compile and safe to share across
// VMs and goroutines.
type Script struct {
	// SHA1 is the hex SHA-1 of the source bytes — the SCRIPT LOAD /
	// EVALSHA identity (Redis model).
	SHA1 string
	// Proto is the compiled chunk (an *lua.FunctionProto).
	Proto *lua.FunctionProto

	name string
}

// Name returns the chunk name the script was compiled under (error
// position prefixes use it).
func (s *Script) Name() string { return s.name }

// identity is the script's cache/pool key: (SHA-1, chunk name).
func (s *Script) identity() string { return s.SHA1 + "\x00" + s.name }

// Compile parses and compiles source, caching by (SHA-1, name).
// Compile errors (syntax) return the frontend's message verbatim.
//
// Identity law: a cache-resident source is served as ONE *Script
// pointer; a source evicted from a bounded cache compiles to a fresh
// pointer. Consumers key scripts by (SHA-1, name).
func (e *Engine) Compile(source []byte, name string) (*Script, error) {
	if name == "" {
		name = "=script"
	}
	sum := sha1.Sum(source)
	sha := hex.EncodeToString(sum[:])
	key := sha + "\x00" + name

	if e.scriptCache != nil {
		if s, ok := e.scriptCache.get(key); ok {
			return s, nil
		}
	} else {
		e.mu.Lock()
		if s, ok := e.cache[key]; ok {
			e.mu.Unlock()
			return s, nil
		}
		e.mu.Unlock()
	}

	chunk, err := parse.Parse(strings.NewReader(string(source)), name)
	if err != nil {
		return nil, err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return nil, err
	}
	s := &Script{SHA1: sha, Proto: proto, name: name}
	if e.scriptCache != nil {
		e.scriptCache.put(key, s)
	} else {
		e.mu.Lock()
		// A concurrent Compile of the same source may have won the race:
		// pointer identity per (SHA-1, name) is the invariant consumers
		// rely on — never hand out two pointers for one source.
		if prev, ok := e.cache[key]; ok {
			e.mu.Unlock()
			return prev, nil
		}
		e.cache[key] = s
		e.mu.Unlock()
	}
	return s, nil
}

// vmRecycleRuns is the pooled-VM age limit: a VM that has served this
// many runs is closed instead of pooled. Package var so the gates can
// exercise the recycle path (white-box).
var vmRecycleRuns = 1024

// Run compiles nothing: it executes s against a fresh VM (the default
// posture — maximum parallelism, zero shared mutable guest state), or
// against a pooled warm VM when WithVMCacheSize is set (repeat scripts
// skip the LState build entirely; each VM still serves one run at a
// time).
func (e *Engine) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error) {
	if s == nil {
		return Result{}, errors.New("host: nil script")
	}
	key := s.identity()
	if e.pool != nil {
		// Atomic take: two concurrent Runs must never hold one VM.
		pooled, ok := e.pool.take(key)
		if ok {
			res, err := pooled.Run(ctx, s, opt)
			e.recycle(pooled, key, err)
			return res, err
		}
	}
	vm, err := e.NewVM()
	if err != nil {
		return Result{}, err
	}
	res, err := vm.Run(ctx, s, opt)
	if e.pool == nil || !poolable(err) {
		_ = vm.Close()
		return res, err
	}
	e.recycle(vm, key, err)
	return res, err
}

// poolable reports whether err leaves the VM reusable: guest-level
// errors unwind the Lua stack to base, while engine-level failures
// (a Go panic outside the Lua error protocol, staging failures) leave
// unknown state — close instead of pooling.
func poolable(err error) bool {
	var se *ScriptError
	return err == nil || errors.As(err, &se)
}

// recycle returns vm to the pool, or closes it when it has served its
// run budget or arrives at a full pool as the entry that loses the LRU
// race. Victims are closed OUTSIDE the pool lock.
func (e *Engine) recycle(vm *VM, key string, runErr error) {
	if !poolable(runErr) || vm.runsServed() >= vmRecycleRuns {
		_ = vm.Close()
		return
	}
	victim, ok := e.pool.put(key, vm)
	if ok && victim != vm {
		_ = victim.Close()
	}
}

// Close marks the engine closed; live VMs keep working (they own their
// LStates) but no new ones may be made. Pooled VMs are engine-owned and
// are drained here.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.pool != nil {
		for _, v := range e.pool.drain() {
			_ = v.Close()
		}
	}
	return nil
}

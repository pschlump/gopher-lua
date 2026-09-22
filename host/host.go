package host

// Engine: process-wide. Owns the compile cache and the host-function
// registry; VMs own their wazero runtimes (see vm.go for why they are not
// shared). Safe for concurrent use.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/luawasm"
	"github.com/pschlump/gopher-lua/parse"
)

// HostFunc is a Lua-visible function implemented in Go (the redis.call
// mechanism). It executes on the goroutine that holds the VM's image
// lock — the same goroutine running the script — so it may inspect VM
// state freely, but it must NOT call vm.Run (the lock is not reentrant;
// ErrReentrant is returned if it does).
type HostFunc func(vm *VM, args []Value) ([]Value, error)

// Option configures an Engine.
type Option func(*options)

type options struct {
	blob      []byte                     // runtime blob override (still SHA-pinned)
	maxMem    int64                      // per-VM allocation budget bytes (0 = unlimited; the blob's 256 MiB linear-memory max is the hard backstop either way)
	eventSink func(vm *VM, args []Value) // EVT_PRINT observer (already wire-decoded)
}

// WithRuntimeBlob replaces the embedded production blob (a staged/test
// build). The replacement must still pass the frozen-surface checks.
func WithRuntimeBlob(bin []byte) Option {
	return func(o *options) { o.blob = bin }
}

// WithMemoryBudgetBytes caps each VM's Lua allocation budget; crossing it
// raises a clean pcall-catchable "not enough memory" error inside the
// script (rt_set_memlimit, M6d D3) — never a trap, never host OOM.
// Applied at VM creation (the budget must precede lnewstate so the whole
// VM lifetime counts).
func WithMemoryBudgetBytes(n int64) Option {
	return func(o *options) { o.maxMem = n }
}

// WithEventSink observes every guest print() (arguments decoded from the
// wire protocol). Called on the lock-holding goroutine.
func WithEventSink(fn func(vm *VM, args []Value)) Option {
	return func(o *options) { o.eventSink = fn }
}

// Engine is the shared, process-wide scripting engine.
type Engine struct {
	opts options

	mu      sync.Mutex
	cache   map[string]*Script
	hostFns []registeredFn
	vms     int // live VMs (RegisterGlobal freezes once the first exists)
	closed  bool
}

type registeredFn struct {
	table, name string
	fn          HostFunc
}

// NewEngine verifies the runtime blob and returns a ready engine.
func NewEngine(opts ...Option) (*Engine, error) {
	o := options{blob: prodBlob}
	for _, f := range opts {
		f(&o)
	}
	if err := verifyBlob(o.blob); err != nil {
		return nil, err
	}
	return &Engine{opts: o, cache: map[string]*Script{}}, nil
}

// RegisterGlobal installs <table>.<name> as a Lua function backed by fn
// (e.g. RegisterGlobal("redis", "call", ...)). Must be called before the
// first NewVM — each VM snapshots the registry at creation.
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

// Script is one compiled script: the durable wasm artifact plus its Redis
// identity. Deterministic for a given (source, name) — cacheable by SHA.
type Script struct {
	// SHA1 is the hex SHA-1 of the source bytes — the SCRIPT LOAD /
	// EVALSHA identity (Redis model).
	SHA1 string
	// Wasm is the compiled module — the artifact cmd/luawasmc writes.
	Wasm []byte

	name string
}

// Name returns the chunk name the script was compiled under (error
// position prefixes use it).
func (s *Script) Name() string { return s.name }

// Compile parses and lowers source to a wasm module, caching by
// (SHA-1, name). Compile errors (syntax, v1-unsupported constructs such
// as coroutines) return the frontend's message verbatim.
func (e *Engine) Compile(source []byte, name string) (*Script, error) {
	if name == "" {
		name = "=script"
	}
	sum := sha1.Sum(source)
	sha := hex.EncodeToString(sum[:])
	key := sha + "\x00" + name
	e.mu.Lock()
	if s, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()

	// the same three-stage chain as testdiff.CompileSource, minus the
	// testdiff import (that package hosts the wasmtime oracle engines —
	// cgo must never reach a daemon through this package)
	chunk, err := parse.Parse(strings.NewReader(string(source)), name)
	if err != nil {
		return nil, err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return nil, err
	}
	bin, err := luawasm.Compile(proto, name)
	if err != nil {
		return nil, err
	}
	s := &Script{SHA1: sha, Wasm: bin, name: name}
	e.mu.Lock()
	e.cache[key] = s
	e.mu.Unlock()
	return s, nil
}

// Run compiles nothing: it executes s against a fresh VM (the v1 default
// deployment — maximum parallelism, zero shared mutable guest state).
func (e *Engine) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error) {
	vm, err := e.NewVM()
	if err != nil {
		return Result{}, err
	}
	defer vm.Close()
	return vm.Run(ctx, s, opt)
}

// Close marks the engine closed; live VMs keep working (they own their
// runtimes) but no new ones may be made.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

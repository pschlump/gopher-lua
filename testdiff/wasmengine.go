package testdiff

// The wasm-backend engine (M4): compiles Lua through the gopher-lua
// frontend + luawasm backend and runs the emitted module against the C
// runtime on wasmtime — the third differential engine. The harness shim
// comes from the runtime module's install_shims (print→host event,
// deterministic os.*, host RNG for math.random), matching the interp
// and clua engines' contract. lua51SjljWasm (embedded in clua.go) is
// the runtime blob.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"

	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/luawasm"
	"github.com/pschlump/gopher-lua/parse"
)

// gcStop runs collectgarbage('stop') in the runtime state via the M2
// driver's ldostring (v1 mitigation — see the Run comment).
func (e *WasmEngine) gcStop(store *wt.Store, rtInst *wt.Instance, call func(*wt.Instance, string, ...interface{}) (interface{}, error), L interface{}, name string) error {
	inAddr, err := call(rtInst, "linbuf")
	if err != nil {
		return err
	}
	nameAddr, err := call(rtInst, "lnamebuf")
	if err != nil {
		return err
	}
	src := []byte("collectgarbage('stop')")
	if len(name) > 500 {
		name = name[:500]
	}
	mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	in, nameA := uint32(toI32(inAddr)), uint32(toI32(nameAddr))
	copy(mem[in:], src)
	copy(mem[nameA:], []byte(name))
	_, err = call(rtInst, "ldostring", L, int(in), len(src), int(nameA), 1)
	return err
}

func toI32(v interface{}) int32 {
	switch x := v.(type) {
	case int32:
		return x
	case uint64:
		return int32(x)
	case int64:
		return int32(x)
	case float64:
		return int32(x)
	}
	return 0
}

// renderErrValue: a staged non-string error TValue (16 bytes) → the
// interp's ValueRepr shape for the deterministic tags. Matches PrintArg /
// ValueRepr: nil, boolean, number (NumRepr); collectables render as bare
// markers (addresses are engine-local — the suite avoids them).
func renderErrValue(raw []byte) string {
	tag := raw[8]
	switch tag {
	case 0:
		return "nil"
	case 1: // boolean: value.b at offset 0
		if raw[0] != 0 {
			return "true"
		}
		return "false"
	case 3: // number: f64 at offset 0
		bits := uint64(0)
		for i := 7; i >= 0; i-- {
			bits = bits<<8 | uint64(raw[i])
		}
		return NumRepr(math.Float64frombits(bits))
	case 4:
		return "" // strings go through the bytes path
	case 5:
		return "<table>"
	case 6:
		return "<function>"
	}
	return "<value>"
}

// CompileSource runs the frontend and the backend: source → script.wasm.
// This is what cmd/luawasmc saves to disk.
func CompileSource(source []byte, name string) ([]byte, error) {
	chunk, err := parse.Parse(strings.NewReader(string(source)), name)
	if err != nil {
		return nil, err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return nil, err
	}
	return luawasm.Compile(proto, name)
}

// WasmEngine compiles and executes scripts through the wasm backend.
type WasmEngine struct {
	name string
	// NoopHosts: replace host shims with no-ops (trap triage)
	NoopHosts bool
	// Precompiled module bytes (nil → compile from source)
	Precompiled []byte
	// SkipUnsupported: scripts using v1-unsupported opcodes produce a
	// SKIP-UNSUPPORTED log instead of an engine error (corpus tests)
	SkipUnsupported bool
}

// NewWasmEngine returns a wasm-backend engine with the given name.
func NewWasmEngine(name string) *WasmEngine { return &WasmEngine{name: name} }

func (e *WasmEngine) Name() string { return e.name }

func (e *WasmEngine) Run(c Case) (log []string) {
	defer func() {
		if p := recover(); p != nil {
			log = []string{fmt.Sprintf("ENGINE-PANIC\t%v\t%s", p, debug.Stack())}
		}
	}()
	var log2 []string
	log = log2
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	bin := e.Precompiled
	if bin == nil {
		var err error
		bin, err = CompileSource(c.Source, c.Name)
		if err != nil {
			if e.SkipUnsupported && strings.Contains(err.Error(), "backend v1") {
				return []string{"SKIP-UNSUPPORTED\t" + err.Error()}
			}
			// parse/compile failures are script-level ERRORs (the interp
			// engine reports them the same way), not engine errors
			return []string{"ERROR\t" + strconv.Quote(err.Error())}
		}
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	defer os.Chdir(wd)

	_ = context.Background()
	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true)
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)

	var guestMem *wt.Memory
	memRead := func(ptr int32, length int32) []byte {
		if guestMem == nil {
			return nil
		}
		base := guestMem.Data(store)
		size := guestMem.DataSize(store)
		if uintptr(ptr)+uintptr(length) > size {
			return nil
		}
		return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base)+uintptr(ptr))), length)
	}

	rng := rand.New(rand.NewSource(42))
	linker := wt.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	decoder := &CLua{}
	eventFn := func(kind, ptr, length int32) {
		if raw := memRead(ptr, length); raw != nil {
			decoder.onEvent(raw, kind, emit)
		}
	}
	if e.NoopHosts {
		eventFn = func(kind, ptr, length int32) {}
	}
	if err := linker.DefineFunc(store, "host", "event", eventFn); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	if err := linker.DefineFunc(store, "host", "random01",
		func() float64 { return rng.Float64() }); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	if err := linker.DefineFunc(store, "host", "randomint",
		func(lo, hi int32) int32 { return int32(rng.Intn(int(hi-lo+1)) + int(lo)) }); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	if err := linker.DefineFunc(store, "host", "randomseed",
		func(seed int64) { rng = rand.New(rand.NewSource(seed)) }); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	// host.wasm_dispatch: the M5a reverse seam — the C runtime's
	// precall_wasm adapter dispatches compiled protos through the host
	// into the script module's exported lua_dispatch (same store, nested
	// call). scriptInst is wired below once instantiated; until then (and
	// for stub hosts) this refuses with -3, which the adapter re-raises
	// as a Lua error.
	var scriptInst *wt.Instance
	dispatch := func(idx, frame, cl, nargs, want int32) int32 {
		if scriptInst == nil {
			return -3
		}
		fn := scriptInst.GetFunc(store, "lua_dispatch")
		if fn == nil {
			return -3
		}
		res, err := fn.Call(store, idx, frame, cl, nargs, want)
		if err != nil {
			// a trap inside the dispatch must not unwind through wasm:
			// surface as a refused dispatch; the adapter raises it
			return -3
		}
		if n, ok := res.(int32); ok {
			return n
		}
		return -3
	}
	if err := linker.DefineFunc(store, "host", "wasm_dispatch", dispatch); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}

	wasi := wt.NewWasiConfig()
	if err := wasi.PreopenDir(c.Dir, "/", true); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	stdoutFile, err := os.CreateTemp("", "wasmstdout-*")
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	stdoutFile.Close()
	defer os.Remove(stdoutFile.Name())
	if err := wasi.SetStdoutFile(stdoutFile.Name()); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	store.SetWasi(wasi)

	rtBin := lua51SjljWasm
	if p := os.Getenv("RTDBG"); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			rtBin = b
		}
	}
	rtMod, err := wt.NewModule(engine, rtBin)
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	rtInst, err := linker.Instantiate(store, rtMod)
	if err != nil {
		return []string{"ENGINE-ERROR\trt instantiate: " + err.Error()}
	}
	if err := linker.DefineInstance(store, "rt", rtInst); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	guestMem = rtInst.GetExport(store, "memory").Memory()

	call := func(inst *wt.Instance, fn string, args ...interface{}) (interface{}, error) {
		f := inst.GetFunc(store, fn)
		if f == nil {
			return nil, fmt.Errorf("missing export %s", fn)
		}
		for i, a := range args {
			switch v := a.(type) {
			case uint64:
				args[i] = int32(v)
			case int:
				args[i] = int32(v)
			}
		}
		return f.Call(store, args...)
	}

	scriptMod, err := wt.NewModule(engine, bin)
	if err != nil {
		return []string{"ENGINE-ERROR\tscript module: " + err.Error()}
	}
	si, err := linker.Instantiate(store, scriptMod)
	if err != nil {
		return []string{"ENGINE-ERROR\tscript instantiate: " + err.Error()}
	}
	scriptInst = si

	L, err := call(rtInst, "lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\tlnewstate: " + err.Error()}
	}
	// the ABI operates on this state (curL in rt_abi.c) — without it,
	// NULL-derefs land in mapped page 0 and surface as wild call_indirects
	if _, err := call(rtInst, "rt_set_state", L); err != nil {
		return []string{"ENGINE-ERROR\trt_set_state: " + err.Error()}
	}
	// v1 GC stop: register cells are not GC roots (the M3 ABI obligation —
	// a full GC-rooted frame arrives with the M6 arena lifecycle); any
	// luaC_checkGC inside library calls (e.g. table.sort's stack churn)
	// would collect tables referenced only from cells. Arena-reset replaces
	// GC for script runs anyway.
	if err := e.gcStop(store, rtInst, call, L, c.Name); err != nil {
		return []string{"ENGINE-ERROR\tgc stop: " + err.Error()}
	}
	// M5d: the gopher message dialect — the wasm engine pins interp ≡
	// wasm byte-for-byte; the clua oracle keeps the stock C 5.1 texts.
	if _, err := call(rtInst, "rt_set_dialect", 1); err != nil {
		return []string{"ENGINE-ERROR\tdialect: " + err.Error()}
	}
	av, err := call(rtInst, "rt_abi_version")
	if err != nil {
		return []string{"ENGINE-ERROR\tABI version: " + err.Error()}
	}
	if avv, ok := av.(int32); !ok || avv != 3 {
		return []string{fmt.Sprintf("ENGINE-ERROR\tABI version %v (need 3)", av)}
	}
	initStep := int32(2)
	if v := os.Getenv("INITSTEP"); v != "" {
		fmt.Sscanf(v, "%d", &initStep)
	}
	if _, err := call(scriptInst, "luawasm_init", initStep); err != nil {
		return append(log, "ENGINE-ERROR\tinit: "+err.Error())
	}
	gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
	frameCells := gv.I32()
	frame, err := call(rtInst, "rt_frame_alloc", int(frameCells)*16)
	if err != nil {
		return []string{"ENGINE-ERROR\tframe: " + err.Error()}
	}

	status, err := call(scriptInst, "lua_main", frame)
	if err != nil {
		return append(log, "ENGINE-ERROR\ttrap: "+err.Error())
	}
	stv, _ := status.(int32)
	if stv != 0 {
		// staged error: message bytes, or — for non-string error values —
		// the exact TValue rendered here (M5d: error(nil)/error(42) match
		// the interp's deterministic renders; table/function values carry
		// addresses and stay outside the byte-exact suite)
		emitted := false
		if buf, err := call(rtInst, "rt_frame_alloc", 4096); err == nil {
			if n, err := call(rtInst, "rt_err_stage_copy", buf, 4096); err == nil {
				nv, _ := n.(int32)
				if nv > 0 {
					if raw := memRead(buf.(int32), nv); raw != nil {
						emit("ERROR", strconv.Quote(string(raw)))
						emitted = true
					}
				}
			}
		}
		if !emitted {
			if vp, err := call(rtInst, "rt_err_value_ptr"); err == nil {
				if raw := memRead(vp.(int32), 16); raw != nil {
					emit("ERROR", strconv.Quote(renderErrValue(raw)))
				}
			}
		}
	}
	if _, err := call(rtInst, "lglobals"); err != nil {
		emit("GLOBALS", "<lglobals failed: "+err.Error()+">")
	}

	if out, err := os.ReadFile(stdoutFile.Name()); err == nil && len(out) > 0 {
		for _, line := range bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n")) {
			emit("STDOUT", string(line))
		}
	}
	return log
}

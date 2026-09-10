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
	"math/rand"
	"os"
	"strconv"
	"strings"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"

	"github.com/pschlump/gopher-lua"
	"github.com/pschlump/gopher-lua/luawasm"
	"github.com/pschlump/gopher-lua/parse"
)

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

func (e *WasmEngine) Run(c Case) []string {
	var log []string
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
			return []string{"ENGINE-ERROR\tcompile: " + err.Error()}
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
	scriptInst, err := linker.Instantiate(store, scriptMod)
	if err != nil {
		return []string{"ENGINE-ERROR\tscript instantiate: " + err.Error()}
	}

	emit("STEP", "pre-lnewstate")
	L, err := call(rtInst, "lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\tlnewstate: " + err.Error()}
	}
	// the ABI operates on this state (curL in rt_abi.c) — without it,
	// NULL-derefs land in mapped page 0 and surface as wild call_indirects
	if _, err := call(rtInst, "rt_set_state", L); err != nil {
		return []string{"ENGINE-ERROR\trt_set_state: " + err.Error()}
	}
	emit("STEP", "lnewstate done")
	av, err := call(rtInst, "rt_abi_version")
	if err != nil {
		return []string{"ENGINE-ERROR\tABI version: " + err.Error()}
	}
	if avv, ok := av.(int32); !ok || avv != 2 {
		return []string{fmt.Sprintf("ENGINE-ERROR\tABI version %v (need 2)", av)}
	}
	initStep := int32(2)
	if v := os.Getenv("INITSTEP"); v != "" {
		fmt.Sscanf(v, "%d", &initStep)
	}
	emit("STEP", "pre-init")
	if _, err := call(scriptInst, "luawasm_init", initStep); err != nil {
		return append(log, "ENGINE-ERROR\tinit: "+err.Error())
	}
	emit("STEP", "init done")
	gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
	frameCells := gv.I32()
	frame, err := call(rtInst, "rt_frame_alloc", int(frameCells)*16)
	if err != nil {
		return []string{"ENGINE-ERROR\tframe: " + err.Error()}
	}

	emit("STEP", fmt.Sprintf("pre-main frame=%v cells=%v", frame, frameCells))
	status, err := call(scriptInst, "lua_main", frame)
	emit("STEP", "post-main")
	if err != nil {
		return append(log, "ENGINE-ERROR\ttrap: "+err.Error())
	}
	stv, _ := status.(int32)
	if stv != 0 {
		// staged error: copy bytes out of a guest buffer
		if buf, err := call(rtInst, "rt_frame_alloc", 4096); err == nil {
			if n, err := call(rtInst, "rt_err_stage_copy", buf, 4096); err == nil {
				nv, _ := n.(int32)
				if nv > 0 {
					if raw := memRead(buf.(int32), nv); raw != nil {
						emit("ERROR", strconv.Quote(string(raw)))
					}
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

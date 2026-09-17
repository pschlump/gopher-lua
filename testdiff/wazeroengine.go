package testdiff

// The wasm-backend engine hosted on wazero (M6a): the pure-Go production
// engine (design A8). Hosts the SAME SJLJ/new-EH runtime blob as the
// wasmtime engine — wazero v1.12 implements the standardized
// exception-handling proposal behind
// experimental.CoreFeaturesExceptionHandling, and the blob is built in that
// dialect (-mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false). The
// M5 error-safety invariant (no longjmp/throw ever crosses a host or script
// boundary; compiled code returns -1 + staged error and the adapter
// re-raises after the dispatch returns) means EH unwinding stays inside the
// blob's own frames — the host never has to propagate an exception across
// its own call sites.
//
// The protocol mirrors wasmengine.go (the wasmtime host): shared memory
// owned by the blob, host.event/random*/wasm_dispatch imports, the M2
// driver exports (lnewstate/ldostring/lglobals...), gc-stop + arg-setup
// ldostrings, rt_set_dialect(1), luawasm_init, rt_frame_alloc, lua_main,
// staged-error readback, io.flush before reading the stdout capture. stdout
// is a bytes.Buffer (wazero WASI WithStdout) instead of a temp file.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func u32[T int32 | int | uint32](v T) uint64 { return uint64(uint32(v)) }

// NewRunEngine returns the wasm-backend engine for the standalone runners
// (glua -W, luawasm-run): the wasmtime differential oracle by default, or
// the wazero production engine (ledger row 32 — one blob, two hosts) when
// GLUA_WASM_ENGINE=wazero. The _cli-tests WAZERO=1 leg selects it so the
// black-box suite runs its wasm legs on the production engine unchanged.
func NewRunEngine(name string, precompiled []byte) Engine {
	if strings.EqualFold(os.Getenv("GLUA_WASM_ENGINE"), "wazero") {
		e := NewWazeroEngine(name)
		e.Precompiled = precompiled
		return e
	}
	e := NewWasmEngine(name)
	e.Precompiled = precompiled
	return e
}

// WazeroEngine compiles and executes scripts through the wasm backend on
// wazero (the production engine). Construction mirrors WasmEngine.
type WazeroEngine struct {
	name string
	// NoopHosts: replace host shims with no-ops (trap triage)
	NoopHosts bool
	// Precompiled module bytes (nil → compile from source)
	Precompiled []byte
	// SkipUnsupported: scripts using v1-unsupported opcodes produce a
	// SKIP-UNSUPPORTED log instead of an engine error (corpus tests)
	SkipUnsupported bool
}

// NewWazeroEngine returns a wazero-hosted wasm-backend engine.
func NewWazeroEngine(name string) *WazeroEngine { return &WazeroEngine{name: name} }

func (e *WazeroEngine) Name() string { return e.name }

func (e *WazeroEngine) Run(c Case) (log []string) {
	defer func() {
		if p := recover(); p != nil {
			log = []string{fmt.Sprintf("ENGINE-PANIC\t%v\t%s", p, debug.Stack())}
		}
	}()
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
			return []string{"ERROR\t" + strconv.Quote(err.Error())}
		}
	}

	ctx := context.Background()
	rc := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	r := wazero.NewRuntimeWithConfig(ctx, rc)
	defer r.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	var stdout bytes.Buffer // wasi fd_write capture (replaces the temp file)
	decoder := &CLua{}

	// host.wasm_dispatch: the M5a re-entrancy seam — the blob's precall
	// adapter dispatches compiled protos through the host into the script
	// module's lua_dispatch (same goroutine, nested call). A trap inside
	// the dispatch must not unwind through wasm: surface as a refused
	// dispatch (-3); the adapter re-raises it as a Lua error.
	var scriptInst api.Module
	dispatch := func(idx, frame, cl, nargs, want int32) int32 {
		if scriptInst == nil {
			return -3
		}
		fn := scriptInst.ExportedFunction("lua_dispatch")
		if fn == nil {
			return -3
		}
		res, err := fn.Call(ctx, u32(idx), u32(frame), u32(cl), u32(nargs), u32(want))
		if err != nil {
			return -3
		}
		return int32(res[0])
	}

	rng := rand.New(rand.NewSource(42))
	eventFn := func(kind, ptr, length int32) {
		if m := r.Module("rt"); m != nil {
			if raw, ok := m.Memory().Read(uint32(ptr), uint32(length)); ok {
				decoder.onEvent(raw, kind, emit)
			}
		}
	}
	if e.NoopHosts {
		eventFn = func(kind, ptr, length int32) {}
	}
	_, err := r.NewHostModuleBuilder("host").
		NewFunctionBuilder().WithFunc(eventFn).Export("event").
		NewFunctionBuilder().WithFunc(func() float64 { return rng.Float64() }).Export("random01").
		NewFunctionBuilder().WithFunc(func(lo, hi int32) int32 {
			return int32(rng.Intn(int(hi-lo+1)) + int(lo))
		}).Export("randomint").
		NewFunctionBuilder().WithFunc(func(seed int64) { rng = rand.New(rand.NewSource(seed)) }).Export("randomseed").
		NewFunctionBuilder().WithFunc(dispatch).Export("wasm_dispatch").
		Instantiate(ctx)
	if err != nil {
		return []string{"ENGINE-ERROR\thost module: " + err.Error()}
	}

	rtBin := lua51SjljWasm
	if p := os.Getenv("RTWASM"); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			rtBin = b
		}
	}
	rtInst, err := r.InstantiateWithConfig(ctx, rtBin,
		wazero.NewModuleConfig().WithName("rt").
			WithStdout(&stdout).WithStderr(&stdout).
			WithFSConfig(wazero.NewFSConfig().WithDirMount(c.Dir, "/")))
	if err != nil {
		return []string{"ENGINE-ERROR\trt instantiate: " + err.Error()}
	}
	mem := rtInst.Memory()

	scriptInst, err = r.InstantiateWithConfig(ctx, bin, wazero.NewModuleConfig().WithName("script"))
	if err != nil {
		return []string{"ENGINE-ERROR\tscript instantiate: " + err.Error()}
	}

	call := func(mod api.Module, fn string, args ...uint64) ([]uint64, error) {
		f := mod.ExportedFunction(fn)
		if f == nil {
			return nil, fmt.Errorf("missing export %s", fn)
		}
		return f.Call(ctx, args...)
	}

	L, err := call(rtInst, "lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\tlnewstate: " + err.Error()}
	}
	Lv := int32(L[0])
	ldostring := func(src, chunkname string, nres uint32) error {
		in, err := call(rtInst, "linbuf")
		if err != nil {
			return err
		}
		nameA, err := call(rtInst, "lnamebuf")
		if err != nil {
			return err
		}
		inAddr, nameAddr := uint32(in[0]), uint32(nameA[0])
		if !mem.Write(inAddr, []byte(src)) || !mem.Write(nameAddr, []byte(chunkname)) {
			return fmt.Errorf("ldostring buffer write OOB")
		}
		_, err = call(rtInst, "ldostring", u32(Lv), u32(inAddr), u32(len(src)), u32(nameAddr), u32(nres))
		return err
	}
	// the ABI operates on this state (curL in rt_abi.c) — without it,
	// NULL-derefs land in mapped page 0 and surface as wild call_indirects
	if _, err := call(rtInst, "rt_set_state", u32(Lv)); err != nil {
		return []string{"ENGINE-ERROR\trt_set_state: " + err.Error()}
	}
	// v1 GC stop: register cells are not GC roots (the M3 ABI obligation);
	// the M6 cap+fresh-VM posture (ledger row 11) replaces arena lifecycle
	if err := ldostring("collectgarbage('stop')", c.Name, 1); err != nil {
		return []string{"ENGINE-ERROR\tgc stop: " + err.Error()}
	}
	// the CLI arg surface (after gcStop, which resets arg = {[0]=name})
	argSetup := "arg={[0]=" + luaQuote(strings.TrimPrefix(c.Name, "@"))
	for _, a := range c.Args {
		argSetup += "," + luaQuote(a)
	}
	argSetup += "}"
	if err := ldostring(argSetup, "=arg", 0); err != nil {
		return []string{"ENGINE-ERROR\targ setup: " + err.Error()}
	}
	// M5d: the gopher message dialect — byte-exact vs the interp oracle
	if _, err := call(rtInst, "rt_set_dialect", 1); err != nil {
		return []string{"ENGINE-ERROR\tdialect: " + err.Error()}
	}
	av, err := call(rtInst, "rt_abi_version")
	if err != nil {
		return []string{"ENGINE-ERROR\tABI version: " + err.Error()}
	}
	if int32(av[0]) != 3 {
		return []string{fmt.Sprintf("ENGINE-ERROR\tABI version %d (need 3)", int32(av[0]))}
	}
	if _, err := call(scriptInst, "luawasm_init", 2); err != nil {
		return append(log, "ENGINE-ERROR\tinit: "+err.Error())
	}
	frameCells := int32(scriptInst.ExportedGlobal("gFrameCells").Get())
	frame, err := call(rtInst, "rt_frame_alloc", u32(frameCells*16))
	if err != nil {
		return []string{"ENGINE-ERROR\tframe: " + err.Error()}
	}

	status, err := call(scriptInst, "lua_main", frame[0])
	if err != nil {
		return append(log, "ENGINE-ERROR\ttrap: "+err.Error())
	}
	if int32(status[0]) != 0 {
		// staged error: message bytes, or — for non-string error values —
		// the exact TValue rendered (matches the wasmtime engine's path)
		emitted := false
		if buf, err := call(rtInst, "rt_frame_alloc", 4096); err == nil {
			if n, err := call(rtInst, "rt_err_stage_copy", buf[0], 4096); err == nil && int32(n[0]) > 0 {
				if raw, ok := mem.Read(uint32(buf[0]), uint32(int32(n[0]))); ok {
					emit("ERROR", strconv.Quote(string(raw)))
					emitted = true
				}
			}
		}
		if !emitted {
			if vp, err := call(rtInst, "rt_err_value_ptr"); err == nil {
				if raw, ok := mem.Read(uint32(vp[0]), 16); ok {
					emit("ERROR", strconv.Quote(renderErrValue(raw)))
				}
			}
		}
	}
	if _, err := call(rtInst, "lglobals"); err != nil {
		emit("GLOBALS", "<lglobals failed: "+err.Error()+">")
	}
	// Ledger row 29: best-effort io.flush before reading the capture (the
	// driver returns without libc exit(), so the atexit flush never runs)
	_ = ldostring("io.flush()", "=flush", 0)

	if out := stdout.Bytes(); len(out) > 0 {
		for _, line := range bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n")) {
			emit("STDOUT", string(line))
		}
	}
	return log
}

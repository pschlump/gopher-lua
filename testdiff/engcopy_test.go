package testdiff

// engineRunCopy: a verbatim copy of WasmEngine.Run for trap bisection.
// Deltas are switchable via the copyOpts struct.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
)

type copyOpts struct {
	noopHosts  bool // probe-identical host shims
	noGuestMem bool // skip the guestMem extern read
	noAbiCheck bool // skip rt_abi_version
}

func engineRunCopy(e *WasmEngine, o copyOpts, c Case) []string {
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
	if o.noopHosts {
		if err := linker.DefineFunc(store, "host", "event", func(kind, ptr, length int32) {}); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
		if err := linker.DefineFunc(store, "host", "random01", func() float64 { return 0 }); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
		if err := linker.DefineFunc(store, "host", "randomint", func(a, b int32) int32 { return 0 }); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
		if err := linker.DefineFunc(store, "host", "randomseed", func(int64) {}); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
	} else {
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
	if !o.noGuestMem {
		guestMem = rtInst.GetExport(store, "memory").Memory()
	}

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
	if _, err := call(rtInst, "lnewstate"); err != nil {
		return []string{"ENGINE-ERROR\tlnewstate: " + err.Error()}
	}
	emit("STEP", "lnewstate done")
	var av interface{} = int32(2)
	if !o.noAbiCheck {
		var abiErr error
		av, abiErr = call(rtInst, "rt_abi_version")
		if abiErr != nil {
			return []string{"ENGINE-ERROR\tABI version: " + abiErr.Error()}
		}
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

func TestEngineCopyOnProbeEnv(t *testing.T) {
	store, rtInst, scriptInst, call := trapEnvNamedBin(t, true, true, "b.lua", nil)
	_ = store
	L, err := call(rtInst, "lnewstate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(rtInst, "rt_set_state", L); err != nil {
		t.Fatal(err)
	}
	if _, err := call(scriptInst, "luawasm_init", int32(2)); err != nil {
		t.Fatal(err)
	}
	gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
	frame, err := call(rtInst, "rt_frame_alloc", int(gv.I32())*16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(scriptInst, "lua_main", frame); err != nil {
		t.Logf("lua_main on PROBE ENV: TRAP")
		return
	}
	t.Log("lua_main on PROBE ENV: ok")
}

func TestEngineCopyTrap(t *testing.T) {
	for _, o := range []copyOpts{
		{},
		{noopHosts: true},
		{noGuestMem: true},
		{noAbiCheck: true},
		{noopHosts: true, noGuestMem: true, noAbiCheck: true},
	} {
		e := &WasmEngine{name: "copy"}
		log := engineRunCopy(e, o, Case{Name: "b.lua", Dir: ".", Source: []byte("local y = x\n")})
		trapped := false
		for _, l := range log {
			if strings.HasPrefix(l, "ENGINE-ERROR\ttrap") {
				trapped = true
			}
		}
		t.Logf("opts=%+v trapped=%v", o, trapped)
	}
	return
	e := &WasmEngine{name: "copy"}
	_ = e
	log := engineRunCopy(e, copyOpts{}, Case{Name: "b.lua", Dir: ".", Source: []byte("local y = x\n")})
	for _, l := range log {
		if len(l) > 90 {
			l = l[:90]
		}
		t.Log(l)
	}
}

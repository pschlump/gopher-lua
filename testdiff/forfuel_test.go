package testdiff

// Fuel-instrumented for-loop probe: if lua_main spins, the store runs out of
// fuel and traps instead of hanging, and the consumed amount tells us how
// far it got.

import (
	"os"
	"testing"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
)

func TestForFuel(t *testing.T) {
	src := "for i = 1, 3 do end\n"
	bin, err := CompileSource([]byte(src), "f.lua")
	if err != nil {
		t.Fatal(err)
	}

	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true)
	cfg.SetConsumeFuel(true)
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)
	store.SetFuel(1000000000)
	linker := wt.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "event", func(kind, ptr, ln int32) {}); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "random01", func() float64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "randomint", func(a, b int32) int32 { return 0 }); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "randomseed", func(int64) {}); err != nil {
		t.Fatal(err)
	}
	store.SetWasi(wt.NewWasiConfig())

	rtMod, err := wt.NewModule(engine, lua51SjljWasm)
	if err != nil {
		t.Fatal(err)
	}
	rtInst, err := linker.Instantiate(store, rtMod)
	if err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineInstance(store, "rt", rtInst); err != nil {
		t.Fatal(err)
	}
	sm, err := wt.NewModule(engine, bin)
	if err != nil {
		t.Fatal(err)
	}
	scriptInst, err := linker.Instantiate(store, sm)
	if err != nil {
		t.Fatal(err)
	}
	call := func(inst *wt.Instance, fn string, args ...interface{}) (interface{}, error) {
		f := inst.GetFunc(store, fn)
		if f == nil {
			return nil, errMissing(fn)
		}
		for i, a := range args {
			switch v := a.(type) {
			case uint64:
				args[i] = int32(v)
			case int:
				args[i] = int32(v)
			case int64:
				args[i] = int32(v)
			}
		}
		return f.Call(store, args...)
	}

	L, _ := call(rtInst, "lnewstate")
	call(rtInst, "rt_set_state", L)
	call(scriptInst, "luawasm_init", int32(2))
	gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
	frame, _ := call(rtInst, "rt_frame_alloc", int(gv.I32())*16)

	const fuel = 4000000
	for _, attempt := range []struct {
		name string
		fuel uint64
	}{{"main", fuel}} {
		store.SetFuel(attempt.fuel)
		st, err := call(scriptInst, "lua_main", frame)
		left, _ := store.GetFuel()
		t.Logf("%s: status=%v err=%v fuel-used=%d", attempt.name, st, err, attempt.fuel-left)
		if err != nil {
			t.Logf("err detail: %v", err)
		}
	}

	// dump frame cells AND kcells after the trap, plus their addresses
	mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	fv := uint32(mustUint(frame))
	kb := uint32(scriptInst.GetExport(store, "gKCells").Global().Get(store).I32())
	t.Logf("frame=0x%x kcells=0x%x delta=%d", fv, kb, int64(fv)-int64(kb))
	for i := 0; i < 4; i++ {
		c := mem[fv+uint32(16*i) : fv+uint32(16*i)+16]
		t.Logf("R[%d]: tag=%d payload=%x", i, c[8], c[:8])
	}
	for i := 0; i < 4; i++ {
		c := mem[kb+uint32(16*i) : kb+uint32(16*i)+16]
		t.Logf("kcell[%d]: tag=%d payload=%x", i, c[8], c[:8])
	}
	store.SetFuel(1000000)
	if v, err := call(rtInst, "rt_fpcalls"); true {
		if err != nil {
			t.Logf("rt_fpcalls read err: %v", err)
		}
		t.Logf("forprep calls=%v in: %v %v %v", v,
			callFloat(rtInst, call, "rt_fpin", 0), callFloat(rtInst, call, "rt_fpin", 1), callFloat(rtInst, call, "rt_fpin", 2))
	}
	_ = os.Getenv
}

func callFloat(inst *wt.Instance, call func(*wt.Instance, string, ...interface{}) (interface{}, error), name string, i int) float64 {
	v, err := call(inst, name, i)
	if err != nil {
		return -888
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return -889
}

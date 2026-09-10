package testdiff

import (
	"strings"
	"testing"
)

func TestZZ2(t *testing.T) {
	bin, err := CompileSource([]byte("zz = 5\n"), "z.lua")
	if err != nil {
		t.Fatal(err)
	}
	store, rtInst, scriptInst, call := trapEnvNamedBin(t, true, true, "z.lua", bin)
	_ = store
	// capture host events (GLOBALS comes through here)
	var events []string
	// trapEnvNamedBin's host.event is a noop; use lglobals via the
	// engine-style reader instead: call lglobals then read _G through
	// a getglobal probe
	L, _ := call(rtInst, "lnewstate")
	if _, err := call(rtInst, "rt_set_state", L); err != nil {
		t.Fatal(err)
	}
	if _, err := call(scriptInst, "luawasm_init", int32(2)); err != nil {
		t.Fatal(err)
	}
	// dump the constants cells for THIS module
	gkv := scriptInst.GetExport(store, "gKCells").Global().Get(store)
	kb := uint32(gkv.I32())
	kdata := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	for i := 0; i < 4; i++ {
		c := kdata[kb+uint32(16*i) : kb+uint32(16*i)+16]
		t.Logf("kcell[%d]: tag=%d payload=%x", i, c[8], c[:8])
	}
	gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
	frame, _ := call(rtInst, "rt_frame_alloc", int(gv.I32())*16)
	st, err := call(scriptInst, "lua_main", frame)
	t.Logf("lua_main status=%v err=%v", st, err)
	if n, err := call(rtInst, "rt_sgcalls"); err == nil {
		t.Logf("setglobal calls during run: %v", n)
	}
	readStr := func(export string) string {
		v, err := call(rtInst, export)
		if err != nil {
			return "<err>"
		}
		addr := mustUint(v)
		mem2 := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
		out := ""
		for i := uint32(0); i < 64; i++ {
			b := mem2[uint32(addr)+i]
			if b == 0 {
				break
			}
			out += string(b)
		}
		return out
	}
	t.Logf("sgname=[%s] sgvaltag=%v", readStr("rt_sgname"), func() int {
		v, _ := call(rtInst, "rt_sgvaltag")
		return int(mustUint(v))
	}())
	t.Logf("ggcalls=%v ggname=[%s] ggresult=%v", func() int {
		v, _ := call(rtInst, "rt_ggcalls")
		return int(mustUint(v))
	}(), readStr("rt_ggname"), func() int {
		v, _ := call(rtInst, "rt_ggresult")
		return int(mustUint(v))
	}())

	// host-side probe: getglobal("zz") into a fresh cell
	buf, _ := call(rtInst, "rt_frame_alloc", 128)
	mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	bu := uint32(mustUint(buf))
	copy(mem[bu+64:], "zz")
	if _, err := call(rtInst, "rt_intern", mustUint(buf), mustUint(buf)+64, 2); err != nil {
		t.Fatal(err)
	}
	// host-side rt_setglobal first: zz2 = 7
	copy(mem[bu+72:], "zz2")
	call(rtInst, "rt_intern", mustUint(buf)+32, mustUint(buf)+72, 3)
	call(rtInst, "rt_mknumber", mustUint(buf)+48, float64(7))
	sg, err := call(rtInst, "rt_setglobal", mustUint(buf)+32, mustUint(buf)+48, 9)
	t.Logf("host setglobal status=%v", sg)
	gs, err := call(rtInst, "rt_getglobal", mustUint(buf)+16, mustUint(buf), 9)
	if err != nil {
		t.Fatalf("getglobal probe: %v", err)
	}
	t.Logf("probe getglobal(zz) status=%v tag=%d", gs, mem[bu+16+8])
	// now read back zz2
	call(rtInst, "rt_getglobal", mustUint(buf)+32, mustUint(buf)+32-32, 9) // key reused; dst overlay
	copy(mem[bu+64:], "zz2")
	call(rtInst, "rt_intern", mustUint(buf), mustUint(buf)+64, 3)
	g2, err2 := call(rtInst, "rt_getglobal", mustUint(buf)+48, mustUint(buf), 9)
	t.Logf("probe getglobal(zz2) status=%v tag=%d payload=%x err=%v", g2, mem[bu+48+8], mem[bu+48:bu+56], err2)
	_ = events
	_ = strings.TrimSpace
}

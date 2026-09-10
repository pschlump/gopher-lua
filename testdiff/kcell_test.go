package testdiff

import (
	"testing"
)

func TestKCells(t *testing.T) {
	src := "local x = 40\nprint(x)\n"
	bin, err := CompileSource([]byte(src), "k.lua")
	if err != nil {
		t.Fatal(err)
	}
	store, rtInst, scriptInst, call := trapEnvNamedBin(t, true, true, "k.lua", bin)
	_ = store
	buf0, _ := call(rtInst, "rt_frame_alloc", 256)
	L, err := call(rtInst, "lnewstate")
	if err != nil {
		t.Fatal(err)
	}
	call(rtInst, "rt_set_state", L)
	if _, err := call(scriptInst, "luawasm_init", int32(2)); err != nil {
		t.Fatal(err)
	}
	// read the gKCells global and dump the first cells
	gex := scriptInst.GetExport(store, "gKCells")
	if gex == nil {
		t.Fatal("gKCells export missing")
	}
	gv := gex.Global().Get(store)
	kbase := uint32(gv.I32())
	mem := rtInst.GetExport(store, "memory").Memory()
	data := mem.UnsafeData(store)
	t.Logf("kcells at %#x", kbase)
	// probe: rt_getglobal with cell[2] ("print" for this script) as key
	if _, err := call(rtInst, "rt_getglobal", mustUint(buf0)+16, uint64(kbase)+32, 9); err != nil {
		t.Logf("getglobal trap: %v", err)
	}
	kd := data[kbase+16 : kbase+32]
	t.Logf("getglobal dst: tag=%d", kd[8])
	for i := -4; i < 6; i++ {
		off := kbase + uint32(16*i)
		cell := data[off : off+16]
		t.Logf("cell[%+d] @%#x: tag=%d payload=%x", i, off, cell[8], cell[:8])
	}
}

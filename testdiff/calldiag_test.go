package testdiff

import (
	"testing"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
)

func TestCallDiag(t *testing.T) {
	bin, err := CompileSource([]byte("for i = 1, 3 do end\n"), "c.lua")
	if err != nil {
		t.Fatal(err)
	}
	store, rtInst, scriptInst, call := trapEnvNamedBin(t, true, true, "c.lua", bin)
	mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	L, _ := call(rtInst, "lnewstate")
	call(rtInst, "rt_set_state", L)
	call(scriptInst, "luawasm_init", int32(2))

	// kcells for this module (K[0]="print" K[1]="a" K[2]="b")
	kb := uint32(scriptInst.GetExport(store, "gKCells").Global().Get(store).I32())
	for i := 0; i < 8; i++ {
		c := mem[kb+uint32(16*i) : kb+uint32(16*i)+16]
		t.Logf("kcell[%d]: tag=%d payload=%x", i, c[8], c[:8])
	}

	fv := uint32(mustUint2(t, call, rtInst, "rt_frame_alloc", 8*16))

	call(rtInst, "rt_err_clear")

	st, err := call(scriptInst, "lua_main", int32(fv))
	t.Logf("lua_main status=%v err=%v | lastfn=%v laststatus=%v failfn=%v ccalls=%v gtcalls=%v ggcalls=%v pending=%v",
		st, err,
		mustUint2(t, call, rtInst, "rt_lastfn"),
		mustUint2(t, call, rtInst, "rt_laststatus"),
		mustUint2(t, call, rtInst, "rt_failfn"),
		mustUint2(t, call, rtInst, "rt_ccalls"),
		mustUint2(t, call, rtInst, "rt_gtcalls"),
		mustUint2(t, call, rtInst, "rt_ggcalls"),
		mustUint2(t, call, rtInst, "rt_err_pending"))
	if n := mustUint2(t, call, rtInst, "rt_err_stage_copy", int32(fv+128), 100); n > 0 {
		t.Logf("staged after lua_main: %q", string(mem[fv+128:fv+128+uint32(n)]))
	}
	for i := 0; i < 5; i++ {
		c := mem[fv+uint32(16*i) : fv+uint32(16*i)+16]
		t.Logf("R[%d]: tag=%d payload=%x", i, c[8], c[:8])
	}
}

func mustUint2(t *testing.T, call func(*wt.Instance, string, ...interface{}) (interface{}, error), inst *wt.Instance, name string, args ...interface{}) uint64 {
	t.Helper()
	v, err := call(inst, name, args...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	switch x := v.(type) {
	case uint64:
		return x
	case int32:
		return uint64(uint32(x))
	case uint32:
		return uint64(x)
	case int64:
		return uint64(x)
	case float64:
		return uint64(x)
	default:
		t.Fatalf("%s: unsupported type %T", name, v)
		return 0
	}
}

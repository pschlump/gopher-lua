package testdiff

import "testing"

// replicate the engine's rt-side sequence host-side, without the script
// module, to isolate where the fault comes from
func TestEngineProbeSeq(t *testing.T) {
	store, scriptInst, rtCall := rtSetup(t, nil)
	_ = store
	_ = scriptInst
	L, err := rtCall("lnewstate")
	if err != nil || L == 0 {
		t.Fatalf("lnewstate %v %v", L, err)
	}
	if _, err := rtCall("rt_set_state", L); err != nil {
		t.Fatal(err)
	}
	p1, err := rtCall("rt_frame_alloc", 11)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := rtCall("rt_frame_alloc", 48)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bufs: %#x %#x", p1, p2)
	// the second rt_frame_alloc'd buffer happens to sit after the first;
	// instead use the namebuf staging area which lnewstate already
	// filled with the chunk name machinery: write via rt_error's msgptr
	// path is unavailable — simplest host write is via inbuf
	nb, _ := rtCall("lnamebuf")
	st0, err0 := rtCall("rt_intern", p2, nb, 0)
	_ = st0
	_ = err0
	t.Logf("intern@namebuf: %v %v", st0, err0)
	st, err := rtCall("rt_intern", p2, p1, 2)
	if err != nil {
		t.Fatalf("intern trap: %v", err)
	}
	t.Logf("intern status=%d", st)
}

package host

import (
	"context"
	"testing"
)

// UsedBytes wraps the blob's rt_mem_used_bytes diagnostic: the same counter
// WithMemoryBudgetBytes enforces against. Guest GC is stopped (v1 law), so
// on a reused image the figure is monotone — it is the pooled-VM recycling
// watermark the daemon's per-script VM pool consumes (guide R3).
func TestUsedBytes(t *testing.T) {
	ctx := context.Background()
	e := mustEngine(t, WithMemoryBudgetBytes(32<<20))
	s := mustCompile(t, e, `
		local t = {}
		for i = 1, 2000 do t[i] = string.rep("x", 128) end
		return #t
	`)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()

	base := vm.UsedBytes(ctx)
	if base <= 0 {
		t.Fatalf("fresh VM reports %d used bytes — rt_mem_used_bytes missing?", base)
	}

	if _, err := vm.Run(ctx, s, RunOptions{}); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	after1 := vm.UsedBytes(ctx)
	if after1 <= base {
		t.Fatalf("after allocating run: used %d, want > %d", after1, base)
	}

	// GC is stopped: a second identical run must not reclaim — monotone.
	if _, err := vm.Run(ctx, s, RunOptions{}); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	after2 := vm.UsedBytes(ctx)
	if after2 <= after1 {
		t.Fatalf("monotonic reuse: run2 used %d, want > run1 %d", after2, after1)
	}
}

// A fresh VM's counter starts near zero (rt_set_memlimit zeroes it at
// creation), so a recycled VM visibly drops below the watermark it crossed.
func TestUsedBytesResetOnFreshVM(t *testing.T) {
	ctx := context.Background()
	e := mustEngine(t, WithMemoryBudgetBytes(32<<20))
	s := mustCompile(t, e, `local t = {} for i = 1, 2000 do t[i] = string.rep("x", 128) end return #t`)

	vm1, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm1.Run(ctx, s, RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	used1 := vm1.UsedBytes(ctx)
	_ = vm1.Close()
	if got := vm1.UsedBytes(ctx); got != 0 {
		t.Fatalf("closed VM reports %d, want 0", got)
	}

	vm2, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm2.Close()
	if used2 := vm2.UsedBytes(ctx); used2 >= used1 {
		t.Fatalf("fresh VM used %d, want < recycled VM's %d", used2, used1)
	}
}

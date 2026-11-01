package host

import (
	"testing"
)

// UsedBytes reports the per-VM accounted allocation counter — the same
// monotone gauge WithMemoryBudgetBytes enforces against (never
// refunded; membudget.go in the interpreter). The pooled-VM recycler
// consumes it as the watermark signal.
func TestUsedBytes(t *testing.T) {
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

	base := vm.UsedBytes()
	if base <= 0 {
		t.Fatalf("fresh VM reports %d used bytes — accounting missing?", base)
	}

	if _, err := vm.Run(t.Context(), s, RunOptions{}); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	after1 := vm.UsedBytes()
	if after1 <= base {
		t.Fatalf("after allocating run: used %d, want > %d", after1, base)
	}

	// the counter is never refunded: a second identical run must show
	// the monotone high-water figure
	if _, err := vm.Run(t.Context(), s, RunOptions{}); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	after2 := vm.UsedBytes()
	if after2 <= after1 {
		t.Fatalf("monotone reuse: run2 used %d, want > run1 %d", after2, after1)
	}
}

// A fresh VM's counter starts at its build allocations, so a recycled
// VM visibly drops below the watermark it crossed; a closed VM reports 0.
func TestUsedBytesResetOnFreshVM(t *testing.T) {
	e := mustEngine(t, WithMemoryBudgetBytes(32<<20))
	s := mustCompile(t, e, `local t = {} for i = 1, 2000 do t[i] = string.rep("x", 128) end return #t`)

	vm1, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm1.Run(t.Context(), s, RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	used1 := vm1.UsedBytes()
	_ = vm1.Close()
	if got := vm1.UsedBytes(); got != 0 {
		t.Fatalf("closed VM reports %d, want 0", got)
	}

	vm2, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm2.Close()
	if used2 := vm2.UsedBytes(); used2 >= used1 {
		t.Fatalf("fresh VM used %d, want < recycled VM's %d", used2, used1)
	}
}

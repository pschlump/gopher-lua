package host

// VM-pool gates (WithVMCacheSize): pool reuse per script identity, LRU
// capacity eviction, runs-budget recycling, error-state reuse, and the
// concurrency contract. White-box assertions use e.vms (NewVM count)
// and e.pool directly — the run results themselves must stay identical
// to the fresh-VM posture.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// killTestDeadline bounds the pool's deadline-kill legs (short enough
// to keep the suite fast; the kill contract itself is gated in
// host_test.go).
const killTestDeadline = 100 * time.Millisecond

func runInt(t *testing.T, e *Engine, s *Script, opt RunOptions) int64 {
	t.Helper()
	res, err := e.Run(context.Background(), s, opt)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Values) != 1 || res.Values[0].Kind != KindInteger {
		t.Fatalf("unexpected result: %+v", res.Values)
	}
	return res.Values[0].Int
}

func TestVMCacheReusePerScript(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(2))
	s := mustCompile(t, e, "return #KEYS + #ARGV")

	if got := runInt(t, e, s, RunOptions{Keys: []string{"a"}, Argv: []string{"1"}}); got != 2 {
		t.Fatalf("run 1 = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d after first run", e.vms)
	}
	// same source, different arguments: pooled VM, fresh staging
	if got := runInt(t, e, s, RunOptions{Keys: []string{"a", "b"}, Argv: []string{"1", "2", "3"}}); got != 5 {
		t.Fatalf("run 2 = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d after pooled rerun (VM not reused)", e.vms)
	}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d", e.pool.Len())
	}
}

func TestVMCacheDistinctScriptsDistinctVMs(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(2))
	a := mustCompile(t, e, "return 11")
	b := mustCompile(t, e, "return 22")

	runInt(t, e, a, RunOptions{})
	runInt(t, e, b, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d (distinct sources must not share a VM)", e.vms)
	}
	runInt(t, e, a, RunOptions{})
	runInt(t, e, b, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d after pooled reruns", e.vms)
	}
}

func TestVMCacheLRUEviction(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(1))
	a := mustCompile(t, e, "return 1")
	b := mustCompile(t, e, "return 2")

	runInt(t, e, a, RunOptions{}) // pool: {a}
	runInt(t, e, b, RunOptions{}) // full: evict a, pool: {b}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d", e.pool.Len())
	}
	if e.vms != 2 {
		t.Fatalf("vms = %d", e.vms)
	}
	runInt(t, e, a, RunOptions{}) // miss: a was evicted → fresh VM
	if e.vms != 3 {
		t.Fatalf("vms = %d after re-running the evicted script", e.vms)
	}
}

func TestVMCacheScriptErrorKeepsVMPooled(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(2))
	s := mustCompile(t, e, "if KEYS[1] == 'boom' then error('script failure') end return 7")

	if _, err := e.Run(context.Background(), s, RunOptions{Keys: []string{"boom"}}); err == nil {
		t.Fatal("expected script error")
	} else if _, ok := err.(*ScriptError); !ok {
		t.Fatalf("error class = %T", err)
	}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d after guest-level error (VM should stay pooled)", e.pool.Len())
	}
	// the unwound VM serves the next run correctly
	if got := runInt(t, e, s, RunOptions{Keys: []string{"ok"}}); got != 7 {
		t.Fatalf("rerun = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d (erroring VM was not reused)", e.vms)
	}
}

// A deadline kill is a guest-level error: the killed VM stays pooled
// and reusable (the run context dies with the run, the stack unwound).
func TestVMCacheDeadlineKilledVMReusable(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(2))
	dead := mustCompile(t, e, "while true do end")
	if _, err := e.Run(context.Background(), dead, RunOptions{Deadline: killTestDeadline}); err == nil {
		t.Fatal("deadline did not fire")
	}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d after deadline kill", e.pool.Len())
	}
	if got := runInt(t, e, mustCompile(t, e, "return 7"), RunOptions{}); got != 7 {
		t.Fatalf("post-kill run = %d", got)
	}
}

// The recycling signal natively is runs served (glasm's frozen-GC
// watermark has no analog — the Go collector is live): a VM past the
// budget closes instead of pooling.
func TestVMCacheRunsRecycle(t *testing.T) {
	old := vmRecycleRuns
	vmRecycleRuns = 2
	defer func() { vmRecycleRuns = old }()

	e := mustEngine(t, WithVMCacheSize(2))
	s := mustCompile(t, e, "return 3")
	runInt(t, e, s, RunOptions{}) // run 1 → pooled (1/2)
	runInt(t, e, s, RunOptions{}) // run 2 → pooled (2/2)
	runInt(t, e, s, RunOptions{}) // fresh VM (past budget)
	if e.vms != 2 {
		t.Fatalf("vms = %d (runs budget did not recycle the VM)", e.vms)
	}
}

func TestVMCacheConcurrency(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(2))
	a := mustCompile(t, e, "local s=0 for i=1,2000 do s=s+i end return s")
	b := mustCompile(t, e, "local s='' for i=1,50 do s=s..i end return #s")

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for round := 0; round < 4; round++ {
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if runInt(t, e, a, RunOptions{}) != 2001000 {
					errs <- errBad("a")
				}
			}()
			go func() {
				defer wg.Done()
				if runInt(t, e, b, RunOptions{}) != 91 {
					errs <- errBad("b")
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if e.pool.Len() > 2 {
		t.Fatalf("pool len = %d over capacity", e.pool.Len())
	}
}

// errBad avoids result literals inside goroutines above.
func errBad(which string) error {
	return &testErr{"wrong result for script " + which}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func TestVMCacheCloseDrains(t *testing.T) {
	e := mustEngine(t, WithVMCacheSize(2))
	a := mustCompile(t, e, "return 1")
	runInt(t, e, a, RunOptions{})
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d before Close", e.pool.Len())
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e.pool.Len() != 0 {
		t.Fatalf("pool len = %d after Close (engine-owned VMs must drain)", e.pool.Len())
	}
	if _, err := e.Run(context.Background(), a, RunOptions{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Run after Close = %v", err)
	}
}

func TestVMCacheDisabledDefault(t *testing.T) {
	// the default posture is unchanged: no pool, every run a fresh VM
	e := mustEngine(t)
	if e.pool != nil {
		t.Fatal("default engine has a pool")
	}
	s := mustCompile(t, e, "return 1")
	runInt(t, e, s, RunOptions{})
	runInt(t, e, s, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d (default must not pool)", e.vms)
	}
}

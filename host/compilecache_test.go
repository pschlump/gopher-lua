package host

// Compile-cache gates: proto cache identity (one *Script pointer per
// (SHA-1, name)), the bounded posture, and concurrent single-mint.
// (glasm's on-disk tier is wasm mechanism — the memory tier is the
// whole cache natively; recompiling a script is µs–ms.)

import (
	"context"
	"sync"
	"testing"
)

func TestCompileCacheSamePointer(t *testing.T) {
	e := mustEngine(t)
	src := []byte("local t = {} for i = 1, 100 do t[i] = i * 2 end return #t")
	s1, err := e.Compile(src, "=c")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := e.Compile(src, "=c")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("same (source, name) minted two *Script pointers")
	}
	if s1.Name() != "=c" || len(s1.SHA1) != 40 {
		t.Fatalf("script identity: %q %q", s1.Name(), s1.SHA1)
	}
	if res, err := e.Run(context.Background(), s1, RunOptions{}); err != nil || res.Values[0].Int != 100 {
		t.Fatalf("run: %+v %v", res, err)
	}
}

// The cache key includes the chunk name: same source under two names
// is two scripts (different error-position identities).
func TestCompileCacheKeyIncludesName(t *testing.T) {
	e := mustEngine(t)
	src := []byte("return 1")
	a, err := e.Compile(src, "=a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Compile(src, "=b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a.SHA1 != b.SHA1 || a.identity() == b.identity() {
		t.Fatalf("name not keyed: %p %p", a, b)
	}
	// empty name defaults to "=script"
	c, err := e.Compile(src, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name() != "=script" {
		t.Fatalf("default name: %q", c.Name())
	}
}

func TestCompileMemCacheBounded(t *testing.T) {
	e := mustEngine(t, WithCompileMemCache(1))
	a, err := e.Compile([]byte("return 1"), "=a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Compile([]byte("return 2"), "=b"); err != nil { // evicts a
		t.Fatal(err)
	}
	a2, err := e.Compile([]byte("return 1"), "=a")
	if err != nil {
		t.Fatal(err)
	}
	if a2 == a {
		t.Fatal("evicted entry served the stale pointer")
	}
	if a2.SHA1 != a.SHA1 || a2.identity() != a.identity() {
		t.Fatal("re-minted script lost its identity")
	}
	if res, err := e.Run(context.Background(), a2, RunOptions{}); err != nil || res.Values[0].Int != 1 {
		t.Fatalf("re-minted run: %+v %v", res, err)
	}
}

// Concurrent Compile of one source mints ONE pointer (the identity law
// under the unbounded map's race guard).
func TestCompileCacheConcurrentSingleMint(t *testing.T) {
	e := mustEngine(t)
	src := []byte("return 'x'")
	var wg sync.WaitGroup
	scripts := make([]*Script, 16)
	for i := range scripts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := e.Compile(src, "=race")
			if err != nil {
				t.Error(err)
				return
			}
			scripts[i] = s
		}(i)
	}
	wg.Wait()
	for i, s := range scripts {
		if s == nil || s != scripts[0] {
			t.Fatalf("goroutine %d minted a different pointer", i)
		}
	}
}

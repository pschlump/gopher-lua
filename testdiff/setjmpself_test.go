package testdiff

// Gate for the Asyncify setjmp/longjmp machinery (M2 prerequisite): the
// runtime/selftest.wasm module must pass on wazero — basic longjmp,
// longjmp over an inner protected frame, repeated setjmp at the same
// stack address, and value preservation across the rewind. Skips when the
// .wasm is not built (runtime/build.sh; requires wasi-sdk + binaryen).

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pschlump/wazero"
	"github.com/pschlump/wazero/imports/wasi_snapshot_preview1"
)

func TestSetjmpSelfTest(t *testing.T) {
	bin, err := os.ReadFile("../runtime/selftest.wasm")
	if err != nil {
		t.Skip("selftest.wasm not built — run runtime/build.sh (needs wasi-sdk + binaryen)")
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	var stdout bytes.Buffer
	mod, err := r.InstantiateWithConfig(ctx, bin,
		wazero.NewModuleConfig().WithStdout(&stdout).WithName("selftest"))
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	res, err := mod.ExportedFunction("selftest").Call(ctx)
	if err != nil {
		t.Fatalf("selftest trapped: %v\nstdout:\n%s", err, stdout.String())
	}
	if res[0] != 0 {
		t.Errorf("selftest reported %d failures\nstdout:\n%s", res[0], stdout.String())
	}
	// stdout inside asyncified regions is unreliable (libc buffering +
	// unwind/rewind replay); the return code is the authoritative signal.
	// Lua's print does not use WASI stdout at all — it goes through the
	// host_event import — so this only affects this C-level selftest.
	if stdout.Len() > 0 {
		t.Logf("stdout:\n%s", stdout.String())
	}
	_ = strings.Contains
}

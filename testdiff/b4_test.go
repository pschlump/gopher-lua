package testdiff

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestBisect4(t *testing.T) {
	bin, err := CompileSource([]byte("for i = 1, 3 do print('i', i) end\n"), "b.lua")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	_, err = r.CompileModule(ctx, bin)
	t.Log(err)
}

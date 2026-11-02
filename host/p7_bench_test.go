package host

// P7 decomposition rig (durable — the stage-isolation benches behind
// docs/Perf-P7-Embed-Seam.md). Decomposes the pool-hit per-run floor
// and the hostfn crossing into their stages so future host-side work
// has a standing measure: staging (KEYS/ARGV table build), FnFromProto
// (per-run closure), pool dispatch, result conversion, and the three
// whole-run shapes (e_arg: staging+script, one-call: +1 crossing,
// e_call: +200 crossings).

import (
	"context"
	"testing"

	lua "github.com/pschlump/gopher-lua"
)

var p7Keys = []string{"key:1", "key:2", "key:3", "key:4", "key:5", "key:6", "key:7", "key:8", "key:9", "key:10"}
var p7Argv = []string{"17", "4", "alpha beta", "0"}

const p7EArg = `return string.format("%d %d", #KEYS + #ARGV, tonumber(ARGV[1]) + tonumber(ARGV[2]))`

const p7ECall = `local total = 0
for i = 1, 200 do
  total = total + bench.call(KEYS[1] .. ":" .. i)
end
return string.format("%d", total)`

// one crossing per run: crossing cost ≈ this − e_arg-shaped baseline
const p7OneCall = `return bench.call(KEYS[1])`

func p7Engine(b *testing.B, src string) (*Engine, *Script) {
	e := NewEngine(WithVMCacheSize(4))
	if err := e.RegisterGlobal("bench", "call", func(vm *VM, args []Value) ([]Value, error) {
		return []Value{Int(int64(len(args[0].Str)))}, nil
	}); err != nil {
		b.Fatal(err)
	}
	s, err := e.Compile([]byte(src), "=bench")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := e.Run(context.Background(), s, RunOptions{Keys: p7Keys, Argv: p7Argv}); err != nil {
		b.Fatal(err)
	}
	return e, s
}

func p7Run(b *testing.B, src string) {
	e, s := p7Engine(b, src)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Run(ctx, s, RunOptions{Keys: p7Keys, Argv: p7Argv}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkP7PoolHitEArg(b *testing.B)    { p7Run(b, p7EArg) }
func BenchmarkP7PoolHitOneCall(b *testing.B) { p7Run(b, p7OneCall) }
func BenchmarkP7PoolHitECall(b *testing.B)   { p7Run(b, p7ECall) }

// --- stage isolation (white-box) ---

// staging: the KEYS/ARGV RawSet pair exactly as vm.Run does it.
func BenchmarkP7Staging(b *testing.B) {
	e, _ := p7Engine(b, p7EArg)
	vm, err := e.NewVM()
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	L := vm.L
	globals, _ := L.GetGlobal("_G").(*lua.LTable)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		L.RawSet(globals, lua.LString("KEYS"), lua.LValue(vm.stringTable(L, p7Keys)))
		L.RawSet(globals, lua.LString("ARGV"), lua.LValue(vm.stringTable(L, p7Argv)))
	}
}

// proto-to-closure: NewFunctionFromProto per run.
func BenchmarkP7FnFromProto(b *testing.B) {
	e, s := p7Engine(b, p7EArg)
	vm, err := e.NewVM()
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	L := vm.L
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fn := L.NewFunctionFromProto(s.Proto)
		L.SetTop(0)
		L.Push(fn)
	}
}

// pool take/put only (no run).
func BenchmarkP7PoolTakePut(b *testing.B) {
	e, s := p7Engine(b, p7EArg)
	vm, err := e.NewVM()
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	key := s.identity()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, ok := e.pool.take(key)
		if !ok {
			b.Fatal("pool miss")
		}
		victim, _ := e.pool.put(key, got)
		_ = victim
	}
}

// result conversion: fromLValue on the e_arg shape's two string returns.
func BenchmarkP7ResultConv(b *testing.B) {
	a := lua.LString("14 21")
	c := lua.LString("0")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fromLValue(a); err != nil {
			b.Fatal(err)
		}
		if _, err := fromLValue(c); err != nil {
			b.Fatal(err)
		}
	}
}

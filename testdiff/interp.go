package testdiff

// The interpreter engine: the primary differential oracle (design doc §7).
// Each run gets a fresh LState, the deterministic shim, and a working
// directory equal to the corpus directory.

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/pschlump/gopher-lua"
)

// Interp is the gopher-lua interpreter oracle. Instances are named so two
// of them can be diffed against each other (the M1 self-diff gate).
type Interp struct {
	name string
}

// NewInterp returns an interpreter engine with the given display name.
func NewInterp(name string) *Interp { return &Interp{name: name} }

func (e *Interp) Name() string { return e.name }

func (e *Interp) Run(c Case) []string {
	var log []string
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	defer os.Chdir(wd)

	L := lua.NewState(lua.Options{
		RegistrySize:        1024 * 20,
		CallStackSize:       1024,
		IncludeGoStackTrace: true,
	})
	defer L.Close()
	installShim(L, emit)
	const tmpName = "testdiff.tmp"
	defer os.Remove(filepath.Join(c.Dir, tmpName))

	fn, err := L.Load(strings.NewReader(string(c.Source)), c.Name)
	if err != nil {
		emit("ERROR", ValueRepr(lua.LString(err.Error()), 0, map[*lua.LTable]bool{}))
		emit("GLOBALS", serializeGlobals(L))
		return log
	}
	L.Push(fn)
	if err := L.PCall(0, lua.MultRet, nil); err != nil {
		// The uncaught error value: PCall returns it as a Go error whose
		// message is the Lua error's string form (with position prefix).
		emit("ERROR", ValueRepr(lua.LString(err.Error()), 0, map[*lua.LTable]bool{}))
	}
	emit("GLOBALS", serializeGlobals(L))
	return log
}

func serializeGlobals(L *lua.LState) string {
	return serializeTable(L.GetGlobal("_G").(*lua.LTable), 0, map[*lua.LTable]bool{})
}

// installShim replaces every nondeterministic surface with a deterministic
// equivalent and hooks print into the event log. The shim set is part of
// the engine contract: every engine (interp today; C-Lua-wasm at M2 and
// the backend at M4) must provide the same replacements.
func installShim(L *lua.LState, emit func(event, payload string)) {
	// print → event log
	printFn := func(L *lua.LState) int {
		top := L.GetTop()
		parts := make([]string, 0, top)
		for i := 1; i <= top; i++ {
			parts = append(parts, PrintArg(L.Get(i)))
		}
		emit("PRINT", strings.Join(parts, "\t"))
		return 0
	}
	L.SetGlobal("print", L.NewFunction(printFn))

	// os.getenv/setenv: pure map, seeded the same everywhere
	env := map[string]string{"PATH": "/bin:/usr/bin"}
	getenv := func(L *lua.LState) int {
		v, ok := env[L.CheckString(1)]
		if !ok {
			L.Push(lua.LNil)
		} else {
			L.Push(lua.LString(v))
		}
		return 1
	}
	setenv := func(L *lua.LState) int {
		env[L.CheckString(1)] = L.CheckString(2)
		L.Push(lua.LTrue)
		return 1
	}
	L.SetField(L.GetGlobal("os"), "getenv", L.NewFunction(getenv))
	L.SetField(L.GetGlobal("os"), "setenv", L.NewFunction(setenv))

	// os.execute: constant exit status (satisfies os.lua's asserts without
	// shelling out — real semantics belong to the C oracle, not the diff)
	execStub := func(L *lua.LState) int {
		L.Push(lua.LNumber(1))
		return 1
	}
	L.SetField(L.GetGlobal("os"), "execute", L.NewFunction(execStub))

	// os.time/clock/date: constants
	constStub := func(v float64) func(*lua.LState) int {
		return func(L *lua.LState) int {
			L.Push(lua.LNumber(v))
			return 1
		}
	}
	L.SetField(L.GetGlobal("os"), "time", L.NewFunction(constStub(1234567890)))
	L.SetField(L.GetGlobal("os"), "clock", L.NewFunction(constStub(0)))
	L.SetField(L.GetGlobal("os"), "date", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString("2000-01-01 00:00:00"))
		return 1
	}))

	// math.random/randomseed: deterministic, engine-owned RNG.
	//
	// gopher-lua's mathlib draws from Go's global math/rand, whose
	// rand.Seed is a no-op on Go >= 1.24 (GODEBUG randseednop), so
	// math.randomseed(42) does not make sequences reproducible — and the
	// unseeded stream is process-random, unlike C Lua's deterministic
	// srand(1) start. The oracle therefore replaces both functions with a
	// per-run source. Every engine must provide this same shim (design
	// doc §7); see the divergence ledger.
	rng := rand.New(rand.NewSource(42))
	mathRandom := func(L *lua.LState) int {
		switch L.GetTop() {
		case 0:
			L.Push(lua.LNumber(rng.Float64()))
		case 1:
			L.Push(lua.LNumber(rng.Intn(L.CheckInt(1)) + 1))
		default:
			min := L.CheckInt(1)
			max := L.CheckInt(2)
			if min > max {
				L.ArgError(2, "interval is empty")
			}
			L.Push(lua.LNumber(rng.Intn(max-min+1) + min))
		}
		return 1
	}
	mathRandomseed := func(L *lua.LState) int {
		rng = rand.New(rand.NewSource(L.CheckInt64(1)))
		return 0
	}
	L.SetField(L.GetGlobal("math"), "random", L.NewFunction(mathRandom))
	L.SetField(L.GetGlobal("math"), "randomseed", L.NewFunction(mathRandomseed))

	// os.tmpname: fixed relative name (real tmpname embeds a process-unique
	// path); the engine removes the file after the run.
	const tmpName = "testdiff.tmp"
	L.SetField(L.GetGlobal("os"), "tmpname", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(tmpName))
		return 1
	}))
}

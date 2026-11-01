# gopher-lua (pschlump fork) — Lua 5.5.1 in pure Go

A Lua 5.5.1 virtual machine, compiler, and standard library in pure Go,
byte-exact with stock C Lua 5.5.1 (error messages, semantics, observable
behavior) — built as the embedded scripting engine for
[Ultima](../ultima), a pure-Go Redis clone (EVAL/SCRIPT).

Fork lineage: [yuin/gopher-lua] → **glasm** (the 5.2→5.5.1 upgrade + the
Lua→wasm backend) → **this repo**, carved from glasm at `0affa95`
(2026-11-01) as the standalone Go VM. Glasm lives on as the Lua→wasm
compiler; this is the native engine.

```go
L := lua.NewState()
defer L.Close()
if err := L.DoString(`print(("Lua"):upper() .. " " .. _VERSION)`); err != nil { ... }
```

Roadmap: the embed package (`host/`), the verification gates, and the
performance program are planned in [docs/Option-A-Plan.md](docs/Option-A-Plan.md).

## Build & test

```sh
make build   # regenerates state.go/vm.go (never edit those — see CLAUDE.md) + builds cmd/glua
make test    # regenerates + go test ./...
./cmd/glua/glua script.lua   # or with no args: REPL
```

License: MIT (see LICENSE; upstream copyright Yusuke Inuzuka / yuin).

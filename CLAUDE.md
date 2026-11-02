# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

**gopher-lua** (`github.com/pschlump/gopher-lua`, pschlump fork) — a **Lua 5.5.1 virtual machine and compiler in pure Go**, the embedded scripting engine for the Ultima project (a pure-Go Redis clone; EVAL/SCRIPT run on this engine). It was carved out of **glasm @ `0affa95`** on 2026-11-01 (the `v5.5` development branch was merged into `master` and deleted on 2026-11-02): glasm's root-package interpreter had been upgraded in place to stock 5.5.1 and gated byte-exact against stock C Lua 5.5.1 — that verification is this repo's inheritance. Glasm itself (`../glasm`) continues as the Lua→wasm compiler project and is the frozen source of this carve; no backflow.

The roadmap is `docs/Option-A-Plan.md` (tracks N = embed package + gates, P = performance program). Read it before planning work here.

## Critical: generated files

**`state.go` and `vm.go` are GENERATED** from `_state.go` / `_vm.go` by `_tools/go-inline` (it inlines functions marked `// +inline-start` … `// +inline-end` at `// +inline-call` sites). Never edit `state.go`/`vm.go` directly — edit the `_`-prefixed source and regenerate via `make build` (or `make test`).

## Commands

```sh
make build          # go-inline *.go && go fmt . && go build -o cmd/glua/glua ./cmd/glua
make test           # go-inline + go fmt + go test ./...
go test ./...       # everything (root package = the interpreter + libs)
go test -short ./...  # skips the official-suite golden leg (26s full / 6.4s short)
./cmd/glua/glua script.lua   # standalone interpreter (no args: REPL)
GOLDEN_UPDATE=1 go test . -run TestGolden   # regenerate goldens — deliberate; every diff line is a behavior delta
```

## Laws

- **Byte-exactness with stock C Lua 5.5.1 is the contract** — error messages, `luaV_*` semantics, observable table-iteration order, numeral formatting. The interpreter is the product; stock C Lua is the spec. Any divergence requires a row in the divergence ledger (glasm's ledger is `docs/glasm-history/Lua-Wasm-Divergence-Ledger.md`; new rows go in `docs/Divergence-Ledger.md` once created) and a covering test.
- **No behavior change without its gate** — the golden-corpus gate (`golden_test.go` over `_goldens/`, 58 cases across `_lua5.5-tests`/`_glua-tests`/`_u1`) is the safety net for all perf work. Goldens regenerate only deliberately (`GOLDEN_UPDATE=1`), and every changed line in `git diff _goldens/` is a behavior delta that cites its ledger row.
- Corpora are `_`-prefixed so the Go tooling ignores them: `_lua5.5-tests` (official suite), `_glua-tests`, `_u1`.

## Conventions

- Commit style follows the glasm/milestone lineage: short prefix naming the milestone/area (e.g. `N1: embed core`, `P1: register windows`).
- Perf changes merge only with the gates green and a benchstat A/B in the summary.

⏺ Done. The file was cmd/glua/glua.go (no main.go existed). Two new options, both reusing the existing testdiff machinery that luawasmc/luawasm-run use:

  -w file — compile to .wasm, write, and exit (no run)
  glua -w out.wasm script.lua     # or: glua -w out.wasm -e 'print(42)'
  Compiles via testdiff.CompileSource and writes the module — luawasmc inline. Source is the script, or the -e string. With neither, it errors (-w needs a script or -e stat, exit 2).

  -W — run a precompiled .wasm
  glua -W artifact.wasm           # forced
  glua artifact.wasm              # also auto-detected
  A .wasm-suffixed script is checked for the \x00asm module magic — if present it runs on the wasm engine (NewWasmEngine + Precompiled, PRINT/STDOUT→stdout, errors→stderr, exit 1), the way
  Lua itself loads binary chunks; if the magic doesn't match (a Lua source file named *.wasm), it falls through to the interpreter as before.

  Verified: dump→run round-trips (-e 'print("from -e", 6*7)' → wasm → from -e 42); a for/ipairs/concat script produces identical output on interpreter and both wasm-run paths; syntax errors
  exit 1; the interpreter path is unchanged. gofmt clean, go build ./... green. Rebuilt binary is at cmd/glua/glua. Changes are uncommitted.

  Pre-existing bug found while testing (not caused by this change — reproduces identically through your existing luawasmc + luawasm-run): recursion-depth failure is non-monotonic and
  silently value-corrupting near the limit —

  - fib(14) → 377 ✓, but fib(15) → "cannot perform add operation between function and number" (a function value shows up where a call result should be)
  - d(40) (one recursive call/level) → corrupted "between number and function", while d(200) → clean "stack overflow"

  That smells like ledger row 20 territory, but worse — silent corruption at fib-depth ~15, not just unclean error text. One suspicious datum: failure tracks total call count as much as
  depth (fib(15) ≈ 1973 calls vs fib(14) ≈ 1219), hinting an activation/frame counter isn't restored on return. I've logged it in the project memory as an M6 item.

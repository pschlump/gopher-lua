⏺ Done — every single-letter option now has a full-word spelling. Since Go's flag package has no alias mechanism, each long name is registered as a second flag bound to the same variable (Go already accepts -name and --name interchangeably):

  ┌─────────┬──────────────────┬──────────────────────┐
  │  Short  │       Long       │                      │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -e stat │ -execute stat    │ execute string       │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -l name │ -require name    │ require library      │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -mx MB  │ -memlimit MB     │ memory limit         │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -dt     │ -dumpast         │ dump AST trees       │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -dc     │ -dumpcode        │ dump VM codes        │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -i      │ -interactive     │ REPL after script    │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -p file │ -cpuprofile file │ CPU profile output   │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -w file │ -wasm file       │ compile to wasm      │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -W      │ -runwasm         │ run precompiled wasm │
  ├─────────┼──────────────────┼──────────────────────┤
  │ -v      │ -version         │ version banner       │
  └─────────┴──────────────────┴──────────────────────┘

  The usage text (-h) now shows both spellings per line. Verified working: -version / --version print the injected build info, -execute runs statements, -wasm + -runwasm round-trip a compile-and-run, -memlimit takes its value, all short forms behave
  as before, and go vet ./cmd/glua is clean.

  One naming judgment call to flag: -p became -cpuprofile (not -profile) since it writes CPU profiles specifically — happy to rename if you'd prefer something else. Not committed yet; say the word if you want it pushed.

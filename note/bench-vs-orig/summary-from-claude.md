⏺ Done. Everything is saved in note/bench-vs-orig/ (harness + scripts + raw runs + RESULTS.md; rerun with go build -o luabench . && ./luabench scripts/..., add GLUA_WASM_ENGINE=wazero for the
  production-engine leg). Methodology: both engines imported into one process, adaptive N with warmups and runtime.GC() before each timed iteration; every script's output was verified identical (outputs:
  OK) across orig / fork-interp / wasm before its numbers count. All numbers below are medians on this arm64 Mac.

  Short scripts

  ┌───────────────────────┬────────────┬────────────┬──────────┬──────────┬────────────┬────────────┬──────────────┬──────────────────────────────┐
  │                       │ orig parse │ fork parse │ orig run │ fork run │ orig TOTAL │ fork TOTAL │ wasm compile │ wasm run (wasmtime / wazero) │
  ├───────────────────────┼────────────┼────────────┼──────────┼──────────┼────────────┼────────────┼──────────────┼──────────────────────────────┤
  │ s0_nop (startup only) │ 3.6µs      │ 3.5µs      │ 26µs     │ 25µs     │ 27.5µs     │ 27.2µs     │ 11µs         │ 18.3ms / ~102ms              │
  ├───────────────────────┼────────────┼────────────┼──────────┼──────────┼────────────┼────────────┼──────────────┼──────────────────────────────┤
  │ s1_hello              │ 9.8µs      │ 9.8µs      │ 31µs     │ 31µs     │ 38.6µs     │ 38.7µs     │ 22µs         │ 18.3ms / ~100ms              │
  ├───────────────────────┼────────────┼────────────┼──────────┼──────────┼────────────┼────────────┼──────────────┼──────────────────────────────┤
  │ s2_string             │ 18µs       │ 17µs       │ 98µs     │ 99µs     │ 112µs      │ 113µs      │ 37µs         │ 18.9ms / ~118ms              │
  ├───────────────────────┼────────────┼────────────┼──────────┼──────────┼────────────┼────────────┼──────────────┼──────────────────────────────┤
  │ s3_table              │ 15µs       │ 15µs       │ 64µs     │ 64µs     │ 77µs       │ 78µs       │ 30µs         │ 18.8ms / ~107ms              │
  └───────────────────────┴────────────┴────────────┴──────────┴──────────┴────────────┴────────────┴──────────────┴──────────────────────────────┘

  Long scripts

  ┌─────────────────────────────┬─────────────┬──────────┬─────────────┬────────────┬────────────────────────┬─────────────────┐
  │                             │  orig run   │ fork run │ orig TOTAL  │ fork TOTAL │   wasm run wasmtime    │ wasm run wazero │
  ├─────────────────────────────┼─────────────┼──────────┼─────────────┼────────────┼────────────────────────┼─────────────────┤
  │ l1_fib (fib 30, call-heavy) │ 154-160ms   │ +5-10%   │ 155-161ms   │ +2-8%      │ 14.20s (88x)           │ 7.46s (48x)     │
  ├─────────────────────────────┼─────────────┼──────────┼─────────────┼────────────┼────────────────────────┼─────────────────┤
  │ l2_nsieve (2^18-2^20 sieve) │ 459-536ms   │ +3-6%    │ 459-475ms   │ +7-9%      │ 360ms (0.78x — faster) │ 9.10s (19x)     │
  ├─────────────────────────────┼─────────────┼──────────┼─────────────┼────────────┼────────────────────────┼─────────────────┤
  │ l3_stringwork (~150KB text) │ 16.5-17.9ms │ parity   │ 16.5-17.3ms │ 0.94-1.00x │ 67ms (4.1x)            │ 446ms (26x)     │
  └─────────────────────────────┴─────────────┴──────────┴─────────────┴────────────┴────────────────────────┴─────────────────┘

  (Ranges = two independent runs; wasm compile was 40-260µs everywhere — noise.)

  Findings

  - The fork did not regress the frontend or the interpreter in any way that matters: parse+compile is byte-for-byte parity, short-script interpreter runs are ≤±2%, and the compute-heavy long scripts show
    only a consistent ~4-6% interpreter slowdown (visible on min-of-N too, so likely real but small).
  - VM startup is the whole short-script story on wasm: ~18ms on wasmtime, ~100ms on wazero (fresh runtime + rt-blob instantiate per run), vs ~30-100µs total for the original interpreter — that's the
    470-1500x gap on tiny scripts.
  - Wasm execution splits by workload shape: straight-line loop/table code beats the interpreter (nsieve 0.78x — the C rt table ops outpace gopher-lua's Go tables); string work is 4x slower (every op is an
    rt_* call from wasm); and call-heavy recursion is 48-88x slower, because every Lua→Lua call round-trips through the host wasm_dispatch seam — fib(30) pays that ~1.6M times. Wazero halves wasmtime's fib
    time (pure-Go callback vs cgo crossing), but loses ~25x on nsieve (memory-heavy; wazevo is compiled in on darwin/arm64, so likely bounds-check + memory.grow-as-copy costs — attribution plausible, not
    proven; Linux production numbers may differ).
  - Incidental: table.concat of 3000 entries hits a stock gopher-lua registry overflow (config.go: RegistrySize = 256*20) in both orig and fork — the wasm/C engine handles it fine. Pre-existing upstream
    behavior; the bench script block-concats around it.

  For the Ultima/Redis use-case this says: short EVAL-style scripts are startup-bound (~100ms on the production engine — matching the ~44ms/cold-start shape already in your notes), compute-in-loops is
  competitive, and call-heavy Lua is the backend's current weak spot.

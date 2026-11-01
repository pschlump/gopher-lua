# gopher-lua `host/` vs native C Lua 5.5.1 — embedded usage (EVAL shape)

This is the N3b gate: the embedded-EVAL comparison suite, ported from
glasm's `cmd/embedcmp` (recorded run: `../glasm/docs/Embedded-EVAL-Perf.md`,
2026-11-01 05:04) with the engine legs renamed `glasm-*` → `glua-*` and
adapted to the native engine. Where glasm raced the wasm boundary, this
repo races **its own root-package interpreter through `host/`** against
the same C target: stock C Lua 5.5.1 driven through its **C embedding
API** — one `lua_State`, each chunk loaded once, called with fresh
KEYS/ARGV tables per run, the Redis-EVAL shape. Short scripts, thousands
of staged runs, wall time AND memory.

Recorded run: tree at `2924562` (N2) plus this suite's new files
(`cmd/embedcmp/`). Raw log: `note/embedcmp-full-20261101-105538.txt`
(gitignored, run 2026-11-01 10:55 — same day as glasm's run on this
host, which is as comparable as cross-suite quotes get here; the
same-session rule still applies, machine perf shifts 2.2–3.3× across
days, and e_\* numbers are never comparable to luabench's h_\* numbers).

Mechanism deltas vs glasm's suite, all visible in the numbers:

- **No two-tier compile cache** — the engine's cache is memory-only, so
  `glua-cold` is a **real parse+compile** per restart (glasm's cold leg
  measured a warm disk-cache hit). No `-cachedir` flag exists.
- **NewVM is µs-scale** (a plain `LState` + `SafeStdLibs` open), not a
  ~2.6 ms wasm instantiation — the fresh legs' 200-run cap is kept for
  glasm-number comparability, not necessity.
- `USED` is the **native accounted-allocation gauge** (N2 `membudget.go`)
  on a manually pinned VM, not a frozen-GC guest total.

## The comparison target

`cmd/embedcmp/c/embedbench55.c` — a custom C `main` against the same
vendored zero-patch tree glasm uses (`cmd/embedcmp/c/lua55`, glasm's
`runtime/lua55` unmodified), built by `cmd/embedcmp/build-embedbench55.sh`
(same flags as glasm's build, so the C legs are the same binary shape):

```sh
clang -O2 -Wall -DLUA_USE_MACOSX -I cmd/embedcmp/c/lua55/src \
  -o note/native-lua55/embedbench cmd/embedcmp/c/embedbench55.c \
  cmd/embedcmp/c/lua55/src/*.c -lreadline -lm
```

(The C sources live under `cmd/embedcmp/c/` — no `.go` files — because a
Go package dir may not hold `.c` sources without cgo; `go build ./...`
ignores the subdirectory.)

Stock runtime defaults. Both harnesses stage identical per-run arguments:
`KEYS = {"key:1".."key:10"}`, `ARGV = {"17", "4", "alpha beta", "0"}`
(string tables; numbers via `tonumber()` in the scripts), and both
register a `bench.call` hostfn (one string down, its byte-length integer
back) as a C closure / a `host.RegisterGlobal` Go closure.

## Reproduce

```sh
cmd/embedcmp/build-embedbench55.sh
go run ./cmd/embedcmp                  # full suite (~4 min) → 8 markdown tables
go run ./cmd/embedcmp -only call       # one script
```

## Environment

|            |                                                        |
|------------|--------------------------------------------------------|
| date       | 2026-11-01 10:55 (single session, one recorded run)    |
| host       | Apple M4 Max, 36 GB, darwin 27.0.0                     |
| Go         | go1.27.0, GOMAXPROCS=14                                |
| C compiler | Apple clang 21.0.0, `-O2 -DLUA_USE_MACOSX`             |
| child env  | `GODEBUG=asyncpreemptoff=1` (posture parity with glasm's record) |
| timing     | C harness: `mach_absolute_time`; Go children: `time.Now` |

## Methodology

Engines × postures (glua legs = this repo's `host/` package, default
options except where noted):

| leg | shape | cold | warm |
|---|---|---|---|
| clua-reuse | ONE state, ONE `luaL_loadbuffer` (registry ref), N staged pcalls — the Redis-EVAL shape | fresh process, 1 run | N in-process runs |
| clua-fresh | naive embedder: newstate+openlibs+register+load+stage+pcall+close per run | — (process-identical to clua-reuse cold) | N runs, capped at 200 |
| glua-cold | fresh process: `NewEngine` → `Compile` (real parse — no disk tier) → exactly 1 `e.Run` — the daemon restart | fresh process, 1 run | — |
| glua-fresh | one process, `Compile` once, N `e.Run` **without** the VM pool (fresh VM per run) | — | N runs, capped at 200 |
| glua-pool | `WithVMCacheSize(4)`: one untimed priming run, then N pool-hit `e.Run`s via the real daemon API | — | N runs |

Corpus: the same 7 short EVAL-shaped scripts glasm uses
(`cmd/embedcmp/scripts/e_*.lua`) — arg arith, KEYS aggregation,
reply-array build, string normalization, 40-field hash loop, 200
`bench.call` hostfn crossings, 500-element sort. All stateless across
runs; per-script counts {coldReps, warmN} = 12×3000
(e_arg/e_sum/e_tag/e_norm), 8×2000 (e_hash/e_call), 8×500 (e_sort);
fresh legs cap at 200.

Measurement honesty rules (inherited from glasm's suite):

- **KEYS/ARGV staging is inside the timed region on BOTH sides** (Redis
  restages per run; `host` stages inside `e.Run`). Result collection too.
- **Cold "process wall" columns carry the ~3.7 ms `/usr/bin/time -l`
  exec floor** (both engines) — hence the separate "cold first run
  (in-process)" and "cold load/compile" tables.
- **maxRSS is a peak** — Go heap high-water ratchets show fully.
- **GC asymmetry**: C runs its stock incremental GC during the loop;
  Go runs its GC too, so warm `go-heap live` IS a steady-state figure —
  but `USED` is the monotone **cumulative-charge** gauge (never
  refunded, by N2 design; pooled VMs reset it at 1024 runs via
  recycling). The C `lua-heap` column is **live** heap after a forced
  full collect. Live vs cumulative — labeled differently, not
  comparable one-to-one.
- Every leg's return values are cross-checked against clua-reuse
  (`normRet`) — this run: **zero mismatches across all 7 scripts × 5
  legs**, the staged-args contract holding on both harnesses.

## Results — wall time

### Cold wall (one-shot process, median; includes the ~3.7 ms harness floor)

| script | clua-reuse | glua-cold |
|--------|------------|-----------|
| e_arg  | 3.55ms     | 4.76ms    |
| e_sum  | 3.52ms     | 4.72ms    |
| e_tag  | 3.55ms     | 4.74ms    |
| e_norm | 3.83ms     | 5.02ms    |
| e_hash | 3.81ms     | 4.82ms    |
| e_call | 3.60ms     | 4.84ms    |
| e_sort | 3.67ms     | 4.95ms    |

### Cold first run (in-process, excludes the /usr/bin/time floor)

| script | clua-reuse | glua-cold |
|--------|------------|-----------|
| e_arg  | 3.0µs      | 55.0µs    |
| e_sum  | 4.7µs      | 57.4µs    |
| e_tag  | 5.5µs      | 67.5µs    |
| e_norm | 3.6µs      | 73.7µs    |
| e_hash | 14.7µs     | 79.2µs    |
| e_call | 35.0µs     | 121.7µs   |
| e_sort | 50.1µs     | 221.5µs   |

### Cold script load/compile (in-process; glua = real parse, no cache tier)

| script | clua-reuse (loadbuffer) | glua-cold (Compile) |
|--------|-------------------------|---------------------|
| e_arg  | 7.0µs                   | 28.9µs              |
| e_sum  | 10.0µs                  | 41.2µs              |
| e_tag  | 9.2µs                   | 31.6µs              |
| e_norm | 8.4µs                   | 30.0µs              |
| e_hash | 9.9µs                   | 32.6µs              |
| e_call | 8.8µs                   | 44.1µs              |
| e_sort | 19.3µs                  | 31.2µs              |

### Warm wall (per run, steady state, median)

| script | clua-reuse | clua-fresh | glua-fresh | glua-pool |
|--------|------------|------------|------------|-----------|
| e_arg  | 0.3µs      | 14.8µs     | 24.3µs     | 2.9µs     |
| e_sum  | 0.7µs      | 17.3µs     | 27.1µs     | 6.2µs     |
| e_tag  | 1.3µs      | 16.9µs     | 27.2µs     | 6.3µs     |
| e_norm | 0.5µs      | 15.7µs     | 28.0µs     | 7.5µs     |
| e_hash | 5.2µs      | 23.5µs     | 38.3µs     | 17.9µs    |
| e_call | 14.8µs     | 37.8µs     | 77.9µs     | 62.0µs    |
| e_sort | 42.0µs     | 59.4µs     | 168.2µs    | 147.7µs   |

### Warm wall × native reuse (glua legs ÷ clua-reuse)

| script | clua-fresh | glua-fresh | glua-pool |
|--------|------------|------------|-----------|
| e_arg  | 44.5×      | 72.9×      | 8.8×      |
| e_sum  | 24.4×      | 38.3×      | 8.8×      |
| e_tag  | 12.7×      | 20.4×      | 4.8×      |
| e_norm | 34.3×      | 61.2×      | 16.5×     |
| e_hash | 4.5×       | 7.3×       | 3.4×      |
| e_call | 2.6×       | 5.3×       | 4.2×      |
| e_sort | 1.4×       | 4.0×       | 3.5×      |

## Results — memory

### Cold maxRSS (one-shot process)

| script | clua-reuse | glua-cold |
|--------|------------|-----------|
| e_arg  | 2.0MB      | 7.3MB     |
| e_sum  | 2.0MB      | 7.4MB     |
| e_tag  | 2.0MB      | 7.3MB     |
| e_norm | 2.0MB      | 7.5MB     |
| e_hash | 2.0MB      | 7.4MB     |
| e_call | 2.1MB      | 7.5MB     |
| e_sort | 2.1MB      | 7.6MB     |

### Warm maxRSS (loop process)

| script | clua-reuse | clua-fresh | glua-fresh | glua-pool |
|--------|------------|------------|------------|-----------|
| e_arg  | 2.1MB      | 2.3MB      | 8.1MB      | 8.5MB     |
| e_sum  | 2.1MB      | 2.3MB      | 8.4MB      | 8.3MB     |
| e_tag  | 2.2MB      | 2.3MB      | 8.4MB      | 8.2MB     |
| e_norm | 2.1MB      | 2.3MB      | 8.6MB      | 8.5MB     |
| e_hash | 2.2MB      | 2.3MB      | 8.2MB      | 8.5MB     |
| e_call | 2.1MB      | 2.3MB      | 8.5MB      | 8.6MB     |
| e_sort | 2.2MB      | 2.5MB      | 8.5MB      | 8.6MB     |

### Memory detail (post-loop)

| script | clua lua-heap (live, post-collect) | glua go-heap live | glua accounted USED 1→N (monotone gauge) |
|--------|------------------------------------|-------------------|------------------------------------------|
| e_arg  | 19.6KB                             | 539KB             | 12KB → 1.7MB                             |
| e_sum  | 19.8KB                             | 539KB             | 12KB → 2.7MB                             |
| e_tag  | 19.8KB                             | 539KB             | 13KB → 5.0MB                             |
| e_norm | 19.7KB                             | 538KB             | 12KB → 1.7MB                             |
| e_hash | 21.9KB                             | 533KB             | 17KB → 11.9MB                            |
| e_call | 21.8KB                             | 532KB             | 18KB → 13.5MB                            |
| e_sort | 19.8KB                             | 510KB             | 20KB → 4.4MB                             |

(`USED` = `vm.UsedBytes` on a manually pinned VM serving all N runs —
the pool is private to `host.Engine` and recycles at 1024 runs, which
would reset the gauge; the table shows the raw per-run accumulation.
~0.5 KB/run for the staging-only scripts, ~6.7 KB/run on e_call's 200
hostfn crossings — cumulative charge, not live set.)

## Findings

1. **The daemon posture (glua-pool) runs 3.4–16.5× C — where the wasm
   engine ran 13–62×.** Same harness, same corpus, same host, same day:
   glasm's pool legs were 20.7–189.7 µs; these are 2.9–147.7 µs. The
   ratio compresses with real work exactly as glasm observed (e_sort
   3.5×, e_call 4.2×, e_hash 3.4×) — the residual on trivial scripts
   (e_arg 8.8×, e_norm 16.5×) is a **fixed per-run floor of ~2.6 µs**
   (pool dispatch + KEYS/ARGV staging + result collection) against C's
   ~0.2 µs, not per-instruction slowdown.
2. **The hostfn crossing is ~0.30 µs each** ((62.0−2.9)/200 on e_call)
   vs C's ~72 ns ((14.8−0.3)/200) and glasm's ~0.85 µs. The wire is
   gone; what remains is the seam-Value conversion (owning LValue-tree
   walk per call). A Redis script doing dozens of `redis.call`s per
   EVAL pays ~4× C per call now.
3. **The no-pool posture collapsed from load-bearing to optional.**
   glua-fresh runs a trivial script in 24–28 µs (NewVM = LState build +
   SafeStdLibs open) vs clua-fresh's 15–17 µs — **~1.6× C**, where
   glasm paid ~2.6 ms (~170× its own pool, ~100× C). For glasm the VM
   pool was feasibility; here it is a ~8× latency optimization on
   trivial scripts that vanishes as scripts do work. Ultima's default
   fresh-VM-per-run posture is daemon-viable on these numbers.
4. **Daemon restart: 4.7–5.0 ms process wall vs C's 3.5–3.8 ms** —
   ~1.3×, dominated by the shared ~3.7 ms exec floor. In-process the
   first run is 55–222 µs vs C's 3–50 µs; `Compile` is a real parse
   now (29–44 µs vs C's 7–19 µs loadbuffer, ~3–4× — the known
   interpreter-frontend gap, and the pure-Go floor: no instantiation
   seam exists to remove).
5. **Memory: ~3.5–4× C's RSS** — 7.3–7.6 MB one-shot / 8.1–8.6 MB warm
   loop vs C's 2.0 / 2.1–2.5 MB, and 6–8× smaller than glasm's 35 /
   50–70 MB. This corroborates Option A §1's "~8–13 MB per VM"
   estimate (the warm loop shares one VM; the number is the Go runtime
   + one LState + SafeStdLibs, not a per-VM multiplier). Live Go heap
   after the loop is ~0.5 MB vs C's ~20 KB live Lua heap — the LState
   + library tables and Go allocator granularity, not guest garbage:
   these scripts leak nothing and the gauge's growth is charge-only.
6. **RET cross-check green across the suite** — 7 scripts × 5 legs,
   zero mismatches: both harnesses stage byte-identical KEYS/ARGV and
   both engines return identical values, including through 200 hostfn
   crossings per run. The N2 staging seam behaves under the daemon
   postures it was built for.

## Open items

- The ~2.6 µs pool-hit floor is now the top per-run cost on trivial
  scripts. Decomposition (dispatch vs staging vs result conversion) is
  a P4/P5-class seam; the P1/P2 payoff scripts are e_sort (3.5×) and
  e_call (4.2×), whose warm time is real interpreter work.
- C-side bytecode-cache fresh mode (`lua_dump` once, load bytecode per
  fresh state) unmeasured — carried from glasm's open items.
- e_\* vs h_\* corpora share no clock-path or shape — never cross-quote.

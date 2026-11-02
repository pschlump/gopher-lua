# P7 — embed-seam allocation diet (2026-11-02)

The embedcmp open items after P6 were the **pool-hit per-run floor**
(e_arg 2.7µs vs C reuse 0.3µs) and the **hostfn crossing cost**.
P7 decomposed both in-process (`host/p7_bench_test.go`, the durable
rig) and found the fixable mass was *allocations*, not latency: the
crossing's `make([]Value, n)` per call and staging's per-element
LString→LValue interface boxes. All changes live in `host/` plus one
new exported helper in `alloc.go` — no `_vm.go`/`_state.go` edit, so
the P3 no-op-layout law is untouched by construction.

## Decomposition (pool-hit e_arg, 877ns / 1445B / 35 allocs pre-P7)

| stage                    | time    | bytes | allocs |
|--------------------------|---------|-------|--------|
| staging (KEYS+ARGV)      | 229ns   | 704B  | 20     |
| script's own execution   | ~570ns  | —     | —      |
| FnFromProto (closure)    | 27ns    | 136B  | 3      |
| pool take/put            | 38ns    | 48B   | 1      |
| result conversion        | 15ns    | 0B    | 0      |

A `-memprofile` of the staging bench named the 20: **14 are the
per-element string boxes** (`host.stringTable`, 70% of all objects) —
an `LValue` interface holding a string must heap-allocate its 16-byte
header, which is exactly why stock C Lua has a global string intern
table. 4 more are the two `LTable`s + array backings (legitimate — the
tables are fresh per run by contract), and 2 are re-boxing the constant
`"KEYS"`/`"ARGV"` names every run.

## Changes

1. **Per-VM staging intern** (`host/vm.go`): repeated KEYS/ARGV
   strings reuse their boxed LValue (`VM.intern`, bound
   `stagingInternMax = 512` — C interns unconditionally, a daemon
   cannot). Boxes hold immutable strings, so reuse is observationally
   identical to re-boxing. The name LValues are pre-boxed package
   constants.
2. **Crossing scratch** (`host/hostfn.go`): the trampoline refills a
   per-VM `argsScratch []Value` instead of `make`ing per call. A9
   makes this safe — only the lock-holding goroutine runs a
   trampoline, and same-VM re-entry is refused by the hostDepth guard
   — and the HostFunc contract now says the args slice is engine
   scratch: copy (`slices.Clone`) to retain. The event-sink (print)
   path still allocates: log sinks legitimately retain args.
3. **`lua.IntegerValue`** (`alloc.go`): exported small-integer box
   through the P6 static cache ([-1024,1024), both signs); the seam's
   `toLValue` integer path routes through it. (Marginal for counts —
   Go's runtime already static-boxes [0,256) — but free, and it covers
   negatives and [256,1024).)

## A/B (`go test ./host -bench P7 -count=6`, benchstat, p≤0.002 unless ~)

| bench          | sec/op            | B/op              | allocs/op    |
|----------------|-------------------|-------------------|--------------|
| PoolHitEArg    | −0.4% (~)         | 1411→1193B −15.4% | 35→21 −40%   |
| PoolHitOneCall | −3.1%             | 1505→1210B −19.6% | 32→17 −47%   |
| PoolHitECall   | **−5.8%**         | 42.6→26.8KB −37%  | 837→623 −26% |
| Staging        | −5.2%             | 704→480B −31.8%   | **20→6 −70%**|
| FnFromProto / PoolTakePut / ResultConv | ~ | ~ | ~ |

Staging's 6 remaining allocs: the two fresh tables + backings (4) plus
the rig's own inline `LString("KEYS")` boxes (2) — the real `Run` path
hoists those, leaving 4.

Wall-clock (embedcmp, same-day stash A/B, 10 keys/4 argv): pool lanes
flat to −2% (e_call 43.0→42.0µs); the in-process −5.8% is the
controlled crossing number. **P7's product is garbage, not latency**:
−26% allocs per crossing, −40% per pool-hit run — proportionally less
GC churn per EVAL under daemon load, the metric the README memory
section tracks.

## Gates

Full suite green with the golden leg uncached (zero golden drift — no
behavior change); `go vet ./...` clean; race battery
(TestHost/TestChannel/TestContext/TestGoldenGlua + the two new P7
gates) clean; `CGO_ENABLED=0 go build ./...` clean. New gates:
`TestStagingInternReuse` (pool-reused VM, alternating payloads, and a
600-string sweep past the intern bound) and `TestCrossingScratchArgs`
(sequential crossings refill the scratch; inline-reading host functions
see exactly their own call's arguments).

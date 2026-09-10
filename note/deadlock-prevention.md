am using a Lwa to Wasm compiler that is embedded in a Go clone of
Redis.   What will it take to make it so that the Lua is goroutine
safe / thread safe.   This will include either deadlock prevention
or deadlock detection.  Suppose that in the Lua code there are 2
different Redis data structures AA and BB that get used.  Then
with 2 different Lua scripts one access AA and then BB, the 2nd
one accesses BB then AA.   During the access of each one there are
internal locks that each Lua routine will get.   How do we prevent
them from deadlocking.   Do some research on this.   I have
implemented all of the code including the Lua compiler and the
Redis clone in Go.


---


Good news first: this exact problem — two scripts touching the same two keys in opposite order — is precisely the class of bug that real Redis architecturally avoids, not solves. Worth understanding why, because it changes what "fixing" this actually means for you.

## Why real Redis doesn't have this problem

Real Redis is single-threaded for command/script execution. Scripts are executed in Redis by an embedded execution engine, and while executing the script, all server activities are blocked during its entire runtime, meaning all of the script's effects either have yet to happen or had already happened. There's no fine-grained per-key locking to deadlock on, because there's no concurrency during script execution at all — no other Redis command can execute until the script completes. AA/BB lock ordering is a non-issue because there's only ever one lock: the giant one around the whole engine.

Since you've built a Wasm-compiled Lua runtime that presumably *does* run scripts concurrently across goroutines (that's the whole appeal of your architecture over stock Redis), you've reintroduced the problem real Redis engineered away. So you have a real design decision, not just a bug to patch.

## Two honest paths

**Path A — Serialize script execution globally.** One giant mutex (or a single-goroutine command queue) around Lua/Wasm script execution, matching real Redis semantics exactly. Zero deadlock risk, zero extra machinery, but you give up the concurrency that's presumably the point of a Go-native implementation. Worth stating explicitly because it's the "boring but correct" baseline — everything below is what you buy with the added complexity of finer-grained locking.

**Path B — Fine-grained per-key/per-structure locking with real concurrency.** This is where AA/BB ordering matters, and where you need one of the two mechanisms below.

## Deadlock prevention: use the fact that Redis already tells you the keys upfront

The `EVAL script numkeys key1 key2 ...` convention isn't just a Lua API detail — it's a gift for exactly this problem. If your `EVAL`-equivalent requires scripts to declare their `KEYS[]` array before execution starts, you already know, before running any Lua, the full set of data structures a script intends to touch.

That lets you do **canonical lock ordering**, the classic deadlock-prevention technique: sort the declared keys (lexicographically, by hash, whatever's consistent) and acquire the locks in that fixed global order, every time, regardless of the order the script accesses them in.

```go
func acquireOrdered(keys []string, registry *LockRegistry) []*sync.Mutex {
    sorted := append([]string(nil), keys...)
    sort.Strings(sorted) // any total, consistent order works
    locks := make([]*sync.Mutex, len(sorted))
    for i, k := range sorted {
        locks[i] = registry.MutexFor(k)
        locks[i].Lock()
    }
    return locks
}
```

Script 1 (`AA` then `BB`) and Script 2 (`BB` then `AA`) both end up acquiring `AA` before `BB` at the OS/mutex level, because the acquisition order is derived from a sort, not from the script's access order. This eliminates the circular-wait condition entirely — it's not detection, it's structural prevention. No timeout, no retry, no possibility of the deadlock happening. This is the same technique used in DB engines that grab row locks in primary-key order to avoid exactly this AB/BA case.

**The catch:** this only works if you actually enforce that scripts only touch declared keys. Lua/Redis scripting technically lets a script call `redis.call` on a key never listed in `KEYS[]`. You have three choices:

1. **Enforce declaration strictly** — at runtime, if a script tries to access a data structure not in its declared `KEYS[]`, error out. This is close to what real Redis *recommends* (scripts should only touch declared keys) even though older Redis doesn't hard-enforce it.
2. **Static analysis of the compiled Wasm/Lua** — walk the script to extract all key literals/expressions it might touch and pre-declare them, falling back to (3) for keys that are computed dynamically.
3. **Two-phase fallback for undeclared keys** (see below) — treat any key not in the declared set as needing best-effort locking with a real deadlock-safe fallback, since you can't order what you don't know about upfront.

## If you need dynamic (non-declared) key access: two-phase locking with wound-wait

For the residual case where a script determines what key to touch only at runtime (computed key names, conditional branches), ordering breaks down because you don't have the full key set before you start locking. Standard technique here, straight out of database concurrency control, is **wound-wait** or **wait-die**:

- Give every script execution a monotonically increasing timestamp/sequence number when it starts (its "age").
- When script X (holding some locks) needs a lock held by script Y:
  - **Wound-wait**: if X is older than Y, X "wounds" Y (aborts Y, Y releases everything and retries later); if X is younger, X waits normally.
  - **Wait-die**: if X is older than Y, X waits; if X is younger, X dies (aborts and retries).
- Either scheme guarantees no cycle can form, because locks are only ever waited-for in one direction relative to age — a strict global order, just on timestamps instead of key names.

This requires your scripts' side effects to be safely abortable/retryable up to the point of the wound — i.e., you should acquire all locks for a script's execution *before* it produces any externally visible effects (buffer writes, don't commit until you're sure you'll hold every lock you need). That's usually true anyway if you structure execution as "acquire everything → run → commit → release."

## Deadlock detection (if you'd rather detect than prevent)

If prevention feels like too much surgery on your key-declaration model, detection is the fallback, with two very different flavors:

**Dev/test-time detection** — swap `sync.Mutex` for `github.com/sasha-s/go-deadlock`. One of the most common sources of deadlocks is inconsistent lock ordering — go-deadlock records the order locks are acquired in per-goroutine and reports when it sees that ordering violated, and it also flags a potential deadlock if a goroutine blocks on a mutex longer than a configurable timeout, printing the stack trace of the goroutine currently holding the contested lock. This is excellent for catching AA/BB-style bugs in CI and load tests before they hit production, and it's a practical tool for finding violations of total lock ordering, used in production-grade codebases like CockroachDB and rclone to find deadlocks before they happen. I wouldn't run it as your live production deadlock strategy though — by default it prints and exits the program when it detects a violation, which is not what you want a live Redis-clone process to do on a client-triggered script conflict. Use it in your test suite to *find* every AA/BB-shaped bug across all your scripts before shipping, not as the runtime safety net.

**Production-time detection/recovery** — build a lightweight wait-for graph: a node per in-flight script goroutine, an edge A→B when A blocks waiting on a lock held by B. On every blocking lock attempt, add the edge (and run cycle detection via DFS, or check incrementally); on acquire, remove it. If a cycle is found, abort the youngest (or lowest-priority) participant in the cycle — release its held locks, return an error/retry signal for that script — exactly like wound-wait but applied reactively instead of preventively. This is strictly more general than lock-ordering (handles arbitrary dynamic access patterns) but costs you a global graph structure that every lock operation has to touch, which is real overhead in a high-throughput data store.

A cheaper approximation of detection that a lot of real systems use instead of a full wait-for graph: **`TryLock` + timeout + jittered backoff**. Go's `sync.Mutex` has had `TryLock()` since 1.18. Acquire each needed lock non-blocking; if any acquisition fails, release everything you're holding, sleep a random short interval, and retry the whole script from the top. This never actually detects a cycle — it just refuses to ever hold-and-wait, so Coffman's "hold and wait" condition is broken instead of "circular wait." Simpler to implement than a graph, at the cost of some wasted retries under contention.

## What I'd actually do for your setup

Given you already have `KEYS[]`-declaring semantics (or can add them) and a Wasm-compiled Lua where you control the `redis.call` intercept layer:

1. Require/derive the key set upfront per script invocation.
2. Acquire per-key-structure locks in a fixed canonical (sorted) order — this alone kills the AA/BB case you described, deterministically, with no retries or timeouts needed.
3. For the rare case of a script computing a key dynamically mid-execution that wasn't declared, fall back to `TryLock`-with-backoff-and-full-restart rather than blocking — never block-and-wait on an undeclared key while holding others.
4. Run `go-deadlock` in your test/fuzz harness (spin up pairs of scripts with intentionally conflicting access orders, like your AA/BB example) to catch any place where ordering assumption 2 was violated by a bug, before it reaches production.

That combination gets you true prevention on the common path (which is your entire example) and a safe, bounded-retry fallback on the uncommon one, with no global stop-the-world lock and no runtime cycle-detection overhead on the hot path.

package lua

/* Per-VM accounted memory budget (N2). Stock wraps its allocator
 * (l_alloc) so every luaM_ allocation is counted and luaM_error raises
 * "not enough memory"; the wasm engine interposed rt_alloc the same
 * way. Go allocations cannot be interposed, so the budget is accounted
 * at the big seams instead: table creation and growth (the per-slot
 * estimates collectgarbage("count") has always charged — 16 per array
 * slot, 48 per hash slot, 64 per table header) and the string builders
 * (string.concat and table.concat charge their built result,
 * string.rep pre-flights its repeat count). Small allocations
 * (individual LValues, upvalue cells) ride inside those estimates — an
 * acknowledged approximation of stock's byte-exact accounting
 * (divergence ledger row), trading exactness for zero cost on
 * unbudgeted VMs.
 *
 * The counter is never refunded (Go's collector frees underneath), so
 * it is a monotone high-water gauge — the stopped-GC watermark posture
 * the wasm engine shipped. Crossing sets the sticky over flag; the seam
 * sites and the per-instruction boundary check (mainLoop /
 * mainLoopWithContext, beside the deadline check) surface it as a
 * pcall-catchable "not enough memory". Overshoot between the crossing
 * charge and the next raise point is bounded by one instruction's
 * allocations, except a single builder call that itself crosses the
 * limit mid-build — its result charge lands only after the build, so
 * a budget crossed mid-build overshoots by that one result
 * (string.rep pre-flights to decline before building).
 *
 * Fields live on Global (set up before the first run, read on the
 * running goroutine afterwards — race-free by thread-creation
 * happens-before; see value.go).
 */

// tableCharge is the creation/growth estimate for a table shape — the
// same numbers gcCharge feeds collectgarbage("count") (table.go).
func tableCharge(acap, hcap int) int64 {
	return int64(acap)*16 + int64(hcap)*48 + 64
}

// memCharge accounts n bytes against this VM's budget. Always safe on
// a nil Global.
func (g *Global) memCharge(n int64) {
	if g == nil || n <= 0 {
		return
	}
	if g.memUsed.Add(n) > g.memLimit && g.memLimit > 0 {
		g.memOver.Store(true)
	}
}

// memBudgetExceeded reports whether the budget has been crossed — the
// per-instruction boundary check and the seam sites poll it before
// raising. A plain memLimit read first keeps unbudgeted VMs at one
// field load.
func (ls *LState) memBudgetExceeded() bool {
	g := ls.G
	return g != nil && g.memLimit > 0 && g.memOver.Load()
}

// memRaise is the seam-site raise: stock luaM_error's text, an ordinary
// pcall-catchable Lua error with the running frame's position
// (luaG_runerror semantics).
func (ls *LState) memRaise() {
	ls.RaiseError("not enough memory")
}

// SetMemoryBudgetBytes arms the per-VM accounted memory budget: once
// set, the allocation seams charge against it and the first write past
// it raises a pcall-catchable "not enough memory". Zero disables the
// limit (MemoryUsedBytes keeps reporting the charge total). Must be
// called before the first run (the limit is not read atomically — see
// membudget.go's header).
func (ls *LState) SetMemoryBudgetBytes(n int64) {
	ls.G.memLimit = n
	if n <= 0 {
		ls.G.memOver.Store(false)
	}
}

// MemoryUsedBytes reports the accounted allocation total (monotone —
// never refunded; see membudget.go's header).
func (ls *LState) MemoryUsedBytes() int64 {
	return ls.G.memUsed.Load()
}

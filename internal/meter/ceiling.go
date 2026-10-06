package meter

import "sync"

// ceilingCounter is an absolute, run-scoped counter: unlike windowLimiter,
// it never prunes -- once total crosses limit, exceeded() stays true for
// the rest of this process's life. This is what makes
// Config.TokenCeiling/CostCeilingMicroUSD an actual per-run ceiling (see
// their own doc comment on the distinction from the sliding-window
// TokenBudget/CostBudgetMicroUSD, which stay in place unmodified as a
// separate burst control).
type ceilingCounter struct {
	mu            sync.Mutex
	limit         int64
	total         int64
	forceExceeded bool
}

func newCeilingCounter(limit int64) *ceilingCounter {
	return &ceilingCounter{limit: limit}
}

// exceeded reports whether this counter's limit has ever been crossed. Once
// true, it never reverts to false -- there is no window to age back out of.
func (c *ceilingCounter) exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.forceExceeded || c.total >= c.limit
}

// add accumulates cost against the running total. Negative/zero cost is a
// no-op, matching windowLimiter.add's own convention.
func (c *ceilingCounter) add(cost int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cost <= 0 {
		return
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if c.total > maxInt64-cost {
		// Overflow: force permanently exceeded rather than let the total
		// wrap negative, which would otherwise let exceeded() incorrectly
		// read as false again after a large-enough single charge.
		c.forceExceeded = true
		return
	}
	c.total += cost
}

// forceExceed marks this counter permanently exceeded regardless of its
// current total -- used when a cost computation itself overflowed (see
// tokenCostMicroUSD), the same fail-closed response windowLimiter.
// forceExceed already gives its own sliding-window counterpart.
func (c *ceilingCounter) forceExceed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.forceExceeded = true
}

// seed sets the running total to total, for a counter rebuilt from a
// durable ledger after a restart. It replaces the total (it does not add to
// it) and never clears forceExceeded; a negative total is treated as zero.
func (c *ceilingCounter) seed(total int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if total < 0 {
		total = 0
	}
	c.total = total
}

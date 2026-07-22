package ratelimit

import "sync"

// ConcurrencyLimiter caps in-flight non-GET requests per billing account —
// the control that actually bounds GPU occupancy, which a requests/minute
// bucket cannot (one account could open its whole minute budget as
// simultaneous long-lived SSE streams). A keyed generalization of the
// authlimit bcrypt semaphore: entries are deleted at zero, so idle accounts
// cost nothing and there is nothing to prune. In-flight slots die with the
// pod, which is exactly right — so do the streams they represent
// (docs/design/per-account-rate-limiting.md §3.2).
type ConcurrencyLimiter struct {
	mu    sync.Mutex
	inUse map[int64]int
}

func NewConcurrency() *ConcurrencyLimiter {
	return &ConcurrencyLimiter{inUse: make(map[int64]int)}
}

// TryAcquire reserves a slot for id without blocking; max <= 0 means no cap
// (unconfigured — boot validation keeps prod values >= 1). Callers must pair
// a successful acquire with Release.
func (c *ConcurrencyLimiter) TryAcquire(id int64, max int) bool {
	if c == nil || max <= 0 {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inUse[id] >= max {
		return false
	}
	c.inUse[id]++
	return true
}

// Release frees a slot taken by TryAcquire. Deletes the entry at zero and
// never goes negative.
func (c *ConcurrencyLimiter) Release(id int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.inUse[id]
	if !ok {
		return
	}
	if n <= 1 {
		delete(c.inUse, id)
		return
	}
	c.inUse[id] = n - 1
}

// InFlight reports the total slots currently held across all accounts
// (feeds the aiproxy_streams_inflight gauge).
func (c *ConcurrencyLimiter) InFlight() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.inUse {
		total += n
	}
	return total
}

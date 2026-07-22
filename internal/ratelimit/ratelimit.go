// Package ratelimit provides the token-bucket rate limiting for the chat
// proxy path: a key-level limiter (per api_keys.rate_limit) and an
// account-level limiter keyed by the billing account
// (docs/design/per-account-rate-limiting.md). The deployment is
// single-replica, so in-memory buckets are sufficient; a shared/distributed
// limiter is intentionally out of scope.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// pruneInterval is how often idle buckets are evicted; idleCutoff is the
// minimum idle time before eviction (matches authlimit).
const (
	pruneInterval = 1 * time.Minute
	idleCutoff    = 10 * time.Minute
)

type bucket struct {
	tokens     float64
	capacity   float64
	refillRate float64 // tokens per second
	lastRefill time.Time
	lastAccess time.Time
}

// Limiter manages token buckets keyed by int64 ID. The proxy runs two
// instances — one keyed by API-key ID, one keyed by billing-account ID.
// They must stay separate instances: the two ID spaces overlap, so a shared
// map would collide key #7 with account #7.
type Limiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
	nowFn   func() time.Time
}

// New builds a Limiter on the wall clock and starts the background prune
// goroutine. Use this from main.
func New() *Limiter {
	limiter := NewWithClock(time.Now)
	go func() {
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			limiter.prune()
		}
	}()
	return limiter
}

// NewWithClock builds a Limiter with an injectable clock and no prune
// goroutine (the authlimit pattern). Exported for deterministic tests.
func NewWithClock(nowFn func() time.Time) *Limiter {
	return &Limiter{
		buckets: make(map[int64]*bucket),
		nowFn:   nowFn,
	}
}

// Allow checks if a request is allowed for the given ID and rate limit.
// Returns (allowed, retryAfter in seconds). Capacity and refill rate are
// rewritten on every call, so limit changes apply on the next request.
func (l *Limiter) Allow(id int64, rateLimit int) (bool, float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.nowFn()
	tokenBucket, exists := l.buckets[id]

	if !exists {
		tokenBucket = &bucket{
			tokens:     float64(rateLimit) - 1, // consume one token now
			capacity:   float64(rateLimit),
			refillRate: float64(rateLimit) / 60.0,
			lastRefill: now,
			lastAccess: now,
		}
		l.buckets[id] = tokenBucket
		return true, 0
	}

	// Update capacity/refill if rate limit changed
	tokenBucket.capacity = float64(rateLimit)
	tokenBucket.refillRate = float64(rateLimit) / 60.0

	// Refill tokens based on elapsed time
	elapsed := now.Sub(tokenBucket.lastRefill).Seconds()
	tokenBucket.tokens = math.Min(tokenBucket.capacity, tokenBucket.tokens+elapsed*tokenBucket.refillRate)
	tokenBucket.lastRefill = now
	tokenBucket.lastAccess = now

	if tokenBucket.tokens >= 1 {
		tokenBucket.tokens--
		return true, 0
	}

	// Calculate retry-after: time until 1 token is available
	retryAfter := (1 - tokenBucket.tokens) / tokenBucket.refillRate
	return false, retryAfter
}

// Return refunds one token to id's bucket, clamped at capacity. Used when a
// later gate rejects a request whose account bucket already charged it: on
// the shared trusted key the key bucket (service key's aggregate) and the
// account bucket (individual end user) have different owners, so an
// aggregate rejection must not burn the individual's budget — and their
// Retry-After must stay truthful. No-op for unknown IDs.
func (l *Limiter) Return(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, exists := l.buckets[id]
	if !exists {
		return
	}
	b.tokens = math.Min(b.capacity, b.tokens+1)
}

func (l *Limiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.nowFn().Add(-idleCutoff)
	for id, tokenBucket := range l.buckets {
		if tokenBucket.lastAccess.Before(cutoff) {
			delete(l.buckets, id)
		}
	}
}

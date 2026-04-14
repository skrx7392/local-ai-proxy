package admin

import (
	"math"
	"sync"
	"time"
)

// PerSessionRateLimit is the request-per-minute cap applied to each admin
// session (Bearer token) independently. Matches the existing per-API-key
// pattern with a higher ceiling suitable for interactive UIs.
const PerSessionRateLimit = 300

// sessionPruneInterval is how often stale per-session buckets are removed.
// sessionIdleCutoff is the minimum idle time before a bucket is evicted.
const (
	sessionPruneInterval = 1 * time.Minute
	sessionIdleCutoff    = 10 * time.Minute
)

type sessionBucket struct {
	tokens     float64
	lastRefill time.Time
	lastAccess time.Time
}

// sessionLimiter manages one token bucket per active admin session,
// keyed by the session's token hash. 300 req/min (5 tokens/sec refill).
//
// nowFn is the clock source (injected for deterministic tests); nil
// means wall clock.
type sessionLimiter struct {
	mu      sync.Mutex
	buckets map[string]*sessionBucket
	nowFn   func() time.Time
}

func newSessionLimiter() *sessionLimiter {
	limiter := &sessionLimiter{buckets: make(map[string]*sessionBucket)}
	go func() {
		ticker := time.NewTicker(sessionPruneInterval)
		defer ticker.Stop()
		for range ticker.C {
			limiter.prune()
		}
	}()
	return limiter
}

func (l *sessionLimiter) now() time.Time {
	if l.nowFn != nil {
		return l.nowFn()
	}
	return time.Now()
}

// Allow consumes one token for the given session hash. The bucket is
// created on first use and refilled based on elapsed time from the
// limiter's clock.
func (l *sessionLimiter) Allow(tokenHash string) bool {
	const (
		capacity   = float64(PerSessionRateLimit)
		refillRate = capacity / 60.0 // tokens per second
	)

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	bucket, exists := l.buckets[tokenHash]
	if !exists {
		l.buckets[tokenHash] = &sessionBucket{
			tokens:     capacity - 1,
			lastRefill: now,
			lastAccess: now,
		}
		return true
	}

	elapsed := now.Sub(bucket.lastRefill).Seconds()
	bucket.tokens = math.Min(capacity, bucket.tokens+elapsed*refillRate)
	bucket.lastRefill = now
	bucket.lastAccess = now

	if bucket.tokens >= 1 {
		bucket.tokens--
		return true
	}
	return false
}

func (l *sessionLimiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-sessionIdleCutoff)
	for hash, b := range l.buckets {
		if b.lastAccess.Before(cutoff) {
			delete(l.buckets, hash)
		}
	}
}

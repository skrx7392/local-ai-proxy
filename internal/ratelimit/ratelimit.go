package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/metrics"
)

type bucket struct {
	tokens     float64
	capacity   float64
	refillRate float64 // tokens per second
	lastRefill time.Time
	lastAccess time.Time
}

// Limiter manages per-key token buckets.
type Limiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
}

func New() *Limiter {
	limiter := &Limiter{
		buckets: make(map[int64]*bucket),
	}
	// Prune stale buckets every minute
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			limiter.prune()
		}
	}()
	return limiter
}

// Allow checks if a request is allowed for the given key ID and rate limit.
// Returns (allowed, retryAfter in seconds).
func (l *Limiter) Allow(keyID int64, rateLimit int) (bool, float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	tokenBucket, exists := l.buckets[keyID]

	if !exists {
		tokenBucket = &bucket{
			tokens:     float64(rateLimit) - 1, // consume one token now
			capacity:   float64(rateLimit),
			refillRate: float64(rateLimit) / 60.0,
			lastRefill: now,
			lastAccess: now,
		}
		l.buckets[keyID] = tokenBucket
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

func (l *Limiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for keyID, tokenBucket := range l.buckets {
		if tokenBucket.lastAccess.Before(cutoff) {
			delete(l.buckets, keyID)
		}
	}
}

// Middleware returns HTTP middleware that enforces per-key rate limits.
func Middleware(limiter *Limiter, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFromContext(r.Context())
			if key == nil {
				// No key in context means auth middleware didn't run (shouldn't happen)
				next.ServeHTTP(w, r)
				return
			}

			allowed, retryAfter := limiter.Allow(key.ID, key.RateLimit)
			if !allowed {
				m.RecordRateLimitReject()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", fmt.Sprintf("%.0f", math.Ceil(retryAfter)))
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

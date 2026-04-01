package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
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
	l := &Limiter{
		buckets: make(map[int64]*bucket),
	}
	// Prune stale buckets every minute
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			l.prune()
		}
	}()
	return l
}

// Allow checks if a request is allowed for the given key ID and rate limit.
// Returns (allowed, retryAfter in seconds).
func (l *Limiter) Allow(keyID int64, rateLimit int) (bool, float64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, exists := l.buckets[keyID]

	if !exists {
		b = &bucket{
			tokens:     float64(rateLimit) - 1, // consume one token now
			capacity:   float64(rateLimit),
			refillRate: float64(rateLimit) / 60.0,
			lastRefill: now,
			lastAccess: now,
		}
		l.buckets[keyID] = b
		return true, 0
	}

	// Update capacity/refill if rate limit changed
	b.capacity = float64(rateLimit)
	b.refillRate = float64(rateLimit) / 60.0

	// Refill tokens based on elapsed time
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.refillRate)
	b.lastRefill = now
	b.lastAccess = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Calculate retry-after: time until 1 token is available
	retryAfter := (1 - b.tokens) / b.refillRate
	return false, retryAfter
}

func (l *Limiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, b := range l.buckets {
		if b.lastAccess.Before(cutoff) {
			delete(l.buckets, id)
		}
	}
}

// Middleware returns HTTP middleware that enforces per-key rate limits.
func Middleware(limiter *Limiter) func(http.Handler) http.Handler {
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

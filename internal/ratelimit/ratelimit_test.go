package ratelimit

import (
	"testing"
	"time"
)

func TestAllow_FirstRequest(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}
	allowed, retryAfter := l.Allow(1, 60)
	if !allowed {
		t.Error("first request should be allowed")
	}
	if retryAfter != 0 {
		t.Errorf("retryAfter should be 0 for allowed request, got %f", retryAfter)
	}
}

func TestAllow_WithinLimit(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Use a rate limit of 10 so the bucket starts with 10 tokens
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(1, 10)
		if !allowed {
			t.Errorf("request %d should be allowed within limit", i+1)
		}
	}
}

func TestAllow_ExceedsLimit(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Exhaust all tokens (rate limit of 5)
	for i := 0; i < 5; i++ {
		l.Allow(1, 5)
	}

	// Next request should be denied
	allowed, retryAfter := l.Allow(1, 5)
	if allowed {
		t.Error("request exceeding limit should be denied")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter should be positive, got %f", retryAfter)
	}
}

func TestAllow_DifferentKeysIndependent(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Exhaust key 1 (rate limit 2)
	l.Allow(1, 2)
	l.Allow(1, 2)
	allowed, _ := l.Allow(1, 2)
	if allowed {
		t.Error("key 1 should be exhausted")
	}

	// Key 2 should still work
	allowed, _ = l.Allow(2, 2)
	if !allowed {
		t.Error("key 2 should be independent and allowed")
	}
}

func TestAllow_TokenRefill(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Use a small rate limit: 60 tokens/minute = 1 token/second
	rateLimit := 60

	// Use all tokens
	for i := 0; i < rateLimit; i++ {
		l.Allow(1, rateLimit)
	}

	// Should be denied now
	allowed, _ := l.Allow(1, rateLimit)
	if allowed {
		t.Error("should be denied after exhausting tokens")
	}

	// Wait enough time for at least 1 token to refill
	// 60 tokens / 60 seconds = 1 token/sec, so 1.1 seconds should give us 1+ tokens
	time.Sleep(1200 * time.Millisecond)

	allowed, _ = l.Allow(1, rateLimit)
	if !allowed {
		t.Error("should be allowed after token refill")
	}
}

func TestPrune(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Add a bucket with old lastAccess
	l.mu.Lock()
	l.buckets[99] = &bucket{
		tokens:     5,
		capacity:   10,
		refillRate: 10.0 / 60.0,
		lastRefill: time.Now().Add(-15 * time.Minute),
		lastAccess: time.Now().Add(-15 * time.Minute),
	}
	l.mu.Unlock()

	l.prune()

	l.mu.Lock()
	_, exists := l.buckets[99]
	l.mu.Unlock()

	if exists {
		t.Error("stale bucket should have been pruned")
	}
}

func TestPrune_KeepsRecent(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Use Allow to create a recent bucket
	l.Allow(42, 10)

	l.prune()

	l.mu.Lock()
	_, exists := l.buckets[42]
	l.mu.Unlock()

	if !exists {
		t.Error("recent bucket should not be pruned")
	}
}

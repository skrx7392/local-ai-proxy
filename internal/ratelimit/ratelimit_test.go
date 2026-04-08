package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
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

func TestAllow_ZeroRateLimit(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Zero rate limit: first request creates a bucket with capacity 0
	// and tokens = 0 - 1 = -1, so it should still be "allowed" for the first call
	// because the bucket is newly created (first request always allowed).
	allowed, _ := l.Allow(1, 0)
	if !allowed {
		t.Error("first request should be allowed even with zero rate limit")
	}

	// Second request: capacity is 0, tokens will be refilled to min(0, ...)
	// which is 0 or negative, so should be denied
	allowed, retryAfter := l.Allow(1, 0)
	if allowed {
		t.Error("second request with zero rate limit should be denied")
	}
	// With zero refill rate, retryAfter calculation involves division by zero
	// or infinity; just check it doesn't panic and retryAfter is not negative
	if retryAfter < 0 {
		t.Errorf("retryAfter should not be negative, got %f", retryAfter)
	}
}

func TestAllow_CapacityUpdateOnRateLimitChange(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	// Start with rate limit 2
	l.Allow(1, 2)
	l.Allow(1, 2)

	// Exhausted at rate limit 2
	allowed, _ := l.Allow(1, 2)
	if allowed {
		t.Error("should be exhausted at rate limit 2")
	}

	// Now increase rate limit to 6000 — refill rate = 6000/60 = 100 tokens/sec.
	// After 100ms, we get ~10 tokens refilled, which is >= 1.
	time.Sleep(100 * time.Millisecond)
	allowed, _ = l.Allow(1, 6000)
	if !allowed {
		t.Error("should be allowed after increasing rate limit (higher refill rate)")
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

func TestPrune_MixedBuckets(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	now := time.Now()

	l.mu.Lock()
	// Stale bucket
	l.buckets[1] = &bucket{
		tokens:     5,
		capacity:   10,
		refillRate: 10.0 / 60.0,
		lastRefill: now.Add(-20 * time.Minute),
		lastAccess: now.Add(-20 * time.Minute),
	}
	// Recent bucket
	l.buckets[2] = &bucket{
		tokens:     5,
		capacity:   10,
		refillRate: 10.0 / 60.0,
		lastRefill: now,
		lastAccess: now,
	}
	// Exactly at cutoff boundary (10 minutes ago) - should be kept
	l.buckets[3] = &bucket{
		tokens:     5,
		capacity:   10,
		refillRate: 10.0 / 60.0,
		lastRefill: now.Add(-9 * time.Minute),
		lastAccess: now.Add(-9 * time.Minute),
	}
	l.mu.Unlock()

	l.prune()

	l.mu.Lock()
	_, exists1 := l.buckets[1]
	_, exists2 := l.buckets[2]
	_, exists3 := l.buckets[3]
	l.mu.Unlock()

	if exists1 {
		t.Error("bucket 1 (stale, 20 min old) should have been pruned")
	}
	if !exists2 {
		t.Error("bucket 2 (recent) should not have been pruned")
	}
	if !exists3 {
		t.Error("bucket 3 (9 min old, within cutoff) should not have been pruned")
	}
}

func TestMiddleware_NoKeyInContext(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := Middleware(l, nil)(next)

	// Request with no auth key in context
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called when no key in context")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_AllowedRequest(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	mw := Middleware(l, nil)(next)

	key := &store.APIKey{
		ID:        1,
		Name:      "test",
		RateLimit: 60,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	ctx := auth.WithKey(req.Context(), key)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	mw.ServeHTTP(rec, req)

	if !nextCalled {
		t.Error("next handler should be called for allowed request")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_RateLimited(t *testing.T) {
	l := &Limiter{buckets: make(map[int64]*bucket)}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := Middleware(l, nil)(next)

	key := &store.APIKey{
		ID:        1,
		Name:      "test",
		RateLimit: 2, // very low limit
	}

	// Exhaust the rate limit
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx := auth.WithKey(req.Context(), key)
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d should be allowed, got %d", i+1, rec.Code)
		}
	}

	// This request should be rate limited
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := auth.WithKey(req.Context(), key)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", rec.Code)
	}

	// Check response body
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected 'error' object in response")
	}
	if errObj["code"] != "rate_limit_exceeded" {
		t.Errorf("expected code 'rate_limit_exceeded', got %v", errObj["code"])
	}

	// Check Retry-After header
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Error("expected Retry-After header")
	}

	// Check Content-Type
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", ct)
	}
}

func TestNew_CreatesLimiter(t *testing.T) {
	l := New()
	if l == nil {
		t.Fatal("New() returned nil")
	}
	if l.buckets == nil {
		t.Error("buckets map should be initialized")
	}

	// Verify it works by making a request
	allowed, _ := l.Allow(1, 10)
	if !allowed {
		t.Error("first request should be allowed")
	}
}

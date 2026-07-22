package ratelimit

import (
	"math/rand"
	"testing"
	"time"
)

// testClock is a manually-advanced clock so refill behavior is asserted
// deterministically (no time.Sleep).
type testClock struct{ t time.Time }

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
}
func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter() (*Limiter, *testClock) {
	clock := newTestClock()
	return NewWithClock(clock.now), clock
}

func TestAllow_FirstRequest(t *testing.T) {
	l, _ := newTestLimiter()
	allowed, retryAfter := l.Allow(1, 60)
	if !allowed {
		t.Error("first request should be allowed")
	}
	if retryAfter != 0 {
		t.Errorf("retryAfter should be 0 for allowed request, got %f", retryAfter)
	}
}

func TestAllow_WithinLimit(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 10; i++ {
		allowed, _ := l.Allow(1, 10)
		if !allowed {
			t.Errorf("request %d should be allowed within limit", i+1)
		}
	}
}

func TestAllow_ExceedsLimit(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < 5; i++ {
		l.Allow(1, 5)
	}
	allowed, retryAfter := l.Allow(1, 5)
	if allowed {
		t.Error("request exceeding limit should be denied")
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter should be positive, got %f", retryAfter)
	}
}

func TestAllow_DifferentIDsIndependent(t *testing.T) {
	l, _ := newTestLimiter()
	l.Allow(1, 2)
	l.Allow(1, 2)
	if allowed, _ := l.Allow(1, 2); allowed {
		t.Error("id 1 should be exhausted")
	}
	if allowed, _ := l.Allow(2, 2); !allowed {
		t.Error("id 2 should be independent and allowed")
	}
}

func TestAllow_TokenRefill(t *testing.T) {
	l, clock := newTestLimiter()
	rateLimit := 60 // 1 token/second

	for i := 0; i < rateLimit; i++ {
		l.Allow(1, rateLimit)
	}
	if allowed, _ := l.Allow(1, rateLimit); allowed {
		t.Error("should be denied after exhausting tokens")
	}

	clock.advance(1200 * time.Millisecond)
	if allowed, _ := l.Allow(1, rateLimit); !allowed {
		t.Error("should be allowed after token refill")
	}
}

func TestAllow_ZeroRateLimit(t *testing.T) {
	l, _ := newTestLimiter()

	// Zero rate limit: first request creates the bucket (always allowed);
	// afterwards capacity 0 denies everything with a zero refill rate.
	if allowed, _ := l.Allow(1, 0); !allowed {
		t.Error("first request should be allowed even with zero rate limit")
	}
	allowed, retryAfter := l.Allow(1, 0)
	if allowed {
		t.Error("second request with zero rate limit should be denied")
	}
	if retryAfter < 0 {
		t.Errorf("retryAfter should not be negative, got %f", retryAfter)
	}
}

func TestAllow_CapacityUpdateOnRateLimitChange(t *testing.T) {
	l, clock := newTestLimiter()

	l.Allow(1, 2)
	l.Allow(1, 2)
	if allowed, _ := l.Allow(1, 2); allowed {
		t.Error("should be exhausted at rate limit 2")
	}

	// Raise the limit to 6000 → refill 100 tokens/sec; 100ms refills ~10.
	clock.advance(100 * time.Millisecond)
	if allowed, _ := l.Allow(1, 6000); !allowed {
		t.Error("should be allowed after increasing rate limit (higher refill rate)")
	}
}

func TestReturn_RefundsToken(t *testing.T) {
	l, _ := newTestLimiter()

	l.Allow(1, 2)
	l.Allow(1, 2)
	if allowed, _ := l.Allow(1, 2); allowed {
		t.Fatal("should be exhausted before the refund")
	}
	l.Return(1)
	if allowed, _ := l.Allow(1, 2); !allowed {
		t.Error("refunded token should allow one more request")
	}
}

func TestReturn_ClampedAtCapacity(t *testing.T) {
	l, _ := newTestLimiter()

	// One consume then three refunds: tokens must clamp at capacity (5),
	// so exactly 5 further requests pass before denial.
	l.Allow(1, 5)
	l.Return(1)
	l.Return(1)
	l.Return(1)
	for i := 0; i < 5; i++ {
		if allowed, _ := l.Allow(1, 5); !allowed {
			t.Fatalf("request %d should be allowed (clamped refund)", i+1)
		}
	}
	if allowed, _ := l.Allow(1, 5); allowed {
		t.Error("over-refunded bucket must not exceed capacity")
	}
}

func TestReturn_UnknownIDIsNoop(t *testing.T) {
	l, _ := newTestLimiter()
	l.Return(42) // must not panic or create a bucket
	l.mu.Lock()
	_, exists := l.buckets[42]
	l.mu.Unlock()
	if exists {
		t.Error("Return must not create buckets")
	}
}

// A wrong refund ordering silently doubles effective limits — pin the
// invariant tokens <= capacity under arbitrary Allow/Return/refill
// interleavings.
func TestReturn_NeverExceedsCapacity(t *testing.T) {
	l, clock := newTestLimiter()
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		switch r.Intn(3) {
		case 0:
			l.Allow(1, 5)
		case 1:
			l.Return(1)
		case 2:
			clock.advance(time.Duration(r.Intn(500)) * time.Millisecond)
		}
		l.mu.Lock()
		if b, ok := l.buckets[1]; ok && b.tokens > b.capacity {
			l.mu.Unlock()
			t.Fatalf("iteration %d: tokens %f exceeded capacity %f", i, b.tokens, b.capacity)
		}
		l.mu.Unlock()
	}
}

func TestPrune(t *testing.T) {
	l, clock := newTestLimiter()

	l.mu.Lock()
	l.buckets[99] = &bucket{
		tokens:     5,
		capacity:   10,
		refillRate: 10.0 / 60.0,
		lastRefill: clock.now().Add(-15 * time.Minute),
		lastAccess: clock.now().Add(-15 * time.Minute),
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
	l, _ := newTestLimiter()
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
	l, clock := newTestLimiter()
	now := clock.now()

	l.mu.Lock()
	l.buckets[1] = &bucket{tokens: 5, capacity: 10, refillRate: 10.0 / 60.0,
		lastRefill: now.Add(-20 * time.Minute), lastAccess: now.Add(-20 * time.Minute)}
	l.buckets[2] = &bucket{tokens: 5, capacity: 10, refillRate: 10.0 / 60.0,
		lastRefill: now, lastAccess: now}
	l.buckets[3] = &bucket{tokens: 5, capacity: 10, refillRate: 10.0 / 60.0,
		lastRefill: now.Add(-9 * time.Minute), lastAccess: now.Add(-9 * time.Minute)}
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

func TestNew_CreatesLimiter(t *testing.T) {
	l := New()
	if l == nil {
		t.Fatal("New() returned nil")
	}
	if allowed, _ := l.Allow(1, 10); !allowed {
		t.Error("first request should be allowed")
	}
}

func TestEffectiveLimit(t *testing.T) {
	limits := Limits{EndUserPerMin: 30, ServicePerMin: 300}
	override := 45

	cases := []struct {
		name             string
		override         *int
		allowanceManaged bool
		want             int
	}{
		{"end-user default", nil, true, 30},
		{"service default", nil, false, 300},
		{"end-user override", &override, true, 45},
		{"service override", &override, false, 45},
	}
	for _, tc := range cases {
		if got := EffectiveLimit(tc.override, tc.allowanceManaged, limits); got != tc.want {
			t.Errorf("%s: expected %d, got %d", tc.name, tc.want, got)
		}
	}
}

package authlimit

import (
	"strings"
	"testing"
	"time"
)

// testClock returns a controllable clock starting at a fixed instant.
func testClock() (func() time.Time, *time.Time) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }, &now
}

func TestKeyedLimiter_AllowsUpToCapacity(t *testing.T) {
	nowFn, _ := testClock()
	l := newKeyedLimiter(5, nowFn)

	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	ok, retryAfter := l.allow("1.2.3.4")
	if ok {
		t.Fatal("6th request in same instant should be denied")
	}
	if retryAfter <= 0 || retryAfter > 12.0 {
		t.Errorf("retryAfter = %v, want in (0, 12]", retryAfter)
	}
}

func TestKeyedLimiter_RefillsOverTime(t *testing.T) {
	nowFn, now := testClock()
	l := newKeyedLimiter(5, nowFn) // 5/min -> 1 token per 12s

	for i := 0; i < 5; i++ {
		l.allow("k")
	}
	if ok, _ := l.allow("k"); ok {
		t.Fatal("bucket should be empty")
	}

	*now = now.Add(12 * time.Second)
	if ok, _ := l.allow("k"); !ok {
		t.Fatal("one token should have refilled after 12s")
	}
	if ok, _ := l.allow("k"); ok {
		t.Fatal("only one token should have refilled")
	}
}

func TestKeyedLimiter_KeysAreIndependent(t *testing.T) {
	nowFn, _ := testClock()
	l := newKeyedLimiter(1, nowFn)

	l.allow("a")
	if ok, _ := l.allow("a"); ok {
		t.Fatal("key a should be exhausted")
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("key b should be unaffected by key a")
	}
}

func TestKeyedLimiter_PruneEvictsIdleBuckets(t *testing.T) {
	nowFn, now := testClock()
	l := newKeyedLimiter(5, nowFn)

	l.allow("stale")
	*now = now.Add(11 * time.Minute)
	l.allow("fresh")

	l.prune(10 * time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.buckets["stale"]; exists {
		t.Error("stale bucket should have been pruned")
	}
	if _, exists := l.buckets["fresh"]; !exists {
		t.Error("fresh bucket should have survived the prune")
	}
}

func TestKeyedLimiter_ZeroRateDisablesLimiting(t *testing.T) {
	nowFn, _ := testClock()
	l := newKeyedLimiter(0, nowFn)
	for i := 0; i < 100; i++ {
		if ok, _ := l.allow("k"); !ok {
			t.Fatal("disabled limiter must always allow")
		}
	}
}

func TestGuard_NilIsSafeAndAllows(t *testing.T) {
	var g *Guard

	if ok, _ := g.AllowLoginIP("1.2.3.4"); !ok {
		t.Error("nil guard must allow login by IP")
	}
	if ok, _ := g.AllowLoginEmail("a@b.c"); !ok {
		t.Error("nil guard must allow login by email")
	}
	if ok, _ := g.AllowRegisterIP("1.2.3.4"); !ok {
		t.Error("nil guard must allow register")
	}
	if ok, _ := g.AllowGeneralIP("1.2.3.4"); !ok {
		t.Error("nil guard must allow general requests")
	}
	if !g.TryAcquireBcrypt() {
		t.Error("nil guard must grant bcrypt slots")
	}
	g.ReleaseBcrypt() // must not panic
}

func TestGuard_BcryptSemaphoreCapsConcurrency(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{BcryptConcurrency: 2}, nowFn)

	if !g.TryAcquireBcrypt() || !g.TryAcquireBcrypt() {
		t.Fatal("first two acquires should succeed")
	}
	if g.TryAcquireBcrypt() {
		t.Fatal("third acquire should fail at concurrency 2")
	}
	g.ReleaseBcrypt()
	if !g.TryAcquireBcrypt() {
		t.Fatal("acquire should succeed after a release")
	}
}

func TestGuard_ZeroBcryptConcurrencyMeansNoCap(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{}, nowFn)
	for i := 0; i < 50; i++ {
		if !g.TryAcquireBcrypt() {
			t.Fatal("uncapped guard must always grant bcrypt slots")
		}
	}
}

func TestGuard_LoginEmailIsCaseInsensitive(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinEmail: 1}, nowFn)

	g.AllowLoginEmail("User@Example.COM")
	if ok, _ := g.AllowLoginEmail("user@example.com"); ok {
		t.Fatal("email buckets must be case-insensitive")
	}
}

func TestGuard_EmailKeysAreBoundedInMemory(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinEmail: 5}, nowFn)

	huge := strings.Repeat("x", 1<<20) + "@example.com" // 1MB "email"
	g.AllowLoginEmail(huge)

	g.loginEmail.mu.Lock()
	defer g.loginEmail.mu.Unlock()
	if len(g.loginEmail.buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(g.loginEmail.buckets))
	}
	for key := range g.loginEmail.buckets {
		if len(key) > 64 {
			t.Errorf("bucket key length = %d, want <= 64 (hashed)", len(key))
		}
	}
}

func TestGuard_LimitersAreSeparate(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinIP: 1, RegisterPerMinIP: 1, GeneralPerMinIP: 1}, nowFn)

	g.AllowLoginIP("ip")
	if ok, _ := g.AllowLoginIP("ip"); ok {
		t.Fatal("login bucket should be exhausted")
	}
	if ok, _ := g.AllowRegisterIP("ip"); !ok {
		t.Fatal("register bucket must be independent of login bucket")
	}
	if ok, _ := g.AllowGeneralIP("ip"); !ok {
		t.Fatal("general bucket must be independent of login bucket")
	}
}

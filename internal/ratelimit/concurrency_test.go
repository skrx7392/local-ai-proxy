package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// Per-account concurrency cap (docs/design/per-account-rate-limiting.md §3.2
// step 3, phase D).

func TestConcurrency_TryAcquireRelease(t *testing.T) {
	c := NewConcurrency()

	if !c.TryAcquire(1, 2) || !c.TryAcquire(1, 2) {
		t.Fatal("first two acquires should succeed")
	}
	if c.TryAcquire(1, 2) {
		t.Error("third acquire above cap must fail")
	}
	// Independent accounts.
	if !c.TryAcquire(2, 2) {
		t.Error("other account must be unaffected")
	}

	c.Release(1)
	if !c.TryAcquire(1, 2) {
		t.Error("released slot should be reusable")
	}
}

func TestConcurrency_EntriesDeletedAtZero(t *testing.T) {
	c := NewConcurrency()
	c.TryAcquire(1, 5)
	c.Release(1)
	c.mu.Lock()
	_, exists := c.inUse[1]
	c.mu.Unlock()
	if exists {
		t.Error("entry must be deleted at zero (idle accounts cost nothing)")
	}
}

func TestConcurrency_ReleaseNeverGoesNegative(t *testing.T) {
	c := NewConcurrency()
	c.Release(1) // no acquire — must not panic or create state
	c.Release(1)
	if !c.TryAcquire(1, 1) {
		t.Error("acquire after spurious releases should succeed")
	}
	if c.TryAcquire(1, 1) {
		t.Error("spurious releases must not have minted extra capacity")
	}
}

func TestConcurrency_ZeroCapMeansNoCap(t *testing.T) {
	c := NewConcurrency()
	for i := 0; i < 50; i++ {
		if !c.TryAcquire(1, 0) {
			t.Fatal("cap <= 0 (unconfigured) must never reject")
		}
	}
	if c.InFlight() != 0 {
		t.Errorf("uncapped acquires must not track slots, got %d", c.InFlight())
	}
}

// blockingChain builds the middleware with concurrency caps and a next
// handler that blocks until released, so tests can hold slots open.
func blockingChain(limits Limits, m *metrics.Metrics) (http.Handler, chan struct{}, *ConcurrencyLimiter) {
	clock := newTestClock()
	release := make(chan struct{})
	conc := NewConcurrency()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(NewWithClock(clock.now), NewWithClock(clock.now), conc, limits, m)(next)
	return mw, release, conc
}

// waitFor spins (with scheduling yields) until cond holds or the deadline
// passes — only used to wait for goroutine scheduling, never for refill.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within deadline")
		}
		runtime.Gosched()
	}
}

func endUserReq(method string, key *store.APIKey, accountID int64) *http.Request {
	req := httptest.NewRequest(method, "/api/v1/chat/completions", nil)
	if method == http.MethodGet {
		req = httptest.NewRequest(method, "/api/v1/models", nil)
	}
	ctx := auth.WithKey(req.Context(), key)
	ctx = billing.WithResolution(ctx, billing.Resolution{AccountID: accountID, AllowanceManaged: true})
	return req.WithContext(ctx)
}

func TestMiddleware_ConcurrencyCapRejects(t *testing.T) {
	m := metrics.New(func() int { return 0 })
	limits := Limits{EndUserPerMin: 100, ServicePerMin: 300, EndUserMaxConcurrent: 1, ServiceMaxConcurrent: 8}
	mw, release, _ := blockingChain(limits, m)
	key := &store.APIKey{ID: 1, RateLimit: 1000}

	// Hold one slot open.
	first := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, endUserReq(http.MethodPost, key, 7))
		first <- rec
	}()
	// Wait until the in-flight request holds its slot.
	waitFor(t, func() bool { return testutil.ToFloat64(m.StreamsInflight) == 1 })

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, endUserReq(http.MethodPost, key, 7))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent request must 429, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "concurrent requests for your account (max 1)") {
		t.Errorf("expected concurrency-scope message, got: %s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "5" {
		t.Errorf("expected advisory Retry-After 5, got %q", rec.Header().Get("Retry-After"))
	}
	if got := testutil.ToFloat64(m.AccountRateLimitRejects.WithLabelValues("concurrency", "enduser")); got != 1 {
		t.Errorf("expected 1 concurrency reject metric, got %v", got)
	}

	close(release)
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("held request should complete 200, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.StreamsInflight); got != 0 {
		t.Errorf("gauge must return to 0 after release, got %v", got)
	}

	// Slot released on normal return: next request passes.
	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, endUserReq(http.MethodPost, key, 7))
	if rec2.Code != http.StatusOK {
		t.Errorf("slot must be reusable after release, got %d", rec2.Code)
	}
}

// A concurrency reject deliberately does NOT refund the account rate token —
// a free retry would let a client busy-poll the semaphore at zero cost.
func TestMiddleware_ConcurrencyRejectKeepsRateToken(t *testing.T) {
	limits := Limits{EndUserPerMin: 5, ServicePerMin: 300, EndUserMaxConcurrent: 1, ServiceMaxConcurrent: 8}
	clock := newTestClock()
	accounts := NewWithClock(clock.now)
	conc := NewConcurrency()
	release := make(chan struct{})
	started := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(NewWithClock(clock.now), accounts, conc, limits, nil)(next)
	key := &store.APIKey{ID: 1, RateLimit: 1000}

	done := make(chan struct{})
	go func() {
		mw.ServeHTTP(httptest.NewRecorder(), endUserReq(http.MethodPost, key, 7))
		close(done)
	}()
	<-started

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, endUserReq(http.MethodPost, key, 7))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected concurrency 429, got %d", rec.Code)
	}

	accounts.mu.Lock()
	tokens := accounts.buckets[7].tokens
	accounts.mu.Unlock()
	// 5 capacity − 1 (held request) − 1 (rejected request, NOT refunded) = 3.
	if tokens != 3 {
		t.Errorf("concurrency reject must consume the rate token: expected 3, got %f", tokens)
	}

	close(release)
	<-done
}

func TestMiddleware_GETsNeverTakeSlots(t *testing.T) {
	m := metrics.New(func() int { return 0 })
	limits := Limits{EndUserPerMin: 100, ServicePerMin: 300, EndUserMaxConcurrent: 1, ServiceMaxConcurrent: 8}
	mw, release, conc := blockingChain(limits, m)
	key := &store.APIKey{ID: 1, RateLimit: 1000}

	// Hold the only slot with a POST.
	postDone := make(chan struct{})
	go func() {
		mw.ServeHTTP(httptest.NewRecorder(), endUserReq(http.MethodPost, key, 7))
		close(postDone)
	}()
	waitFor(t, func() bool { return testutil.ToFloat64(m.StreamsInflight) == 1 })

	// GET must pass despite the saturated slot — but it blocks on the same
	// next handler, so run it concurrently and release both.
	got := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, endUserReq(http.MethodGet, key, 7))
		got <- rec.Code
	}()
	// The GET completing 200 proves it passed the concurrency gate while
	// the POST held the only slot; the gauge never exceeding 1 proves it
	// took no slot of its own.
	close(release)
	if code := <-got; code != http.StatusOK {
		t.Fatalf("GET must bypass the concurrency cap, got %d", code)
	}
	<-postDone
	if n := conc.InFlight(); n != 0 {
		t.Errorf("expected all slots released, got %d", n)
	}
}

// A leaked slot silently strangles an account until pod restart — the defer
// must release on handler panic.
func TestMiddleware_SlotReleasedOnPanic(t *testing.T) {
	clock := newTestClock()
	conc := NewConcurrency()
	limits := Limits{EndUserPerMin: 100, ServicePerMin: 300, EndUserMaxConcurrent: 1, ServiceMaxConcurrent: 8}
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	mw := Middleware(NewWithClock(clock.now), NewWithClock(clock.now), conc, limits, nil)(panicking)
	key := &store.APIKey{ID: 1, RateLimit: 1000}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the handler panic to propagate")
			}
		}()
		mw.ServeHTTP(httptest.NewRecorder(), endUserReq(http.MethodPost, key, 7))
	}()

	if !conc.TryAcquire(7, 1) {
		t.Error("slot must be released after a handler panic")
	}
	conc.Release(7)
}

func TestMiddleware_NilConcurrencyLimiterSkipsCap(t *testing.T) {
	clock := newTestClock()
	limits := Limits{EndUserPerMin: 100, ServicePerMin: 300, EndUserMaxConcurrent: 1, ServiceMaxConcurrent: 1}
	mw := Middleware(NewWithClock(clock.now), NewWithClock(clock.now), nil, limits, nil)(okNext())
	key := &store.APIKey{ID: 1, RateLimit: 1000}

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, endUserReq(http.MethodPost, key, 7))
	if rec.Code != http.StatusOK {
		t.Errorf("nil concurrency limiter must pass through, got %d", rec.Code)
	}
}

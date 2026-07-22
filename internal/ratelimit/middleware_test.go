package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

var testLimits = Limits{EndUserPerMin: 30, ServicePerMin: 300}

// serveChain sends one request through the middleware, optionally attaching
// an auth key and a billing resolution to the context.
func serveChain(t *testing.T, mw http.Handler, key *store.APIKey, res *billing.Resolution) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
	ctx := req.Context()
	if key != nil {
		ctx = auth.WithKey(ctx, key)
	}
	if res != nil {
		ctx = billing.WithResolution(ctx, *res)
	}
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func okNext() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func newMiddleware(limits Limits, m *metrics.Metrics) (http.Handler, *Limiter, *Limiter) {
	clock := newTestClock()
	keys := NewWithClock(clock.now)
	accounts := NewWithClock(clock.now)
	return Middleware(keys, accounts, NewConcurrency(), limits, m)(okNext()), keys, accounts
}

func TestMiddleware_NoKeyInContext(t *testing.T) {
	mw, _, _ := newMiddleware(testLimits, nil)
	rec := serveChain(t, mw, nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 pass-through without key, got %d", rec.Code)
	}
}

func TestMiddleware_KeyOnlyWhenNoResolution(t *testing.T) {
	mw, _, accounts := newMiddleware(testLimits, nil)
	key := &store.APIKey{ID: 1, RateLimit: 2}

	for i := 0; i < 2; i++ {
		if rec := serveChain(t, mw, key, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d should pass, got %d", i+1, rec.Code)
		}
	}
	rec := serveChain(t, mw, key, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "API key") {
		t.Errorf("key-scope 429 must name the API key, got: %s", rec.Body.String())
	}

	// No billing resolution → the account limiter must stay untouched.
	accounts.mu.Lock()
	n := len(accounts.buckets)
	accounts.mu.Unlock()
	if n != 0 {
		t.Errorf("account limiter must be untouched without a resolution, has %d buckets", n)
	}
}

func TestMiddleware_EndUserDefaultApplied(t *testing.T) {
	mw, _, _ := newMiddleware(Limits{EndUserPerMin: 2, ServicePerMin: 300}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1000}
	res := &billing.Resolution{AccountID: 7, AllowanceManaged: true}

	for i := 0; i < 2; i++ {
		if rec := serveChain(t, mw, key, res); rec.Code != http.StatusOK {
			t.Fatalf("request %d should pass, got %d", i+1, rec.Code)
		}
	}
	rec := serveChain(t, mw, key, res)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 at the end-user default, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "your account (2 req/min)") {
		t.Errorf("account-scope 429 must state the account limit, got: %s", rec.Body.String())
	}
}

func TestMiddleware_ServiceDefaultApplied(t *testing.T) {
	mw, _, _ := newMiddleware(Limits{EndUserPerMin: 300, ServicePerMin: 2}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1000}
	res := &billing.Resolution{AccountID: 8, AllowanceManaged: false}

	for i := 0; i < 2; i++ {
		if rec := serveChain(t, mw, key, res); rec.Code != http.StatusOK {
			t.Fatalf("request %d should pass, got %d", i+1, rec.Code)
		}
	}
	if rec := serveChain(t, mw, key, res); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 at the service default, got %d", rec.Code)
	}
}

func TestMiddleware_OverridePrecedence(t *testing.T) {
	mw, _, _ := newMiddleware(Limits{EndUserPerMin: 300, ServicePerMin: 300}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1000}
	override := 1
	res := &billing.Resolution{AccountID: 9, AllowanceManaged: true, RateLimitPerMin: &override}

	if rec := serveChain(t, mw, key, res); rec.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", rec.Code)
	}
	rec := serveChain(t, mw, key, res)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("override of 1/min must beat the class default, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "(1 req/min)") {
		t.Errorf("429 must state the effective (override) limit, got: %s", rec.Body.String())
	}
}

// The shared-key fix: exhausting one end user's bucket must not affect
// another user riding the same key.
func TestMiddleware_AccountIsolation(t *testing.T) {
	mw, _, _ := newMiddleware(Limits{EndUserPerMin: 2, ServicePerMin: 300}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1000}
	userA := &billing.Resolution{AccountID: 100, AllowanceManaged: true}
	userB := &billing.Resolution{AccountID: 101, AllowanceManaged: true}

	serveChain(t, mw, key, userA)
	serveChain(t, mw, key, userA)
	if rec := serveChain(t, mw, key, userA); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("user A should be throttled, got %d", rec.Code)
	}
	if rec := serveChain(t, mw, key, userB); rec.Code != http.StatusOK {
		t.Errorf("user B must be unaffected by user A's throttle, got %d", rec.Code)
	}
}

// Account-first ordering: an account-level reject must not consume a key
// token (a throttled user must never drain the shared aggregate bucket).
func TestMiddleware_AccountRejectDoesNotChargeKeyBucket(t *testing.T) {
	mw, keys, _ := newMiddleware(Limits{EndUserPerMin: 1, ServicePerMin: 300}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 5}
	res := &billing.Resolution{AccountID: 100, AllowanceManaged: true}

	serveChain(t, mw, key, res) // passes; key tokens 5→4
	for i := 0; i < 3; i++ {
		if rec := serveChain(t, mw, key, res); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("account reject expected, got %d", rec.Code)
		}
	}

	keys.mu.Lock()
	b := keys.buckets[key.ID]
	tokens := b.tokens
	keys.mu.Unlock()
	if tokens != 4 {
		t.Errorf("account rejects must not charge the key bucket: expected 4 tokens, got %f", tokens)
	}
}

// Key-level reject refunds the account token: on the shared key the two
// buckets have different owners, so aggregate saturation must not burn the
// individual user's budget.
func TestMiddleware_KeyRejectRefundsAccountToken(t *testing.T) {
	mw, _, accounts := newMiddleware(Limits{EndUserPerMin: 5, ServicePerMin: 300}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1}
	res := &billing.Resolution{AccountID: 100, AllowanceManaged: true}

	if rec := serveChain(t, mw, key, res); rec.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", rec.Code)
	}
	rec := serveChain(t, mw, key, res)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request should hit the key limit, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "API key") {
		t.Errorf("expected key-scope message, got: %s", rec.Body.String())
	}

	accounts.mu.Lock()
	tokens := accounts.buckets[res.AccountID].tokens
	accounts.mu.Unlock()
	// 5 capacity − 1 (request 1) − 1 (request 2) + 1 (refund) = 4.
	if tokens != 4 {
		t.Errorf("key reject must refund the account token: expected 4, got %f", tokens)
	}
}

func TestMiddleware_KeyLevel429Shape(t *testing.T) {
	mw, _, _ := newMiddleware(testLimits, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1}

	serveChain(t, mw, key, nil)
	rec := serveChain(t, mw, key, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

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
	if errObj["type"] != "rate_limit_exceeded" {
		t.Errorf("expected type 'rate_limit_exceeded', got %v", errObj["type"])
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("expected JSON Content-Type, got %q", ct)
	}
}

func TestMiddleware_AccountLevel429ShapeAndRetryAfter(t *testing.T) {
	mw, _, _ := newMiddleware(Limits{EndUserPerMin: 1, ServicePerMin: 300}, nil)
	key := &store.APIKey{ID: 1, RateLimit: 1000}
	res := &billing.Resolution{AccountID: 7, AllowanceManaged: true}

	serveChain(t, mw, key, res)
	rec := serveChain(t, mw, key, res)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	errObj := body["error"].(map[string]any)
	// Same code/type as the key gate — SDK parsers key on these — but a
	// scope-differentiated human message.
	if errObj["code"] != "rate_limit_exceeded" {
		t.Errorf("expected code 'rate_limit_exceeded', got %v", errObj["code"])
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" || ra == "0" {
		t.Errorf("Retry-After must be at least 1s, got %q", ra)
	}
}

func TestMiddleware_MetricsLabels(t *testing.T) {
	m := metrics.New(func() int { return 0 })
	mw, _, _ := newMiddleware(Limits{EndUserPerMin: 1, ServicePerMin: 1}, m)

	// End-user account reject.
	key := &store.APIKey{ID: 1, RateLimit: 1000}
	endUser := &billing.Resolution{AccountID: 1, AllowanceManaged: true}
	serveChain(t, mw, key, endUser)
	serveChain(t, mw, key, endUser)
	if got := testutil.ToFloat64(m.AccountRateLimitRejects.WithLabelValues("rate", "enduser")); got != 1 {
		t.Errorf("expected 1 enduser rate reject, got %v", got)
	}

	// Service account reject.
	service := &billing.Resolution{AccountID: 2, AllowanceManaged: false}
	serveChain(t, mw, key, service)
	serveChain(t, mw, key, service)
	if got := testutil.ToFloat64(m.AccountRateLimitRejects.WithLabelValues("rate", "service")); got != 1 {
		t.Errorf("expected 1 service rate reject, got %v", got)
	}

	// Key-level reject stays in the legacy counter, not the account one.
	tightKey := &store.APIKey{ID: 9, RateLimit: 1}
	serveChain(t, mw, tightKey, nil)
	serveChain(t, mw, tightKey, nil)
	if got := testutil.ToFloat64(m.RateLimitRejects); got != 1 {
		t.Errorf("expected 1 key-level reject in legacy counter, got %v", got)
	}
	if got := testutil.ToFloat64(m.AccountRateLimitRejects.WithLabelValues("rate", "enduser")) +
		testutil.ToFloat64(m.AccountRateLimitRejects.WithLabelValues("rate", "service")); got != 2 {
		t.Errorf("key-level reject must not increment the account counter, got %v", got)
	}
}

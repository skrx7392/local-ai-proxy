package authlimit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/metrics"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func doRequest(h http.Handler, method, path, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Forwarded-For", ip)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_LoginThrottledPerIP(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinIP: 2, RegisterPerMinIP: 3, GeneralPerMinIP: 100}, nowFn)
	h := Middleware(g, nil)(okHandler())

	for i := 0; i < 2; i++ {
		if rec := doRequest(h, "POST", "/api/auth/login", "203.0.113.7"); rec.Code != http.StatusOK {
			t.Fatalf("login %d: got %d, want 200", i+1, rec.Code)
		}
	}

	rec := doRequest(h, "POST", "/api/auth/login", "203.0.113.7")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd login: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response must set Retry-After")
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body is not valid JSON: %v", err)
	}
	if body.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error code = %q, want rate_limit_exceeded", body.Error.Code)
	}

	// A different client IP is unaffected.
	if rec := doRequest(h, "POST", "/api/auth/login", "198.51.100.1"); rec.Code != http.StatusOK {
		t.Errorf("different IP: got %d, want 200", rec.Code)
	}
}

func TestMiddleware_RegisterRoutesShareStricterBucket(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinIP: 100, RegisterPerMinIP: 2, GeneralPerMinIP: 100}, nowFn)
	h := Middleware(g, nil)(okHandler())

	if rec := doRequest(h, "POST", "/api/auth/register", "203.0.113.7"); rec.Code != http.StatusOK {
		t.Fatalf("user register: got %d, want 200", rec.Code)
	}
	if rec := doRequest(h, "POST", "/api/accounts/register", "203.0.113.7"); rec.Code != http.StatusOK {
		t.Fatalf("service register: got %d, want 200", rec.Code)
	}
	// Both register endpoints drain the same per-IP bucket.
	if rec := doRequest(h, "POST", "/api/auth/register", "203.0.113.7"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd register from same IP: got %d, want 429", rec.Code)
	}
}

func TestMiddleware_OtherRoutesUseGeneralBucket(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinIP: 1, RegisterPerMinIP: 1, GeneralPerMinIP: 3}, nowFn)
	h := Middleware(g, nil)(okHandler())

	for i := 0; i < 3; i++ {
		if rec := doRequest(h, "GET", "/api/users/profile", "203.0.113.7"); rec.Code != http.StatusOK {
			t.Fatalf("profile %d: got %d, want 200", i+1, rec.Code)
		}
	}
	if rec := doRequest(h, "GET", "/api/users/profile", "203.0.113.7"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th profile: got %d, want 429", rec.Code)
	}
}

func TestMiddleware_RefillRestoresAccess(t *testing.T) {
	nowFn, now := testClock()
	g := NewWithClock(Config{LoginPerMinIP: 1}, nowFn)
	h := Middleware(g, nil)(okHandler())

	doRequest(h, "POST", "/api/auth/login", "203.0.113.7")
	if rec := doRequest(h, "POST", "/api/auth/login", "203.0.113.7"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd login: got %d, want 429", rec.Code)
	}

	*now = now.Add(61 * time.Second)
	if rec := doRequest(h, "POST", "/api/auth/login", "203.0.113.7"); rec.Code != http.StatusOK {
		t.Fatalf("login after refill window: got %d, want 200", rec.Code)
	}
}

func TestMiddleware_RejectionIncrementsMetric(t *testing.T) {
	nowFn, _ := testClock()
	g := NewWithClock(Config{LoginPerMinIP: 1}, nowFn)
	m := metrics.New(func() int { return 0 })
	h := Middleware(g, m)(okHandler())

	doRequest(h, "POST", "/api/auth/login", "203.0.113.7")
	doRequest(h, "POST", "/api/auth/login", "203.0.113.7") // rejected

	if got := testutil.ToFloat64(m.RateLimitRejects); got != 1 {
		t.Errorf("RateLimitRejects = %v, want 1", got)
	}
}

func TestMiddleware_NilGuardPassesThrough(t *testing.T) {
	h := Middleware(nil, nil)(okHandler())
	for i := 0; i < 20; i++ {
		if rec := doRequest(h, "POST", "/api/auth/login", "203.0.113.7"); rec.Code != http.StatusOK {
			t.Fatalf("nil guard must never limit, got %d", rec.Code)
		}
	}
}

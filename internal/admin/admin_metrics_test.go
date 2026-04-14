package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// setupAdminMetricsTest mirrors setupAdminTest but wires a fresh *Metrics into
// Options so we can assert counter increments on admin-auth rejection paths.
func setupAdminMetricsTest(t *testing.T) (http.Handler, *store.Store, *metrics.Metrics) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping admin metrics integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	wipe := func(p *pgxpool.Pool) {
		c := context.Background()
		_, _ = p.Exec(c, "DELETE FROM registration_events")
		_, _ = p.Exec(c, "DELETE FROM credit_holds")
		_, _ = p.Exec(c, "DELETE FROM credit_transactions")
		_, _ = p.Exec(c, "DELETE FROM account_usage_stats")
		_, _ = p.Exec(c, "DELETE FROM credit_balances")
		_, _ = p.Exec(c, "DELETE FROM credit_pricing")
		_, _ = p.Exec(c, "DELETE FROM registration_tokens")
		_, _ = p.Exec(c, "DELETE FROM usage_logs")
		_, _ = p.Exec(c, "DELETE FROM user_sessions")
		_, _ = p.Exec(c, "DELETE FROM api_keys")
		_, _ = p.Exec(c, "DELETE FROM users")
		_, _ = p.Exec(c, "DELETE FROM accounts")
	}

	pool := s.Pool()
	wipe(pool)
	t.Cleanup(func() {
		wipe(s.Pool())
		s.Close()
	})

	m := metrics.New(func() int { return 0 })
	usageCh := make(chan store.UsageEntry, 100)
	h := NewHandler(s, testAdminKey, usageCh, Options{Metrics: m})
	return h, s, m
}

func TestAdminAuth_InvalidAdminKey_IncrementsCounter(t *testing.T) {
	h, _, m := setupAdminMetricsTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("X-Admin-Key", "wrong-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues("invalid_admin_key")); got != 1 {
		t.Errorf("invalid_admin_key counter = %v, want 1", got)
	}
}

func TestAdminAuth_MissingCredentials_IncrementsCounter(t *testing.T) {
	h, _, m := setupAdminMetricsTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues("missing_credentials")); got != 1 {
		t.Errorf("missing_credentials counter = %v, want 1", got)
	}
}

func TestAdminAuth_InvalidSession_IncrementsCounter(t *testing.T) {
	h, _, m := setupAdminMetricsTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer nonexistent-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues("invalid_session")); got != 1 {
		t.Errorf("invalid_session counter = %v, want 1", got)
	}
}

func TestAdminAuth_SessionExpired_IncrementsCounter(t *testing.T) {
	h, s, m := setupAdminMetricsTest(t)

	token := createSession(t, s, "expired", "admin", -1*time.Minute, true)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues("session_expired")); got != 1 {
		t.Errorf("session_expired counter = %v, want 1", got)
	}
}

func TestAdminAuth_AccountDisabled_IncrementsCounter(t *testing.T) {
	h, s, m := setupAdminMetricsTest(t)

	token := createSession(t, s, "disabled", "admin", time.Hour, false)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues("account_disabled")); got != 1 {
		t.Errorf("account_disabled counter = %v, want 1", got)
	}
}

func TestAdminAuth_NotAdmin_IncrementsCounter(t *testing.T) {
	h, s, m := setupAdminMetricsTest(t)

	token := createSession(t, s, "plainuser", "user", time.Hour, true)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues("not_admin")); got != 1 {
		t.Errorf("not_admin counter = %v, want 1", got)
	}
}

func TestAdminAuth_RateLimit429_DoesNotIncrementAuthFailureCounter(t *testing.T) {
	// 429 rate-limit rejections are counted separately in ratelimit_rejects_total;
	// they must not pollute the admin_auth_failures histogram.
	h, _, m := setupAdminMetricsTest(t)

	// Burn through the X-Admin-Key bucket (10 req/min).
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
		req.Header.Set("X-Admin-Key", testAdminKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	// Sum every failure label — must be 0.
	labels := []string{"invalid_admin_key", "missing_credentials", "invalid_session", "session_expired", "user_not_found", "account_disabled", "not_admin"}
	var total float64
	for _, l := range labels {
		total += testutil.ToFloat64(m.AdminAuthFailures.WithLabelValues(l))
	}
	if total != 0 {
		t.Errorf("admin_auth_failures incremented for rate-limit path (total=%v)", total)
	}
}

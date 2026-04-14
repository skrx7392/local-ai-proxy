package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/krishna/local-ai-proxy/internal/health"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// setupAdminWithObservability mirrors setupAdminTest but threads in the BE 5
// dependencies. usageDepth/usageCap let a test force the usage_writer probe
// into the "full" state.
func setupAdminWithObservability(
	t *testing.T,
	snap ConfigSnapshot,
	startTime time.Time,
	ollamaURL string,
	usageDepth, usageCap int,
) http.Handler {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping admin integration test")
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
	wipe(s.Pool())
	t.Cleanup(func() {
		wipe(s.Pool())
		s.Close()
	})

	usageCh := make(chan store.UsageEntry, 100)
	checker := health.NewChecker(s, ollamaURL, func() int { return usageDepth }, usageCap)

	return NewHandler(s, testAdminKey, usageCh, Options{
		Snapshot:  snap,
		Checker:   checker,
		StartTime: startTime,
	})
}

func defaultSnap() ConfigSnapshot {
	return ConfigSnapshot{
		OllamaURL:               "http://ollama.local:11434",
		Port:                    "8080",
		LogLevel:                "info",
		MaxRequestBodyBytes:     52428800,
		DefaultCreditGrant:      1.5,
		CORSOrigins:             "*",
		AdminRateLimitPerMinute: 10,
		UsageChannelCapacity:    1000,
		AdminSessionDurationHrs: 6,
		UserSessionDurationHrs:  168,
		Version:                 "test-version",
		BuildTime:               "2026-04-14T00:00:00Z",
		GoVersion:               runtime.Version(),
	}
}

// stubOllama returns a 200-OK HEAD-friendly server so the ollama check stays
// green during config/health tests. Callers Close() it via t.Cleanup.
func stubOllama(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestConfig_RequiresAuth(t *testing.T) {
	snap := defaultSnap()
	h := setupAdminWithObservability(t, snap, time.Now(), stubOllama(t), 0, 1000)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without admin key, got %d", rec.Code)
	}
}

func TestConfig_ReturnsWhitelistedFields(t *testing.T) {
	snap := defaultSnap()
	h := setupAdminWithObservability(t, snap, time.Now(), stubOllama(t), 0, 1000)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	expected := map[string]any{
		"ollama_url":                   snap.OllamaURL,
		"port":                         snap.Port,
		"log_level":                    snap.LogLevel,
		"max_request_body_bytes":       float64(snap.MaxRequestBodyBytes),
		"default_credit_grant":         snap.DefaultCreditGrant,
		"cors_origins":                 snap.CORSOrigins,
		"admin_rate_limit_per_minute":  float64(snap.AdminRateLimitPerMinute),
		"usage_channel_capacity":       float64(snap.UsageChannelCapacity),
		"admin_session_duration_hours": float64(snap.AdminSessionDurationHrs),
		"user_session_duration_hours":  float64(snap.UserSessionDurationHrs),
		"version":                      snap.Version,
		"build_time":                   snap.BuildTime,
		"go_version":                   snap.GoVersion,
	}
	for k, want := range expected {
		got, ok := body[k]
		if !ok {
			t.Errorf("missing field %q", k)
			continue
		}
		if got != want {
			t.Errorf("field %q: want %v (%T), got %v (%T)", k, want, want, got, got)
		}
	}

	for _, forbidden := range []string{
		"admin_key", "AdminKey",
		"database_url", "DatabaseURL",
		"admin_bootstrap_token", "AdminBootstrapToken",
	} {
		if _, present := body[forbidden]; present {
			t.Errorf("secret field %q must not be exposed", forbidden)
		}
	}
}

func TestHealth_RequiresAuth(t *testing.T) {
	h := setupAdminWithObservability(t, defaultSnap(), time.Now(), stubOllama(t), 0, 1000)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without admin key, got %d", rec.Code)
	}
}

func TestHealth_HappyPath(t *testing.T) {
	snap := defaultSnap()
	h := setupAdminWithObservability(t, snap, time.Now().Add(-90*time.Second), stubOllama(t), 0, 1000)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("status: want ok, got %v", body["status"])
	}
	if v, ok := body["version"].(string); !ok || v != snap.Version {
		t.Errorf("version: want %q, got %v", snap.Version, body["version"])
	}
	if v, ok := body["uptime_seconds"].(float64); !ok || v < 90 {
		t.Errorf("uptime_seconds: want >= 90, got %v", body["uptime_seconds"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("checks: expected object, got %T", body["checks"])
	}
	for _, k := range []string{"db", "ollama", "usage_writer"} {
		c, ok := checks[k].(map[string]any)
		if !ok {
			t.Errorf("missing check %q", k)
			continue
		}
		if c["status"] != "ok" {
			t.Errorf("check %q: want status ok, got %v", k, c["status"])
		}
	}

	// Pool stats stay in Prometheus per locked decision #7.
	for _, forbidden := range []string{"pool", "db_pool_total", "db_pool_idle", "db_pool_acquired"} {
		if _, present := body[forbidden]; present {
			t.Errorf("pool stats key %q must not appear in /admin/health (Prometheus only)", forbidden)
		}
	}
}

func TestHealth_DegradedWhenUsageWriterFull(t *testing.T) {
	h := setupAdminWithObservability(t, defaultSnap(), time.Now(), stubOllama(t), 1000, 1000)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when usage channel full, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "degraded" {
		t.Errorf("status: want degraded, got %v", body["status"])
	}
}

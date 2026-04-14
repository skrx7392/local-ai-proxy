package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func TestBootstrap_Success_IncrementsRegistrationCounter(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping bootstrap metrics integration test")
	}
	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	wipe := func(p *pgxpool.Pool) {
		c := context.Background()
		_, _ = p.Exec(c, "DELETE FROM registration_events")
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

	m := metrics.New(func() int { return 0 })
	h := New(s, testBootstrapToken, m)

	body, _ := json.Marshal(bootstrapRequest{
		Token:    testBootstrapToken,
		Email:    "bootstrap-metrics@example.com",
		Password: "strongpass123",
		Name:     "Bootstrap Metrics",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(m.Registrations.WithLabelValues("admin_bootstrap")); got != 1 {
		t.Errorf("admin_bootstrap counter = %v, want 1", got)
	}
}

func TestBootstrap_InvalidToken_DoesNotIncrement(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	m := metrics.New(func() int { return 0 })
	h := New(s, testBootstrapToken, m)

	body, _ := json.Marshal(bootstrapRequest{
		Token:    "wrong-token",
		Email:    "bad@example.com",
		Password: "strongpass123",
		Name:     "Bad",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.Registrations.WithLabelValues("admin_bootstrap")); got != 0 {
		t.Errorf("admin_bootstrap counter = %v, want 0 (auth failed)", got)
	}
}

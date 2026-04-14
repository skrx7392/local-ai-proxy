package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// setupUserMetricsTest wires a fresh *Metrics and returns handler + store + m.
func setupUserMetricsTest(t *testing.T) (http.Handler, *store.Store, *metrics.Metrics) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping user metrics integration test")
	}
	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	wipe := func() {
		c := context.Background()
		_, _ = s.Pool().Exec(c, "DELETE FROM registration_events")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_holds")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_transactions")
		_, _ = s.Pool().Exec(c, "DELETE FROM account_usage_stats")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_balances")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_pricing")
		_, _ = s.Pool().Exec(c, "DELETE FROM registration_tokens")
		_, _ = s.Pool().Exec(c, "DELETE FROM usage_logs")
		_, _ = s.Pool().Exec(c, "DELETE FROM user_sessions")
		_, _ = s.Pool().Exec(c, "DELETE FROM api_keys")
		_, _ = s.Pool().Exec(c, "DELETE FROM users")
		_, _ = s.Pool().Exec(c, "DELETE FROM accounts")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})

	m := metrics.New(func() int { return 0 })
	h := NewHandler(s, 0, m)
	return h, s, m
}

func TestRegister_IncrementsUserSignupCounter(t *testing.T) {
	h, _, m := setupUserMetricsTest(t)

	body, _ := json.Marshal(registerRequest{
		Email: "metrics-signup@example.com", Password: "strongpass123", Name: "Sign",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(m.Registrations.WithLabelValues("user_signup")); got != 1 {
		t.Errorf("user_signup counter = %v, want 1", got)
	}
}

func TestRegister_DuplicateEmail_DoesNotIncrement(t *testing.T) {
	h, _, m := setupUserMetricsTest(t)

	body, _ := json.Marshal(registerRequest{
		Email: "dup-metrics@example.com", Password: "strongpass123", Name: "Dup",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first register want 201, got %d", rec.Code)
	}

	// Second attempt with same email — must 409 and NOT increment again.
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second register want 409, got %d", rec2.Code)
	}
	if got := testutil.ToFloat64(m.Registrations.WithLabelValues("user_signup")); got != 1 {
		t.Errorf("user_signup counter = %v, want 1 after one success + one conflict", got)
	}
}

func TestRegisterServiceAccount_IncrementsServiceCounter(t *testing.T) {
	h, s, m := setupUserMetricsTest(t)

	// Seed a registration token so the service account endpoint can succeed.
	rawTok := "svc-reg-token-metrics"
	tokenHash := auth.HashKey(rawTok)
	if _, err := s.CreateRegistrationToken("svc-metrics", tokenHash, 10.0, 1, nil); err != nil {
		t.Fatalf("CreateRegistrationToken: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"registration_token": rawTok,
		"name":               "svc-metrics",
		"rate_limit":         60,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/register", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := testutil.ToFloat64(m.Registrations.WithLabelValues("service_registration")); got != 1 {
		t.Errorf("service_registration counter = %v, want 1", got)
	}
}

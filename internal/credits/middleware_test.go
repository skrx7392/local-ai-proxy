package credits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func setupTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		pool := s.Pool()
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_holds")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_transactions")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS account_usage_stats")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_balances")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_pricing")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS registration_tokens")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS usage_logs")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS user_sessions")
		_, _ = pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS user_id")
		_, _ = pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS account_id")
		_, _ = pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS session_token_limit")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS api_keys")
		_, _ = pool.Exec(context.Background(), "ALTER TABLE users DROP COLUMN IF EXISTS account_id")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS users")
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS accounts")
		s.Close()
	})

	pool := s.Pool()
	_, _ = pool.Exec(ctx, "DELETE FROM credit_holds")
	_, _ = pool.Exec(ctx, "DELETE FROM credit_transactions")
	_, _ = pool.Exec(ctx, "DELETE FROM account_usage_stats")
	_, _ = pool.Exec(ctx, "DELETE FROM credit_balances")
	_, _ = pool.Exec(ctx, "DELETE FROM credit_pricing")
	_, _ = pool.Exec(ctx, "DELETE FROM registration_tokens")
	_, _ = pool.Exec(ctx, "DELETE FROM usage_logs")
	_, _ = pool.Exec(ctx, "DELETE FROM user_sessions")
	_, _ = pool.Exec(ctx, "DELETE FROM api_keys")
	_, _ = pool.Exec(ctx, "DELETE FROM users")
	_, _ = pool.Exec(ctx, "DELETE FROM accounts")
	return s
}

func TestCreditGate_ErrorResponseFormat(t *testing.T) {
	// Verify that credit gate error responses are valid JSON with Content-Type header.
	// This replaces the old TestWriteJSONError after consolidating writeJSONError → apierror.WriteError.
	db := setupTestStore(t)
	accID, _, _ := db.RegisterUser("gate-format@example.com", "hash", "GateFormat")
	// No credits — will get 402
	gate := CreditGate(db, nil)

	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	key := &store.APIKey{ID: 1, AccountID: &accID}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("expected 402, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected application/json content type")
	}
}

func TestCreditGate_NilAccountID_PassesThrough(t *testing.T) {
	db := setupTestStore(t)
	gate := CreditGate(db, nil)

	called := false
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// Key without AccountID
	key := &store.APIKey{ID: 1, Name: "test"}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if !called {
		t.Error("expected handler to be called for nil AccountID")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestCreditGate_SufficientBalance_PassesThrough(t *testing.T) {
	db := setupTestStore(t)
	accID, _, _ := db.RegisterUser("gate-pass@example.com", "hash", "GatePass")
	_ = db.AddCredits(accID, 100, "grant")
	gate := CreditGate(db, nil)

	called := false
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	key := &store.APIKey{ID: 1, AccountID: &accID}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if !called {
		t.Error("expected handler to be called with sufficient balance")
	}
}

func TestCreditGate_ZeroBalance_Returns402(t *testing.T) {
	db := setupTestStore(t)
	accID, _, _ := db.RegisterUser("gate-zero@example.com", "hash", "GateZero")
	// No credits added — balance is 0
	gate := CreditGate(db, nil)

	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	key := &store.APIKey{ID: 1, AccountID: &accID}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("expected 402, got %d", rec.Code)
	}
}

func TestCreditGate_NoKey_PassesThrough(t *testing.T) {
	db := setupTestStore(t)
	gate := CreditGate(db, nil)

	called := false
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// No key in context at all
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if !called {
		t.Error("expected handler to be called when no key in context")
	}
}

func TestCreditGate_InactiveAccount_Returns403(t *testing.T) {
	db := setupTestStore(t)
	accID, _, _ := db.RegisterUser("gate-inactive@example.com", "hash", "GateInactive")
	_ = db.AddCredits(accID, 100, "grant")
	_ = db.SetAccountActive(accID, false)
	gate := CreditGate(db, nil)

	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	key := &store.APIKey{ID: 1, AccountID: &accID}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

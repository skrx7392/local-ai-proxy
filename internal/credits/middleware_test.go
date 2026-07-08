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
	wipe := func() {
		c := context.Background()
		pool := s.Pool()
		// DELETE rather than DROP so concurrent tests don't race on Postgres
		// catalog locks during migrate.
		_, _ = pool.Exec(c, "DELETE FROM registration_events")
		_, _ = pool.Exec(c, "DELETE FROM credit_holds")
		_, _ = pool.Exec(c, "DELETE FROM credit_transactions")
		_, _ = pool.Exec(c, "DELETE FROM account_usage_stats")
		_, _ = pool.Exec(c, "DELETE FROM credit_balances")
		_, _ = pool.Exec(c, "DELETE FROM credit_pricing")
		_, _ = pool.Exec(c, "DELETE FROM registration_tokens")
		_, _ = pool.Exec(c, "DELETE FROM usage_logs")
		_, _ = pool.Exec(c, "DELETE FROM user_sessions")
		_, _ = pool.Exec(c, "DELETE FROM api_keys")
		_, _ = pool.Exec(c, "DELETE FROM users")
		_, _ = pool.Exec(c, "DELETE FROM accounts")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})
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

func TestCreditGate_NilAccountID_Returns403(t *testing.T) {
	// The legacy NULL-account bypass is gone: a key without an account is
	// rejected, never waved through. Startup backfill guarantees this state
	// cannot occur in a healthy deployment, so rejecting is fail-closed.
	db := setupTestStore(t)
	gate := CreditGate(db, nil)

	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for nil AccountID")
	}))

	// Key without AccountID
	key := &store.APIKey{ID: 1, Name: "test"}
	req := httptest.NewRequest("GET", "/test", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected application/json content type")
	}
}

func TestCreditGate_LegacyAdminKey_UpgradePath(t *testing.T) {
	// Production upgrade scenario: a live admin-created key with NULL
	// account_id (pre-upgrade it chatted via the old bypass). After the
	// startup backfill it must chat again — attached to the admin service
	// account, metered like every other key.
	db := setupTestStore(t)
	gate := CreditGate(db, nil)

	rawHash := "legacy-hash-upgrade"
	if _, err := db.CreateKey("legacy-admin", rawHash, "sk-legacy", 60); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	serve := func() (int, bool) {
		called := false
		handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		key, err := db.GetKeyByHash(rawHash)
		if err != nil {
			t.Fatalf("GetKeyByHash: %v", err)
		}
		if key == nil {
			t.Fatal("expected key to exist")
		}
		req := httptest.NewRequest("GET", "/test", nil)
		req = req.WithContext(auth.WithKey(req.Context(), key))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, called
	}

	// Before the backfill runs, the gate fails closed.
	if code, called := serve(); code != http.StatusForbidden || called {
		t.Errorf("pre-backfill: expected 403 and handler not called, got %d called=%v", code, called)
	}

	// Startup migration: ensure the admin service account, attach legacy keys.
	adminAccID, err := db.EnsureAdminServiceAccount(1000)
	if err != nil {
		t.Fatalf("EnsureAdminServiceAccount: %v", err)
	}
	if _, err := db.BackfillAdminKeyAccounts(adminAccID); err != nil {
		t.Fatalf("BackfillAdminKeyAccounts: %v", err)
	}

	// The same key now passes the gate via the service account.
	if code, called := serve(); code != http.StatusOK || !called {
		t.Errorf("post-backfill: expected 200 and handler called, got %d called=%v", code, called)
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

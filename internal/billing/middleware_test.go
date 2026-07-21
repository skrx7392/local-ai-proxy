package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func setupBillingTest(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping billing integration test")
	}
	s, err := store.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	wipe := func() {
		c := context.Background()
		for _, table := range []string{
			"registration_events", "credit_holds", "credit_transactions",
			"account_usage_stats", "credit_balances", "usage_logs",
			"user_sessions", "api_keys", "users", "federated_identities", "accounts",
		} {
			_, _ = s.Pool().Exec(c, "DELETE FROM "+table)
		}
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})
	return s
}

// serve runs the middleware over a request carrying the given key and headers,
// and returns the captured billing resolution (nil if none) plus the recorder.
func serve(t *testing.T, db *store.Store, key *store.APIKey, headers map[string]string) (*Resolution, *httptest.ResponseRecorder) {
	t.Helper()
	var captured *Resolution
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if res, ok := FromContext(r.Context()); ok {
			captured = &res
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
	if key != nil {
		req = req.WithContext(auth.WithKey(req.Context(), key))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	Middleware(db, 5.0)(next).ServeHTTP(rec, req)
	return captured, rec
}

func owuiHeaders() map[string]string {
	return map[string]string{
		HeaderUserID:    "owui-user-1",
		HeaderUserEmail: "bob@example.com",
		HeaderUserName:  "Bob",
	}
}

func TestMiddleware_TrustedKeyWithHeaders_ResolvesEndUser(t *testing.T) {
	db := setupBillingTest(t)
	sharedAcc, _, _ := db.RegisterUser("shared@example.com", "hash", "Shared")

	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true}
	res, rec := serve(t, db, key, owuiHeaders())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if res == nil {
		t.Fatal("expected a billing resolution in context")
	}
	if !res.AllowanceManaged {
		t.Error("end-user resolution must be allowance-managed")
	}
	if res.AccountID == sharedAcc {
		t.Error("end user must NOT bill the shared key account")
	}

	// Provisioned with the default grant, ready to pass the credit gate.
	bal, err := db.GetCreditBalance(res.AccountID)
	if err != nil || bal == nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal.Balance != 5.0 {
		t.Errorf("expected allowance-funded balance 5.0, got %v", bal.Balance)
	}

	// Same identity again: same account (stable mapping).
	res2, _ := serve(t, db, key, owuiHeaders())
	if res2 == nil || res2.AccountID != res.AccountID {
		t.Errorf("expected stable account mapping, got %v then %v", res, res2)
	}
}

func TestMiddleware_UntrustedKeyWithHeaders_BillsKeyAccount(t *testing.T) {
	db := setupBillingTest(t)
	accID, _, _ := db.RegisterUser("untrusted@example.com", "hash", "Untrusted")

	key := &store.APIKey{ID: 1, AccountID: &accID, TrustUserHeaders: false}
	res, rec := serve(t, db, key, owuiHeaders())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if res == nil {
		t.Fatal("expected a billing resolution in context")
	}
	if res.AccountID != accID || res.AllowanceManaged {
		t.Errorf("spoofed headers on an untrusted key must be inert, got %+v", res)
	}

	// No end-user account may have been provisioned.
	var count int
	_ = db.Pool().QueryRow(t.Context(), `SELECT COUNT(*) FROM federated_identities`).Scan(&count)
	if count != 0 {
		t.Errorf("expected no federated identities for untrusted key, got %d", count)
	}
}

func TestMiddleware_TrustedKeyNoHeaders_BillsKeyAccount(t *testing.T) {
	db := setupBillingTest(t)
	accID, _, _ := db.RegisterUser("system@example.com", "hash", "System")

	key := &store.APIKey{ID: 1, AccountID: &accID, TrustUserHeaders: true}
	res, rec := serve(t, db, key, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if res == nil || res.AccountID != accID || res.AllowanceManaged {
		t.Errorf("trusted key without headers must bill its own account, got %+v", res)
	}
}

func TestMiddleware_NoKey_PassesThroughWithoutResolution(t *testing.T) {
	db := setupBillingTest(t)
	res, rec := serve(t, db, nil, owuiHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if res != nil {
		t.Errorf("expected no resolution without a key, got %+v", res)
	}
}

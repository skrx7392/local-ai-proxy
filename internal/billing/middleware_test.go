package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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

// The rate limiter reads the per-account override off the Resolution — both
// billing paths must thread it (docs/design/per-account-rate-limiting.md §4.2).
func TestMiddleware_ThreadsRateLimitOverride_EndUser(t *testing.T) {
	db := setupBillingTest(t)
	sharedAcc, _, _ := db.RegisterUser("shared-rl@example.com", "hash", "SharedRL")
	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true}

	res, _ := serve(t, db, key, owuiHeaders())
	if res == nil {
		t.Fatal("expected a billing resolution")
	}
	if res.RateLimitPerMin != nil {
		t.Errorf("fresh end-user account: expected nil override, got %v", *res.RateLimitPerMin)
	}

	perMin := 15
	if err := db.SetAccountRateLimit(res.AccountID, &perMin); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}
	res2, _ := serve(t, db, key, owuiHeaders())
	if res2 == nil || res2.RateLimitPerMin == nil || *res2.RateLimitPerMin != 15 {
		t.Errorf("expected override 15 on resolution, got %+v", res2)
	}
}

func TestMiddleware_ThreadsRateLimitOverride_ServiceAccount(t *testing.T) {
	db := setupBillingTest(t)
	accID, _, _ := db.RegisterUser("svc-rl@example.com", "hash", "SvcRL")
	perMin := 99
	if err := db.SetAccountRateLimit(accID, &perMin); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}

	// Service path reads the override off the key row (GetKeyByHash join).
	key := &store.APIKey{ID: 1, AccountID: &accID, AccountRateLimitPerMin: &perMin}
	res, _ := serve(t, db, key, nil)
	if res == nil || res.RateLimitPerMin == nil || *res.RateLimitPerMin != 99 {
		t.Errorf("expected override 99 on service resolution, got %+v", res)
	}
}

// A direct key on an allowance-managed account keeps that account's class:
// end-user limiter default and monthly-limit 402 semantics, not service ones.
func TestMiddleware_DirectKeyOnEndUserAccount_KeepsClass(t *testing.T) {
	db := setupBillingTest(t)
	res, err := db.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "direct-key-eua", Email: "direct@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	key := &store.APIKey{ID: 1, AccountID: &res.AccountID, AccountAllowanceManaged: true}

	billed, rec := serve(t, db, key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if billed == nil || billed.AccountID != res.AccountID {
		t.Fatalf("expected key-account billing, got %+v", billed)
	}
	if !billed.AllowanceManaged {
		t.Error("direct key on an end-user account must keep AllowanceManaged=true")
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

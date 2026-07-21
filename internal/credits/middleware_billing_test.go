package credits

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// The gate must check the RESOLVED billing account, not the key's own account:
// an end user with a funded allowance passes even when the shared key account
// is empty (the Codex P1 regression from the design review).
func TestCreditGate_ResolvedBillingAccount_OverridesKeyAccount(t *testing.T) {
	db := setupTestStore(t)
	sharedAcc, _, _ := db.RegisterUser("gate-shared@example.com", "hash", "Shared")
	// Shared account left at 0 balance.

	endUser, err := db.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "gate-user-1", Email: "eu@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}

	gate := CreditGate(db, nil)
	called := false
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true}
	req := httptest.NewRequest("POST", "/test", nil)
	ctx := auth.WithKey(req.Context(), key)
	ctx = billing.WithResolution(ctx, billing.Resolution{AccountID: endUser.AccountID, AllowanceManaged: true})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req.WithContext(ctx))
	if !called {
		t.Errorf("funded end user must pass the gate even with an empty key account (got %d: %s)",
			rec.Code, rec.Body.String())
	}
}

func TestCreditGate_AllowanceExhausted_MonthlyLimitWording(t *testing.T) {
	db := setupTestStore(t)
	sharedAcc, _, _ := db.RegisterUser("gate-shared2@example.com", "hash", "Shared2")
	_ = db.AddCredits(sharedAcc, 100, "grant")

	// End user provisioned with a zero grant → blocked from the start.
	endUser, err := db.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "gate-user-2", Email: "blocked@example.com",
	}, 0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}

	gate := CreditGate(db, nil)
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called for an exhausted allowance")
	}))

	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true}
	req := httptest.NewRequest("POST", "/test", nil)
	ctx := auth.WithKey(req.Context(), key)
	ctx = billing.WithResolution(ctx, billing.Resolution{AccountID: endUser.AccountID, AllowanceManaged: true})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d", rec.Code)
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if resp.Error.Code != "monthly_limit_reached" {
		t.Errorf("expected code monthly_limit_reached, got %q", resp.Error.Code)
	}
}

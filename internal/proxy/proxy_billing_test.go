package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// End-to-end through the billing middleware + handler: a trusted key
// forwarding an OpenWebUI identity settles against the auto-provisioned
// end-user account; the shared key account is untouched; the usage entry is
// attributed to the end-user account.
func TestBillingIntegration_TrustedHeaders_BillEndUserAccount(t *testing.T) {
	db := setupTestDB(t)
	sharedAcc, _, _ := db.RegisterUser("owui-shared@example.com", "hash", "OWUIShared")
	_ = db.AddCredits(sharedAcc, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 2000, 2000, 500)

	ollamaResp := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "llama3.1:8b",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hi!"}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})
	chain := billing.Middleware(db, 5.0)(h)

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(billing.HeaderUserID, "owui-e2e-1")
	req.Header.Set(billing.HeaderUserEmail, "enduser@example.com")
	key := &store.APIKey{ID: 1, Name: "openwebui", AccountID: &sharedAcc, TrustUserHeaders: true}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// End-user account exists, funded by the allowance, and paid for the request.
	var endUserAcc int64
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT account_id FROM federated_identities WHERE source='openwebui' AND external_id='owui-e2e-1'`).Scan(&endUserAcc); err != nil {
		t.Fatalf("end-user account not provisioned: %v", err)
	}
	endBal, _ := db.GetCreditBalance(endUserAcc)
	if endBal.Balance >= 5.0 {
		t.Errorf("expected end-user balance < 5.0 after settlement, got %v", endBal.Balance)
	}
	if endBal.Reserved != 0 {
		t.Errorf("expected end-user reserved 0 after settlement, got %v", endBal.Reserved)
	}

	// Shared account: completely untouched.
	sharedBal, _ := db.GetCreditBalance(sharedAcc)
	if sharedBal.Balance != 1000 || sharedBal.Reserved != 0 {
		t.Errorf("shared account must be untouched, got balance %v reserved %v",
			sharedBal.Balance, sharedBal.Reserved)
	}

	// Usage entry attributed to the end-user account.
	select {
	case entry := <-usageCh:
		if entry.AccountID == nil || *entry.AccountID != endUserAcc {
			t.Errorf("expected usage attribution to end-user account %d, got %v", endUserAcc, entry.AccountID)
		}
		if entry.CreditsCharged <= 0 {
			t.Errorf("expected CreditsCharged > 0, got %v", entry.CreditsCharged)
		}
	default:
		t.Error("expected a usage entry")
	}
}

// Reserve failure on an allowance-managed account uses the monthly-limit wording.
func TestBillingIntegration_AllowanceTooLow_MonthlyLimitReached(t *testing.T) {
	db := setupTestDB(t)
	sharedAcc, _, _ := db.RegisterUser("owui-shared2@example.com", "hash", "OWUIShared2")
	_ = db.AddCredits(sharedAcc, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 2000, 2000, 500)

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})
	chain := billing.Middleware(db, 0.000001)(h) // grant too small for any request

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}],"max_tokens":500}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(billing.HeaderUserID, "owui-e2e-2")
	key := &store.APIKey{ID: 1, Name: "openwebui", AccountID: &sharedAcc, TrustUserHeaders: true}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "monthly_limit_reached") {
		t.Errorf("expected monthly_limit_reached error code, got: %s", rec.Body.String())
	}
}

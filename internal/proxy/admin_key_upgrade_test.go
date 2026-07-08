package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/credits"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// TestLegacyAdminKey_ChatsAfterBackfill reproduces the exact production
// upgrade scenario for OSS-2: a live admin-created API key with NULL
// account_id (which chatted via the old credit-gate bypass) must keep
// chatting after the startup migration attaches it to the admin service
// account. It exercises the real chain the request travels in main.go:
// CreditGate -> proxy handler -> upstream node.
func TestLegacyAdminKey_ChatsAfterBackfill(t *testing.T) {
	db := setupTestDB(t)

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{
		"id":      "chatcmpl-legacy",
		"object":  "chat.completion",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hi"}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	defer upstream.Close()

	const model = "llama3:latest"
	if err := db.UpsertPricing(model, 0.001, 0.001, 100); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	// The pre-upgrade production state: an admin-minted key with no account.
	const keyHash = "legacy-prod-key-hash"
	if _, err := db.CreateKey("prod-legacy", keyHash, "sk-prodleg", 60); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	usageCh := make(chan store.UsageEntry, 10)
	proxyHandler := NewHandler(singleNodeRegistry(t, upstream.URL, model), usageCh, 52428800, db, nil, Options{})
	chain := credits.CreditGate(db, nil)(proxyHandler)

	chat := func(reqModel string) *httptest.ResponseRecorder {
		key, err := db.GetKeyByHash(keyHash)
		if err != nil {
			t.Fatalf("GetKeyByHash: %v", err)
		}
		if key == nil {
			t.Fatal("expected key to exist")
		}
		body := `{"model":"` + reqModel + `","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = addKeyToRequest(req, key)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}

	// Pre-backfill the gate fails closed — the bypass is gone.
	if rec := chat(model); rec.Code != http.StatusForbidden {
		t.Fatalf("pre-backfill: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// The startup migration (same calls main.go makes).
	adminAccID, err := db.EnsureAdminServiceAccount(1000)
	if err != nil {
		t.Fatalf("EnsureAdminServiceAccount: %v", err)
	}
	if _, err := db.BackfillAdminKeyAccounts(adminAccID); err != nil {
		t.Fatalf("BackfillAdminKeyAccounts: %v", err)
	}

	// The very same key chats successfully again.
	rec := chat(model)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-backfill: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse chat response: %v", err)
	}
	if resp["id"] != "chatcmpl-legacy" {
		t.Errorf("expected upstream response to pass through, got %v", resp)
	}

	// And it is metered now: the request settled credits against the account.
	bal, err := db.GetCreditBalance(adminAccID)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal == nil {
		t.Fatal("expected balance row for admin service account")
	}
	if bal.Balance >= 1000 {
		t.Errorf("expected credits to be charged (balance < 1000), got %v", bal.Balance)
	}

	// Behavior change called out in the PR: formerly-bypassing keys are now
	// subject to the pricing allowlist — unpriced models 400.
	if rec := chat("unpriced-model:latest"); rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unpriced model, got %d: %s", rec.Code, rec.Body.String())
	} else {
		assertErrorCode(t, rec, "unknown_model", "invalid_request_error")
	}
}

// TestCreditGateChain_NoBypassForNilAccount pins the invariant at the chain
// level: no request from a keyed client reaches the proxy without an
// account-backed credit check.
func TestCreditGateChain_NoBypassForNilAccount(t *testing.T) {
	db := setupTestDB(t)

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{"id": "x"})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	proxyHandler := NewHandler(singleNodeRegistry(t, upstream.URL, "m"), usageCh, 52428800, db, nil, Options{})
	chain := credits.CreditGate(db, nil)(proxyHandler)

	key := &store.APIKey{ID: 42, Name: "detached"} // AccountID nil
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req = addKeyToRequest(req, key)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for NULL-account key, got %d: %s", rec.Code, rec.Body.String())
	}
}

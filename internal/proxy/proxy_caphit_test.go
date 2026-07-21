package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/creditrequest"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// The reserve-failure 402 (gate passed on a positive sliver of balance, the
// estimate didn't fit) is the second cap-hit site: it must file a credit
// request just like the gate's pre-check does.
func TestReserveFailure_AllowanceManaged_FilesCreditRequest(t *testing.T) {
	db := setupTestDB(t)
	sharedAcc, _, _ := db.RegisterUser("owui-caphit@example.com", "hash", "OWUICapHit")
	_ = db.AddCredits(sharedAcc, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 2000, 2000, 500)

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{})
	defer upstream.Close()

	rec := creditrequest.New(db, "", 0.000001)
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil,
		Options{CapHits: rec})
	// Grant far too small for any request: billing resolution succeeds, the
	// gateless chain reaches reserve, and reserve fails.
	chain := billing.Middleware(db, 0.000001)(h)

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}],"max_tokens":500}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(billing.HeaderUserID, "owui-caphit-1")
	req.Header.Set(billing.HeaderUserEmail, "caphit@example.com")
	key := &store.APIKey{ID: 1, Name: "openwebui", AccountID: &sharedAcc, TrustUserHeaders: true}
	req = addKeyToRequest(req, key)

	w := httptest.NewRecorder()
	chain.ServeHTTP(w, req)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "monthly_limit_reached") {
		t.Errorf("expected monthly_limit_reached, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin has been notified") {
		t.Errorf("expected actionable message, got: %s", w.Body.String())
	}
	rec.Wait()

	rows, err := db.ListCreditRequests("pending", time.Now())
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 pending credit request, got %d", len(rows))
	}
	if rows[0].Email == nil || *rows[0].Email != "caphit@example.com" {
		t.Errorf("expected end-user attribution, got %+v", rows[0])
	}
}

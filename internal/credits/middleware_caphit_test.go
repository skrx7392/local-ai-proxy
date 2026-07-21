package credits

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/creditrequest"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// Gate 402 on an allowance-managed account files a credit request (once) and
// tells the user the admin has been notified.
func TestCreditGate_AllowanceCapHit_FilesCreditRequest(t *testing.T) {
	db := setupTestStore(t)
	res, err := db.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "gate-cap-1", Email: "cap@example.com",
	}, 0, time.Now()) // zero grant: capped from the first request
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	rec := creditrequest.New(db, "", 5.0)
	gate := CreditGate(db, nil, rec)
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	sharedAcc := int64(999)
	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true}
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
		ctx := auth.WithKey(req.Context(), key)
		ctx = billing.WithResolution(ctx, billing.Resolution{AccountID: res.AccountID, AllowanceManaged: true})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req.WithContext(ctx))
		return w
	}

	w := do()
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "monthly_limit_reached") {
		t.Errorf("expected monthly_limit_reached, got: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin has been notified") {
		t.Errorf("expected actionable message, got: %s", w.Body.String())
	}

	// Retrying while capped must not file another request.
	_ = do()
	rec.Wait()

	rows, err := db.ListCreditRequests("pending", time.Now())
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	if len(rows) != 1 || rows[0].AccountID != res.AccountID {
		t.Errorf("expected 1 pending request for account %d, got %+v", res.AccountID, rows)
	}
}

// A plain (non-allowance) account keeps insufficient_credits and never files.
func TestCreditGate_NonAllowance402_DoesNotFile(t *testing.T) {
	db := setupTestStore(t)
	accID, _, _ := db.RegisterUser("gate-cap-2@example.com", "hash", "GateCap2")

	rec := creditrequest.New(db, "", 5.0)
	gate := CreditGate(db, nil, rec)
	handler := gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	}))

	key := &store.APIKey{ID: 1, AccountID: &accID}
	req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "insufficient_credits") {
		t.Errorf("expected insufficient_credits, got: %s", w.Body.String())
	}
	rec.Wait()

	rows, err := db.ListCreditRequests("pending", time.Now())
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no credit requests, got %+v", rows)
	}
}

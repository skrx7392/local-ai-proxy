package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// postAdminKey issues POST /api/admin/keys with the given JSON body.
func postAdminKey(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdmin_CreateKey_DefaultsToAdminServiceAccount(t *testing.T) {
	h, s := setupAdminTest(t)

	rec := postAdminKey(t, h, `{"name":"svc-default","rate_limit":30}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.AccountID <= 0 {
		t.Fatalf("expected positive account_id in response, got %d", resp.AccountID)
	}

	acct, err := s.GetAccountByID(resp.AccountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if acct == nil {
		t.Fatal("expected account to exist")
	}
	if acct.Name != store.AdminServiceAccountName || acct.Type != "service" {
		t.Errorf("expected the admin service account, got name=%q type=%q", acct.Name, acct.Type)
	}

	key, err := s.GetKeyByID(resp.ID)
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if key.AccountID == nil || *key.AccountID != resp.AccountID {
		t.Errorf("expected key attached to account %d, got %v", resp.AccountID, key.AccountID)
	}

	// A second key without account_id reuses the same service account.
	rec2 := postAdminKey(t, h, `{"name":"svc-default-2"}`)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var resp2 createKeyResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp2.AccountID != resp.AccountID {
		t.Errorf("expected same service account %d, got %d", resp.AccountID, resp2.AccountID)
	}
}

func TestAdmin_CreateKey_WithExplicitAccountID(t *testing.T) {
	h, s := setupAdminTest(t)

	accID, _, err := s.RegisterUser("keyowner@example.com", "hash", "KeyOwner")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	rec := postAdminKey(t, h, fmt.Sprintf(`{"name":"explicit","account_id":%d}`, accID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.AccountID != accID {
		t.Errorf("expected account_id %d, got %d", accID, resp.AccountID)
	}

	key, err := s.GetKeyByID(resp.ID)
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if key.AccountID == nil || *key.AccountID != accID {
		t.Errorf("expected key attached to account %d, got %v", accID, key.AccountID)
	}
}

func TestAdmin_CreateKey_UnknownAccountID_Returns404(t *testing.T) {
	h, _ := setupAdminTest(t)

	rec := postAdminKey(t, h, `{"name":"nope","account_id":999999}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown account, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_CreateAccountKey_ResponseIncludesAccountID(t *testing.T) {
	h, s := setupAdminTest(t)

	accID, _, err := s.RegisterUser("acctkey@example.com", "hash", "AcctKey")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/admin/accounts/%d/keys", accID),
		bytes.NewBufferString(`{"name":"bound"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.AccountID != accID {
		t.Errorf("expected account_id %d in response, got %d", accID, resp.AccountID)
	}
}

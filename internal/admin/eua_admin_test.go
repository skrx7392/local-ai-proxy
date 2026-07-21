package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func adminPut(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAdmin_SetTrustUserHeaders(t *testing.T) {
	h, s := setupAdminTest(t)
	keyID, err := s.CreateKey("openwebui", "hash-trust-admin", "laip_ta", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	rec := adminPut(t, h, "/api/admin/keys/"+strconv.FormatInt(keyID, 10)+"/trust-user-headers",
		`{"trust_user_headers":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	key, _ := s.GetKeyByID(keyID)
	if key == nil || !key.TrustUserHeaders {
		t.Error("expected trust_user_headers=true after PUT")
	}

	// Flip back off.
	rec = adminPut(t, h, "/api/admin/keys/"+strconv.FormatInt(keyID, 10)+"/trust-user-headers",
		`{"trust_user_headers":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on unset, got %d", rec.Code)
	}
	key, _ = s.GetKeyByID(keyID)
	if key.TrustUserHeaders {
		t.Error("expected trust_user_headers=false after unset")
	}
}

func TestAdmin_SetTrustUserHeaders_Errors(t *testing.T) {
	h, _ := setupAdminTest(t)

	if rec := adminPut(t, h, "/api/admin/keys/999999/trust-user-headers", `{"trust_user_headers":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key: expected 404, got %d", rec.Code)
	}
	if rec := adminPut(t, h, "/api/admin/keys/abc/trust-user-headers", `{"trust_user_headers":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id: expected 400, got %d", rec.Code)
	}
	if rec := adminPut(t, h, "/api/admin/keys/1/trust-user-headers", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing field: expected 400, got %d", rec.Code)
	}
}

func TestAdmin_SetAccountAllowance(t *testing.T) {
	h, s := setupAdminTest(t)
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-eua-1", Email: "allow@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	idStr := strconv.FormatInt(res.AccountID, 10)

	if rec := adminPut(t, h, "/api/admin/accounts/"+idStr+"/allowance", `{"monthly_grant":25.5}`); rec.Code != http.StatusOK {
		t.Fatalf("set: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var grant *float64
	_ = s.Pool().QueryRow(t.Context(), `SELECT monthly_grant FROM accounts WHERE id=$1`, res.AccountID).Scan(&grant)
	if grant == nil || *grant != 25.5 {
		t.Errorf("expected monthly_grant 25.5, got %v", grant)
	}

	// null clears back to env default.
	if rec := adminPut(t, h, "/api/admin/accounts/"+idStr+"/allowance", `{"monthly_grant":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d", rec.Code)
	}
	grant = nil
	_ = s.Pool().QueryRow(t.Context(), `SELECT monthly_grant FROM accounts WHERE id=$1`, res.AccountID).Scan(&grant)
	if grant != nil {
		t.Errorf("expected cleared monthly_grant, got %v", *grant)
	}

	if rec := adminPut(t, h, "/api/admin/accounts/"+idStr+"/allowance", `{"monthly_grant":-1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative: expected 400, got %d", rec.Code)
	}
	if rec := adminPut(t, h, "/api/admin/accounts/999999/allowance", `{"monthly_grant":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown account: expected 404, got %d", rec.Code)
	}
}

func TestAdmin_ListAccounts_IncludesAllowanceFields(t *testing.T) {
	h, s := setupAdminTest(t)
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-eua-2", Email: "list@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/accounts?envelope=0&type=end_user", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var rows []struct {
		ID               int64    `json:"id"`
		Type             string   `json:"type"`
		AllowanceManaged bool     `json:"allowance_managed"`
		MonthlyGrant     *float64 `json:"monthly_grant"`
		Email            *string  `json:"email"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 end_user account, got %d", len(rows))
	}
	row := rows[0]
	if row.ID != res.AccountID || !row.AllowanceManaged || row.Type != "end_user" {
		t.Errorf("unexpected row: %+v", row)
	}
	if row.MonthlyGrant != nil {
		t.Errorf("expected nil monthly_grant (env default), got %v", *row.MonthlyGrant)
	}
	if row.Email == nil || *row.Email != "list@example.com" {
		t.Errorf("expected federated email, got %v", row.Email)
	}
}

func TestAdmin_SetAccountAllowance_AbsentFieldRejected(t *testing.T) {
	h, s := setupAdminTest(t)
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-eua-3", Email: "absent@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	override := 42.0
	if err := s.SetMonthlyGrant(res.AccountID, &override); err != nil {
		t.Fatalf("SetMonthlyGrant: %v", err)
	}

	// {} must NOT silently clear the override (null is the explicit clear).
	rec := adminPut(t, h, "/api/admin/accounts/"+strconv.FormatInt(res.AccountID, 10)+"/allowance", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("absent field: expected 400, got %d", rec.Code)
	}
	var grant *float64
	_ = s.Pool().QueryRow(t.Context(), `SELECT monthly_grant FROM accounts WHERE id=$1`, res.AccountID).Scan(&grant)
	if grant == nil || *grant != 42.0 {
		t.Errorf("override must survive a rejected request, got %v", grant)
	}
}

func TestAdmin_KeyReads_SurfaceTrustFlag(t *testing.T) {
	h, s := setupAdminTest(t)
	keyID, err := s.CreateKey("openwebui", "hash-trust-read", "laip_tr2", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := s.SetTrustUserHeaders(keyID, true); err != nil {
		t.Fatalf("SetTrustUserHeaders: %v", err)
	}

	for _, path := range []string{"/api/admin/keys?envelope=0", "/api/admin/keys/" + strconv.FormatInt(keyID, 10)} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Key", testAdminKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"trust_user_headers":true`) {
			t.Errorf("%s: response must surface trust_user_headers=true: %s", path, rec.Body.String()[:200])
		}
	}
}

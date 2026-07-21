package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// newCreditRequestHandler builds a handler with the env-default grant wired,
// mirroring main.go. A fresh handler per test keeps each test inside the
// 10 req/min X-Admin-Key budget.
func newCreditRequestHandler(s *store.Store) http.Handler {
	usageCh := make(chan store.UsageEntry, 10)
	return NewHandler(s, testAdminKey, usageCh, Options{EndUserMonthlyGrant: 5.0})
}

func provisionEndUserAccount(t *testing.T, s *store.Store, extID, email string) int64 {
	t.Helper()
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: extID, Email: email, DisplayName: "EU " + extID,
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	return res.AccountID
}

func adminGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type creditRequestDTO struct {
	ID                    int64   `json:"id"`
	AccountID             int64   `json:"account_id"`
	AccountName           string  `json:"account_name"`
	Email                 *string `json:"email"`
	Period                string  `json:"period"`
	Status                string  `json:"status"`
	CreatedAt             string  `json:"created_at"`
	ResolvedAt            *string `json:"resolved_at"`
	ResolvedNote          *string `json:"resolved_note"`
	EffectiveMonthlyGrant float64 `json:"effective_monthly_grant"`
	Balance               float64 `json:"balance"`
}

func decodeCreditRequestList(t *testing.T, rec *httptest.ResponseRecorder) ([]creditRequestDTO, *Pagination) {
	t.Helper()
	var env struct {
		Data       []creditRequestDTO `json:"data"`
		Pagination *Pagination        `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	return env.Data, env.Pagination
}

func TestCreditRequests_ListPendingDefault(t *testing.T) {
	_, s := setupAdminTest(t)
	h := newCreditRequestHandler(s)

	accA := provisionEndUserAccount(t, s, "adm-1a", "list-a@example.com")
	accB := provisionEndUserAccount(t, s, "adm-1b", "list-b@example.com")
	override := 12.5
	if err := s.SetMonthlyGrant(accB, &override); err != nil {
		t.Fatalf("SetMonthlyGrant: %v", err)
	}
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	if _, _, err := s.FileCreditRequest(accA, now); err != nil {
		t.Fatalf("file A: %v", err)
	}
	if _, _, err := s.FileCreditRequest(accB, now.Add(time.Minute)); err != nil {
		t.Fatalf("file B: %v", err)
	}

	rec := adminGet(t, h, "/api/admin/credit-requests")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rows, pag := decodeCreditRequestList(t, rec)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if pag == nil || pag.Total != 2 {
		t.Errorf("expected pagination total 2, got %+v", pag)
	}
	// Newest first: B (with its override), then A (env default).
	if rows[0].AccountID != accB || rows[1].AccountID != accA {
		t.Errorf("expected [B, A], got [%d, %d]", rows[0].AccountID, rows[1].AccountID)
	}
	if rows[0].EffectiveMonthlyGrant != 12.5 {
		t.Errorf("expected effective grant 12.5 for override, got %v", rows[0].EffectiveMonthlyGrant)
	}
	if rows[1].EffectiveMonthlyGrant != 5.0 {
		t.Errorf("expected effective grant 5.0 (env default), got %v", rows[1].EffectiveMonthlyGrant)
	}
	if rows[0].Email == nil || *rows[0].Email != "list-b@example.com" {
		t.Errorf("expected email join, got %v", rows[0].Email)
	}
	if rows[0].Period != "2026-07-01" {
		t.Errorf("expected period 2026-07-01, got %q", rows[0].Period)
	}
	if rows[0].Status != "pending" {
		t.Errorf("expected pending, got %q", rows[0].Status)
	}
}

func TestCreditRequests_StatusFilterAndValidation(t *testing.T) {
	_, s := setupAdminTest(t)
	h := newCreditRequestHandler(s)

	acc := provisionEndUserAccount(t, s, "adm-2", "filter@example.com")
	id, _, err := s.FileCreditRequest(acc, time.Now())
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := s.ResolveCreditRequest(id, "granted", "+$5 test", time.Now()); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rec := adminGet(t, h, "/api/admin/credit-requests")
	rows, _ := decodeCreditRequestList(t, rec)
	if len(rows) != 0 {
		t.Errorf("default pending view: expected 0 rows, got %d", len(rows))
	}

	rec = adminGet(t, h, "/api/admin/credit-requests?status=granted")
	rows, _ = decodeCreditRequestList(t, rec)
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("expected the granted row, got %+v", rows)
	}
	if rows[0].ResolvedNote == nil || *rows[0].ResolvedNote != "+$5 test" {
		t.Errorf("expected resolved note, got %v", rows[0].ResolvedNote)
	}
	if rows[0].ResolvedAt == nil {
		t.Error("expected resolved_at")
	}

	rec = adminGet(t, h, "/api/admin/credit-requests?status=bogus")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bogus status, got %d", rec.Code)
	}
}

func TestCreditRequests_Resolve(t *testing.T) {
	_, s := setupAdminTest(t)
	h := newCreditRequestHandler(s)

	acc := provisionEndUserAccount(t, s, "adm-3", "resolve@example.com")
	id, _, err := s.FileCreditRequest(acc, time.Now())
	if err != nil {
		t.Fatalf("file: %v", err)
	}

	rec := adminPut(t, h, "/api/admin/credit-requests/"+itoa(id),
		`{"status":"granted","note":"+$5 via discord by tester"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode resolve envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Data.ID != id || env.Data.Status != "granted" {
		t.Errorf("expected {data:{id:%d,status:granted}}, got %s", id, rec.Body.String())
	}

	rows, err := s.ListCreditRequests("granted", time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 granted row, got %v err=%v", rows, err)
	}
	if rows[0].ResolvedNote == nil || *rows[0].ResolvedNote != "+$5 via discord by tester" {
		t.Errorf("note not persisted: %v", rows[0].ResolvedNote)
	}

	// Already resolved → 409 with a distinct code.
	rec = adminPut(t, h, "/api/admin/credit-requests/"+itoa(id), `{"status":"dismissed"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = adminPut(t, h, "/api/admin/credit-requests/999999", `{"status":"granted"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}

	rec = adminPut(t, h, "/api/admin/credit-requests/"+itoa(id), `{"status":"pending"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid target status, got %d", rec.Code)
	}

	rec = adminPut(t, h, "/api/admin/credit-requests/not-a-number", `{"status":"granted"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad id, got %d", rec.Code)
	}
}

func TestCreditRequests_StalePendingExpiresViaHTTP(t *testing.T) {
	_, s := setupAdminTest(t)
	h := newCreditRequestHandler(s)

	acc := provisionEndUserAccount(t, s, "adm-5", "stale-http@example.com")
	// File for LAST month (last day of it, so the period is unambiguous).
	now := time.Now().UTC()
	lastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	id, filed, err := s.FileCreditRequest(acc, lastMonth)
	if err != nil || !filed {
		t.Fatalf("backdated file: filed=%v err=%v", filed, err)
	}

	// A stale Discord card's button lands directly on the resolve endpoint:
	// refused with a distinct code, no state left actionable.
	rec := adminPut(t, h, "/api/admin/credit-requests/"+itoa(id), `{"status":"granted"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for stale resolve, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !bytes.Contains([]byte(body), []byte("request_expired")) {
		t.Errorf("expected request_expired code, got %s", body)
	}

	rec = adminGet(t, h, "/api/admin/credit-requests")
	rows, _ := decodeCreditRequestList(t, rec)
	if len(rows) != 0 {
		t.Errorf("pending view must be empty after expiry, got %+v", rows)
	}
	rec = adminGet(t, h, "/api/admin/credit-requests?status=expired")
	rows, _ = decodeCreditRequestList(t, rec)
	if len(rows) != 1 || rows[0].ID != id {
		t.Errorf("expected the request under status=expired, got %+v", rows)
	}
}

func TestCreditRequests_RequireAuth(t *testing.T) {
	h, _ := setupAdminTest(t)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/admin/credit-requests"},
		{http.MethodPut, "/api/admin/credit-requests/1"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401 without auth, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestListAccounts_EffectiveMonthlyGrant(t *testing.T) {
	_, s := setupAdminTest(t)
	h := newCreditRequestHandler(s)

	defaultAcc := provisionEndUserAccount(t, s, "adm-4a", "grant-default@example.com")
	overrideAcc := provisionEndUserAccount(t, s, "adm-4b", "grant-override@example.com")
	override := 12.5
	if err := s.SetMonthlyGrant(overrideAcc, &override); err != nil {
		t.Fatalf("SetMonthlyGrant: %v", err)
	}
	personalAcc, _, _ := s.RegisterUser("personal@example.com", "hash", "Personal")

	rec := adminGet(t, h, "/api/admin/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []struct {
			ID                    int64    `json:"id"`
			AllowanceManaged      bool     `json:"allowance_managed"`
			EffectiveMonthlyGrant *float64 `json:"effective_monthly_grant"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := map[int64]*float64{}
	for _, a := range env.Data {
		byID[a.ID] = a.EffectiveMonthlyGrant
	}
	if g := byID[defaultAcc]; g == nil || *g != 5.0 {
		t.Errorf("default end-user account: expected effective 5.0, got %v", g)
	}
	if g := byID[overrideAcc]; g == nil || *g != 12.5 {
		t.Errorf("override end-user account: expected effective 12.5, got %v", g)
	}
	if g := byID[personalAcc]; g != nil {
		t.Errorf("personal account: expected null effective grant, got %v", *g)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

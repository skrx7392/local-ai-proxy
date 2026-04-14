package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// ---- Shared helpers -------------------------------------------------------

func doAdminGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeEnvelope expects a `{data, pagination}` shape; caller supplies the
// concrete data slice type via the generic parameter.
func decodeEnvelope[T any](t *testing.T, body []byte) (T, *Pagination) {
	t.Helper()
	var env struct {
		Data       T           `json:"data"`
		Pagination *Pagination `json:"pagination"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decodeEnvelope: %v\nbody: %s", err, string(body))
	}
	return env.Data, env.Pagination
}

// ---- /api/admin/keys ------------------------------------------------------

func seedThreeKeys(t *testing.T, s *store.Store) (activeIDs []int64, revokedID int64) {
	t.Helper()
	for i := 0; i < 3; i++ {
		id, err := s.CreateKey(fmt.Sprintf("k%d", i), fmt.Sprintf("hash-%d", i), fmt.Sprintf("sk-k%d", i), 60)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if i == 2 {
			if err := s.RevokeKey(id); err != nil {
				t.Fatalf("RevokeKey: %v", err)
			}
			revokedID = id
		} else {
			activeIDs = append(activeIDs, id)
		}
	}
	return
}

func TestListKeys_LegacyShape_NoEnvelopeMetadata(t *testing.T) {
	h, s := setupAdminTest(t)
	seedThreeKeys(t, s)

	rec := doAdminGET(t, h, "/api/admin/keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Legacy: must decode as a raw array. Any pagination key is a regression.
	var arr []keyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("legacy shape must be a raw array: %v", err)
	}
	if len(arr) != 3 {
		t.Errorf("len=%d, want 3", len(arr))
	}
	if contains(rec.Body.String(), `"pagination"`) {
		t.Error("legacy response must not include pagination key")
	}
}

func TestListKeys_LegacyShape_WithIsActiveFilter_NoPagination(t *testing.T) {
	// is_active on /keys means "not revoked". Filters apply in legacy mode;
	// only the envelope wrapper is gated behind envelope=1.
	h, s := setupAdminTest(t)
	seedThreeKeys(t, s)

	rec := doAdminGET(t, h, "/api/admin/keys?is_active=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var arr []keyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("legacy shape must be a raw array: %v", err)
	}
	if len(arr) != 2 {
		t.Errorf("len=%d, want 2 active keys", len(arr))
	}
	for _, k := range arr {
		if k.Revoked {
			t.Errorf("expected only non-revoked keys, got revoked %d", k.ID)
		}
	}
}

func TestListKeys_Envelope_PaginationTotalIsFilteredCount(t *testing.T) {
	h, s := setupAdminTest(t)
	seedThreeKeys(t, s)

	// Revoked-only: filtered list has 1 row. pagination.total must reflect
	// that, not the 3-row pre-filter list.
	rec := doAdminGET(t, h, "/api/admin/keys?envelope=1&is_active=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]keyResponse](t, rec.Body.Bytes())
	if pag == nil {
		t.Fatal("envelope=1 must include pagination")
	}
	if pag.Total != 1 {
		t.Errorf("pagination.total = %d, want 1 (filtered count)", pag.Total)
	}
	if len(data) != 1 {
		t.Errorf("len(data) = %d, want 1", len(data))
	}
}

func TestListKeys_Envelope_LimitOffset(t *testing.T) {
	h, s := setupAdminTest(t)
	for i := 0; i < 5; i++ {
		if _, err := s.CreateKey(fmt.Sprintf("pg%d", i), fmt.Sprintf("pg-hash-%d", i), fmt.Sprintf("sk-pg%d", i), 60); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
	}
	rec := doAdminGET(t, h, "/api/admin/keys?envelope=1&limit=2&offset=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]keyResponse](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 5 || pag.Limit != 2 || pag.Offset != 2 {
		t.Errorf("pagination=%+v, want {limit:2,offset:2,total:5}", pag)
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}
}

func TestListKeys_InvalidEnvelope_400(t *testing.T) {
	h, _ := setupAdminTest(t)
	rec := doAdminGET(t, h, "/api/admin/keys?envelope=true")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

func TestListKeys_InvalidIsActive_400(t *testing.T) {
	h, _ := setupAdminTest(t)
	rec := doAdminGET(t, h, "/api/admin/keys?is_active=1")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// ---- /api/admin/users -----------------------------------------------------

// seedUserWithRoleAndActive bypasses the handler layer to stamp role/is_active
// directly so tests can produce a known distribution without exercising the
// bootstrap/guardrail endpoints.
func seedUserWithRoleAndActive(t *testing.T, s *store.Store, email, name, role string, active bool) int64 {
	t.Helper()
	id, err := s.CreateUser(email, "hash-"+email, name)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if role == "admin" {
		if _, err := s.Pool().Exec(context.Background(),
			`UPDATE users SET role='admin' WHERE id=$1`, id); err != nil {
			t.Fatalf("set role: %v", err)
		}
	}
	if !active {
		if err := s.SetUserActive(id, false); err != nil {
			t.Fatalf("SetUserActive: %v", err)
		}
	}
	return id
}

func TestListUsers_LegacyShape(t *testing.T) {
	h, s := setupAdminTest(t)
	seedUserWithRoleAndActive(t, s, "u1@example.com", "U1", "user", true)
	seedUserWithRoleAndActive(t, s, "u2@example.com", "U2", "admin", true)

	rec := doAdminGET(t, h, "/api/admin/users")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), `"pagination"`) {
		t.Error("legacy response must not include pagination key")
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("expected array, got %v", err)
	}
	if len(arr) != 2 {
		t.Errorf("len=%d, want 2", len(arr))
	}
}

func TestListUsers_Envelope_RoleFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	seedUserWithRoleAndActive(t, s, "u1@example.com", "U1", "user", true)
	seedUserWithRoleAndActive(t, s, "a1@example.com", "A1", "admin", true)
	seedUserWithRoleAndActive(t, s, "a2@example.com", "A2", "admin", true)

	rec := doAdminGET(t, h, "/api/admin/users?envelope=1&role=admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 2 {
		t.Errorf("pagination = %+v, want total=2 (filtered count)", pag)
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}
	for _, u := range data {
		if u["role"] != "admin" {
			t.Errorf("got role=%v in admin-filtered list", u["role"])
		}
	}
}

func TestListUsers_Envelope_IsActiveFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	seedUserWithRoleAndActive(t, s, "a1@example.com", "A1", "admin", true)
	seedUserWithRoleAndActive(t, s, "u1@example.com", "U1", "user", true)
	seedUserWithRoleAndActive(t, s, "u2@example.com", "U2", "user", false)

	rec := doAdminGET(t, h, "/api/admin/users?envelope=1&is_active=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 1 {
		t.Errorf("pagination = %+v, want total=1", pag)
	}
	if len(data) != 1 || data[0]["email"] != "u2@example.com" {
		t.Errorf("unexpected data=%+v", data)
	}
}

func TestListUsers_InvalidRole_400(t *testing.T) {
	h, _ := setupAdminTest(t)
	rec := doAdminGET(t, h, "/api/admin/users?role=superadmin")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

func TestListUsers_InvalidLimit_400(t *testing.T) {
	h, _ := setupAdminTest(t)
	rec := doAdminGET(t, h, "/api/admin/users?limit=-5")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// ---- /api/admin/accounts --------------------------------------------------

func TestListAccounts_LegacyShape(t *testing.T) {
	h, s := setupAdminTest(t)
	if _, err := s.CreateAccount("acc-a", "personal"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if _, err := s.CreateAccount("acc-b", "service"); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	rec := doAdminGET(t, h, "/api/admin/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), `"pagination"`) {
		t.Error("legacy response must not include pagination key")
	}
}

func TestListAccounts_Envelope_TypeFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	_, _ = s.CreateAccount("p-1", "personal")
	_, _ = s.CreateAccount("p-2", "personal")
	_, _ = s.CreateAccount("s-1", "service")

	rec := doAdminGET(t, h, "/api/admin/accounts?envelope=1&type=service")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 1 {
		t.Errorf("pagination = %+v, want total=1", pag)
	}
	if len(data) != 1 || data[0]["type"] != "service" {
		t.Errorf("unexpected data=%+v", data)
	}
}

func TestListAccounts_Envelope_IsActiveFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	a1, _ := s.CreateAccount("active-acc", "personal")
	a2, _ := s.CreateAccount("inactive-acc", "personal")
	_ = a1
	if err := s.SetAccountActive(a2, false); err != nil {
		t.Fatalf("SetAccountActive: %v", err)
	}

	rec := doAdminGET(t, h, "/api/admin/accounts?envelope=1&is_active=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 1 {
		t.Errorf("pagination = %+v, want total=1", pag)
	}
	if len(data) != 1 || data[0]["name"] != "inactive-acc" {
		t.Errorf("unexpected data=%+v", data)
	}
}

func TestListAccounts_InvalidType_400(t *testing.T) {
	h, _ := setupAdminTest(t)
	rec := doAdminGET(t, h, "/api/admin/accounts?type=other")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// ---- /api/admin/pricing ---------------------------------------------------

func TestListPricing_LegacyShape(t *testing.T) {
	h, s := setupAdminTest(t)
	_ = s.UpsertPricing("m1", 0.001, 0.002, 500)
	_ = s.UpsertPricing("m2", 0.003, 0.004, 500)

	rec := doAdminGET(t, h, "/api/admin/pricing")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), `"pagination"`) {
		t.Error("legacy response must not include pagination key")
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("legacy shape must be a raw array: %v", err)
	}
	if len(arr) < 2 {
		t.Errorf("len=%d, want >=2", len(arr))
	}
}

func TestListPricing_Envelope_Pagination(t *testing.T) {
	h, s := setupAdminTest(t)
	_ = s.UpsertPricing("m1", 0.001, 0.002, 500)
	_ = s.UpsertPricing("m2", 0.003, 0.004, 500)
	_ = s.UpsertPricing("m3", 0.005, 0.006, 500)

	rec := doAdminGET(t, h, "/api/admin/pricing?envelope=1&limit=2&offset=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 3 || pag.Limit != 2 {
		t.Errorf("pagination = %+v, want {limit:2, total:3}", pag)
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}
}

// ---- /api/admin/registration-tokens ---------------------------------------

func TestListRegistrationTokens_LegacyShape(t *testing.T) {
	h, s := setupAdminTest(t)
	_, _ = s.CreateRegistrationToken("t1", "reg-hash-1", 10, 1, nil)
	_, _ = s.CreateRegistrationToken("t2", "reg-hash-2", 10, 1, nil)

	rec := doAdminGET(t, h, "/api/admin/registration-tokens")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), `"pagination"`) {
		t.Error("legacy response must not include pagination key")
	}
}

func TestListRegistrationTokens_Envelope_IsActiveFilter_MapsToRevoked(t *testing.T) {
	// is_active on /registration-tokens maps to the inverse of Revoked.
	h, s := setupAdminTest(t)
	_, _ = s.CreateRegistrationToken("active-t", "reg-hash-active", 10, 1, nil)
	revokedID, _ := s.CreateRegistrationToken("revoked-t", "reg-hash-revoked", 10, 1, nil)
	if err := s.RevokeRegistrationToken(revokedID); err != nil {
		t.Fatalf("RevokeRegistrationToken: %v", err)
	}

	rec := doAdminGET(t, h, "/api/admin/registration-tokens?envelope=1&is_active=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 1 {
		t.Errorf("pagination = %+v, want total=1", pag)
	}
	if len(data) != 1 || data[0]["name"] != "active-t" {
		t.Errorf("unexpected data=%+v", data)
	}
}

// ---- /api/admin/usage -----------------------------------------------------

func TestGetUsage_LegacyShape_PreservesExistingParams(t *testing.T) {
	h, s := setupAdminTest(t)
	id, _ := s.CreateKey("usage-key", "u-hash", "sk-use", 60)
	_ = s.LogUsage(store.UsageEntry{
		APIKeyID: id, Model: "llama3",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		DurationMs: 100, Status: "completed",
	})

	// No envelope param → legacy raw array shape, existing key_id filter
	// still honored.
	rec := doAdminGET(t, h, fmt.Sprintf("/api/admin/usage?key_id=%d", id))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if contains(rec.Body.String(), `"pagination"`) {
		t.Error("legacy /usage response must not include pagination key")
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("legacy shape must be a raw array: %v", err)
	}
	if len(arr) != 1 {
		t.Errorf("len=%d, want 1", len(arr))
	}
}

func TestGetUsage_Envelope_WrapsWithPagination(t *testing.T) {
	h, s := setupAdminTest(t)
	id, _ := s.CreateKey("usage-key", "u-hash", "sk-use", 60)
	for i := 0; i < 3; i++ {
		_ = s.LogUsage(store.UsageEntry{
			APIKeyID: id, Model: fmt.Sprintf("model-%d", i),
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			DurationMs: 100, Status: "completed",
		})
	}

	rec := doAdminGET(t, h, fmt.Sprintf("/api/admin/usage?envelope=1&key_id=%d&limit=2&offset=0", id))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	data, pag := decodeEnvelope[[]map[string]any](t, rec.Body.Bytes())
	if pag == nil || pag.Total != 3 || pag.Limit != 2 {
		t.Errorf("pagination = %+v, want {limit:2, total:3}", pag)
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}
}

func TestGetUsage_InvalidEnvelope_400(t *testing.T) {
	h, _ := setupAdminTest(t)
	rec := doAdminGET(t, h, "/api/admin/usage?envelope=yes")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

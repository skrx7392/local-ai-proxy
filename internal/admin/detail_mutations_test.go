package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// envelopeData decodes the JSON envelope and returns the `data` field.
func envelopeData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Data       map[string]any `json:"data"`
		Pagination map[string]any `json:"pagination,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%s)", err, string(body))
	}
	return env.Data
}

func envelopeList(t *testing.T, body []byte) ([]map[string]any, map[string]any) {
	t.Helper()
	var env struct {
		Data       []map[string]any `json:"data"`
		Pagination map[string]any   `json:"pagination,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope list: %v (body=%s)", err, string(body))
	}
	return env.Data, env.Pagination
}

// --- GET /api/admin/users/{id} -----------------------------------------------

func TestAdmin_GetUser_OK(t *testing.T) {
	h, s := setupAdminTest(t)
	uid, _ := s.CreateUser("detail@example.com", "hash", "Detail")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/"+strconv.FormatInt(uid, 10), nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := envelopeData(t, rec.Body.Bytes())
	if int64(data["id"].(float64)) != uid {
		t.Errorf("expected id=%d, got %v", uid, data["id"])
	}
	if data["email"] != "detail@example.com" {
		t.Errorf("expected email, got %v", data["email"])
	}
}

func TestAdmin_GetUser_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/99999", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestAdmin_GetUser_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/abc", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- PUT /api/admin/users/{id}/role ------------------------------------------

func TestAdmin_UpdateUserRole_Promote(t *testing.T) {
	h, s := setupAdminTest(t)
	uid, _ := s.CreateUser("promote@example.com", "hash", "Promote")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(uid, 10)+"/role",
		bytes.NewBufferString(`{"role":"admin"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := envelopeData(t, rec.Body.Bytes())
	if data["role"] != "admin" {
		t.Errorf("expected role=admin in response, got %v", data["role"])
	}
	u, _ := s.GetUserByID(uid)
	if u.Role != "admin" {
		t.Errorf("expected DB role=admin, got %q", u.Role)
	}
}

func TestAdmin_UpdateUserRole_DemoteLastAdmin_409(t *testing.T) {
	h, s := setupAdminTest(t)
	onlyAdmin := seedAdmin(t, s, "only@example.com")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(onlyAdmin, 10)+"/role",
		bytes.NewBufferString(`{"role":"user"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var errResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	errObj, _ := errResp["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "last_admin" {
		t.Errorf("expected code 'last_admin', got %v", errObj["code"])
	}
}

func TestAdmin_UpdateUserRole_InvalidRole_400(t *testing.T) {
	h, s := setupAdminTest(t)
	uid, _ := s.CreateUser("bad@example.com", "h", "Bad")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(uid, 10)+"/role",
		bytes.NewBufferString(`{"role":"superadmin"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_UpdateUserRole_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/99999/role",
		bytes.NewBufferString(`{"role":"admin"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- GET /api/admin/keys/{id} -------------------------------------------------

func TestAdmin_GetKey_OK(t *testing.T) {
	h, s := setupAdminTest(t)
	kid, _ := s.CreateKey("detail-key", "hash-get", "sk-gk", 60)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys/"+strconv.FormatInt(kid, 10), nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := envelopeData(t, rec.Body.Bytes())
	if int64(data["id"].(float64)) != kid {
		t.Errorf("expected id=%d, got %v", kid, data["id"])
	}
	// Raw key hash must NOT be exposed — only prefix.
	if _, present := data["key_hash"]; present {
		t.Error("key_hash leaked to client")
	}
	if data["key_prefix"] != "sk-gk" {
		t.Errorf("expected key_prefix=sk-gk, got %v", data["key_prefix"])
	}
}

func TestAdmin_GetKey_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys/99999", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- PUT /api/admin/keys/{id}/rate-limit --------------------------------------

func TestAdmin_UpdateKeyRateLimit_OK(t *testing.T) {
	h, s := setupAdminTest(t)
	kid, _ := s.CreateKey("rl-key", "hash-rl", "sk-rl", 60)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(kid, 10)+"/rate-limit",
		bytes.NewBufferString(`{"rate_limit":500}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data := envelopeData(t, rec.Body.Bytes())
	if int(data["rate_limit"].(float64)) != 500 {
		t.Errorf("expected rate_limit=500, got %v", data["rate_limit"])
	}
	k, _ := s.GetKeyByID(kid)
	if k.RateLimit != 500 {
		t.Errorf("DB rate_limit: expected 500, got %d", k.RateLimit)
	}
}

func TestAdmin_UpdateKeyRateLimit_CapEnforced(t *testing.T) {
	h, s := setupAdminTest(t)
	kid, _ := s.CreateKey("cap-key", "hash-cap", "sk-cap", 60)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(kid, 10)+"/rate-limit",
		bytes.NewBufferString(`{"rate_limit":20000}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for cap violation, got %d: %s", rec.Code, rec.Body.String())
	}
	// Original rate_limit must remain unchanged.
	k, _ := s.GetKeyByID(kid)
	if k.RateLimit != 60 {
		t.Errorf("expected rate_limit unchanged=60, got %d", k.RateLimit)
	}
}

func TestAdmin_UpdateKeyRateLimit_RejectsZero(t *testing.T) {
	// Explicit updates must name a positive integer — unlike createKey, which
	// treats 0 as "omitted → default 60" for back-compat. See PLAN.md §PR 0
	// note on PR 3.
	h, s := setupAdminTest(t)
	kid, _ := s.CreateKey("zero-key", "hash-zro", "sk-zro", 120)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(kid, 10)+"/rate-limit",
		bytes.NewBufferString(`{"rate_limit":0}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for explicit rate_limit=0, got %d: %s", rec.Code, rec.Body.String())
	}
	k, _ := s.GetKeyByID(kid)
	if k.RateLimit != 120 {
		t.Errorf("expected rate_limit unchanged=120, got %d", k.RateLimit)
	}
}

func TestAdmin_UpdateKeyRateLimit_RejectsNegative(t *testing.T) {
	h, s := setupAdminTest(t)
	kid, _ := s.CreateKey("neg-key", "hash-neg", "sk-neg", 90)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(kid, 10)+"/rate-limit",
		bytes.NewBufferString(`{"rate_limit":-5}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative rate_limit, got %d: %s", rec.Code, rec.Body.String())
	}
	k, _ := s.GetKeyByID(kid)
	if k.RateLimit != 90 {
		t.Errorf("expected rate_limit unchanged=90, got %d", k.RateLimit)
	}
}

func TestAdmin_UpdateKeyRateLimit_MissingField(t *testing.T) {
	h, s := setupAdminTest(t)
	kid, _ := s.CreateKey("miss-key", "hash-mis", "sk-mis", 60)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(kid, 10)+"/rate-limit",
		bytes.NewBufferString(`{}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing field, got %d", rec.Code)
	}
}

func TestAdmin_UpdateKeyRateLimit_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/99999/rate-limit",
		bytes.NewBufferString(`{"rate_limit":60}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- GET /api/admin/registrations ---------------------------------------------

func TestAdmin_ListRegistrations_ReturnsEnvelope(t *testing.T) {
	h, s := setupAdminTest(t)

	if _, _, err := s.RegisterUser("reg1@example.com", "h", "Reg1"); err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if _, _, err := s.RegisterUser("reg2@example.com", "h", "Reg2"); err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/registrations", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data, pag := envelopeList(t, rec.Body.Bytes())
	if len(data) < 2 {
		t.Errorf("expected >=2 events, got %d", len(data))
	}
	if pag == nil {
		t.Fatal("expected pagination block")
	}
	if int(pag["total"].(float64)) < 2 {
		t.Errorf("expected total>=2, got %v", pag["total"])
	}
	// Newest first — data[0] should be reg2.
	if data[0]["user_email"] != "reg2@example.com" {
		t.Errorf("expected newest first (reg2), got %v", data[0]["user_email"])
	}
}

func TestAdmin_ListRegistrations_Pagination(t *testing.T) {
	h, s := setupAdminTest(t)

	for i := 0; i < 4; i++ {
		email := string(rune('a'+i)) + "@pag.test"
		if _, _, err := s.RegisterUser(email, "h", "U"); err != nil {
			t.Fatalf("RegisterUser: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/registrations?limit=2&offset=0", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	data, pag := envelopeList(t, rec.Body.Bytes())
	if len(data) != 2 {
		t.Errorf("expected 2 rows, got %d", len(data))
	}
	if int(pag["total"].(float64)) != 4 {
		t.Errorf("expected total=4, got %v", pag["total"])
	}
	if int(pag["limit"].(float64)) != 2 {
		t.Errorf("expected limit=2, got %v", pag["limit"])
	}
}

// --- Ensure adjacent routes still work (no routing conflict from {id}) --------

func TestAdmin_UserDetail_DoesNotShadowActivate(t *testing.T) {
	h, s := setupAdminTest(t)
	uid, _ := s.CreateUser("shadow@example.com", "h", "Shadow")
	if err := s.SetUserActive(uid, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(uid, 10)+"/activate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected activate to still return 200, got %d", rec.Code)
	}
}

// --- Session auth for new endpoints -------------------------------------------

func TestAdmin_GetUser_SessionAuth(t *testing.T) {
	h, s := setupAdminTest(t)

	token := createSession(t, s, "detail-sess", "admin", time.Hour, true)
	target, _ := s.CreateUser("target@example.com", "h", "Target")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/"+strconv.FormatInt(target, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via session auth, got %d: %s", rec.Code, rec.Body.String())
	}
	_ = envelopeData(t, rec.Body.Bytes())
}

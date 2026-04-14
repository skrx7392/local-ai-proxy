package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// --- Admin handlers: list/grant/create on accounts + pricing + registration tokens ---

func TestAdmin_ListAccounts(t *testing.T) {
	h, s := setupAdminTest(t)

	accountID, err := s.CreateAccount("acc-for-list", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	_ = s.InitCreditBalance(accountID)
	_ = s.AddCredits(accountID, 25.5, "seed")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/accounts", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) == 0 {
		t.Fatal("expected at least one account")
	}
	// Balance and available must be present.
	if _, ok := list[0]["balance"]; !ok {
		t.Error("expected balance field in accounts response")
	}
}

func TestAdmin_GrantCredits(t *testing.T) {
	h, s := setupAdminTest(t)

	accountID, err := s.CreateAccount("grant-target", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := s.InitCreditBalance(accountID); err != nil {
		t.Fatalf("InitCreditBalance: %v", err)
	}

	body := `{"amount":12.5,"description":"test grant"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/credits", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "granted" {
		t.Errorf("expected status=granted, got %v", resp["status"])
	}
}

func TestAdmin_GrantCredits_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/not-a-number/credits", bytes.NewBufferString(`{"amount":1}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid account id, got %d", rec.Code)
	}
}

func TestAdmin_GrantCredits_ZeroAmount(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/credits", bytes.NewBufferString(`{"amount":0}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for zero amount, got %d", rec.Code)
	}
}

func TestAdmin_GrantCredits_InvalidJSON(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/credits", bytes.NewBufferString("{bad"))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad json, got %d", rec.Code)
	}
}

func TestAdmin_CreateAccountKey_Success(t *testing.T) {
	h, s := setupAdminTest(t)

	accountID, err := s.CreateAccount("account-key-target", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	body := `{"name":"svc-key","rate_limit":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Name != "svc-key" || resp.RateLimit != 60 || resp.Key == "" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestAdmin_CreateAccountKey_MissingName(t *testing.T) {
	h, s := setupAdminTest(t)

	accountID, _ := s.CreateAccount("missing-name-acc", "personal")

	body := `{"rate_limit":60}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", rec.Code)
	}
}

func TestAdmin_CreateAccountKey_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/abc/keys", bytes.NewBufferString(`{"name":"x"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid id, got %d", rec.Code)
	}
}

func TestAdmin_CreateAccountKey_InvalidJSON(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/keys", bytes.NewBufferString("not-json"))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid json, got %d", rec.Code)
	}
}

// --- Registration tokens ---

func TestAdmin_CreateRegistrationToken_Success(t *testing.T) {
	h, _ := setupAdminTest(t)
	expiry := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	body := `{"name":"rtk","credit_grant":5,"max_uses":3,"expires_at":"` + expiry + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["name"] != "rtk" {
		t.Errorf("unexpected name: %v", resp["name"])
	}
	if resp["token"] == "" {
		t.Error("expected non-empty token")
	}
}

func TestAdmin_CreateRegistrationToken_DefaultMaxUses(t *testing.T) {
	h, _ := setupAdminTest(t)
	body := `{"name":"default-uses"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 (default max_uses), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if mu, _ := resp["max_uses"].(float64); mu != 1 {
		t.Errorf("expected default max_uses=1, got %v", resp["max_uses"])
	}
}

func TestAdmin_CreateRegistrationToken_MissingName(t *testing.T) {
	h, _ := setupAdminTest(t)
	body := `{"credit_grant":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", rec.Code)
	}
}

func TestAdmin_CreateRegistrationToken_InvalidJSON(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString("{bad"))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestAdmin_CreateRegistrationToken_InvalidExpiresAt(t *testing.T) {
	h, _ := setupAdminTest(t)
	body := `{"name":"x","expires_at":"not-a-date"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid expires_at, got %d", rec.Code)
	}
}

func TestAdmin_ListRegistrationTokens(t *testing.T) {
	h, _ := setupAdminTest(t)

	// Seed via the admin endpoint.
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString(`{"name":"seed"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed failed: %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/registration-tokens", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) == 0 {
		t.Error("expected at least one registration token")
	}
}

func TestAdmin_RevokeRegistrationToken(t *testing.T) {
	h, _ := setupAdminTest(t)

	// Create
	req := httptest.NewRequest(http.MethodPost, "/api/admin/registration-tokens", bytes.NewBufferString(`{"name":"to-revoke"}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := int64(created["id"].(float64))

	// Revoke
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/registration-tokens/"+strconv.FormatInt(id, 10), nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_RevokeRegistrationToken_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/registration-tokens/999999", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestAdmin_RevokeRegistrationToken_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/registration-tokens/not-a-number", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// --- Pricing ---

func TestAdmin_UpsertListDeletePricing(t *testing.T) {
	h, _ := setupAdminTest(t)

	upsert := `{"model_id":"llama3","prompt_rate":0.001,"completion_rate":0.002,"typical_completion":200}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing", bytes.NewBufferString(upsert))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/admin/pricing", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list pricing: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var pricing []store.CreditPricing
	_ = json.Unmarshal(rec.Body.Bytes(), &pricing)
	if len(pricing) == 0 {
		t.Fatal("expected at least one pricing row")
	}
	id := pricing[0].ID

	// Delete
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/pricing/"+strconv.FormatInt(id, 10), nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete pricing: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_UpsertPricing_MissingModelID(t *testing.T) {
	h, _ := setupAdminTest(t)
	body := `{"prompt_rate":0.001,"completion_rate":0.002}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestAdmin_UpsertPricing_InvalidRates(t *testing.T) {
	h, _ := setupAdminTest(t)
	body := `{"model_id":"x","prompt_rate":0,"completion_rate":0.001}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestAdmin_UpsertPricing_InvalidJSON(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/pricing", bytes.NewBufferString("{bad"))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestAdmin_DeletePricing_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/pricing/999999", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestAdmin_DeletePricing_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/pricing/abc", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// --- Session limits ---

func TestAdmin_SetSessionLimit_Success(t *testing.T) {
	h, s := setupAdminTest(t)
	keyID, _ := s.CreateKey("limit-key", "hash-xyz", "sk-pref", 60)

	limit := 100
	body, _ := json.Marshal(map[string]*int{"limit": &limit})
	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(keyID, 10)+"/session-limit", bytes.NewBuffer(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_SetSessionLimit_Remove(t *testing.T) {
	h, s := setupAdminTest(t)
	keyID, _ := s.CreateKey("remove-limit", "hash-abc", "sk-pref", 60)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(keyID, 10)+"/session-limit", bytes.NewBufferString(`{"limit":null}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_SetSessionLimit_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/999999/session-limit", bytes.NewBufferString(`{"limit":100}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestAdmin_SetSessionLimit_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/abc/session-limit", bytes.NewBufferString(`{"limit":100}`))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestAdmin_SetSessionLimit_InvalidJSON(t *testing.T) {
	h, _ := setupAdminTest(t)
	req := httptest.NewRequest(http.MethodPut, "/api/admin/keys/1/session-limit", bytes.NewBufferString("bad"))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// --- Context / accessor coverage ---

func TestAdmin_AdminSessionFromContext(t *testing.T) {
	ctx := context.Background()
	if got := AdminSessionFromContext(ctx); got != nil {
		t.Errorf("expected nil for empty context, got %+v", got)
	}
	sess := &store.Session{ID: 42}
	ctx = context.WithValue(ctx, adminSessionCtxKey{}, sess)
	if got := AdminSessionFromContext(ctx); got == nil || got.ID != 42 {
		t.Errorf("expected session with ID=42, got %+v", got)
	}
}

func TestSessionLimiter_Prune(t *testing.T) {
	lim := &sessionLimiter{buckets: make(map[string]*sessionBucket)}
	old := &sessionBucket{
		tokens:     5,
		lastAccess: time.Now().Add(-20 * time.Minute),
		lastRefill: time.Now().Add(-20 * time.Minute),
	}
	fresh := &sessionBucket{
		tokens:     5,
		lastAccess: time.Now(),
		lastRefill: time.Now(),
	}
	lim.buckets["old"] = old
	lim.buckets["fresh"] = fresh

	lim.prune()

	if _, ok := lim.buckets["old"]; ok {
		t.Error("expected old bucket to be pruned")
	}
	if _, ok := lim.buckets["fresh"]; !ok {
		t.Error("fresh bucket should be retained")
	}
}

func TestSessionLimiter_RefillOverTime(t *testing.T) {
	lim := &sessionLimiter{buckets: make(map[string]*sessionBucket)}
	key := "refill-test"

	// Drain
	for i := 0; i < PerSessionRateLimit; i++ {
		if !lim.Allow(key) {
			t.Fatalf("should allow %d requests, failed at %d", PerSessionRateLimit, i)
		}
	}
	if lim.Allow(key) {
		t.Fatal("bucket should be empty")
	}

	// Rewind so the next Allow refills enough to unblock one request.
	b := lim.buckets[key]
	b.lastRefill = time.Now().Add(-1 * time.Second) // 5 tokens/sec refill
	if !lim.Allow(key) {
		t.Error("expected allow after 1s worth of refill")
	}
}

// --- Store-error fallbacks: close the store's pool mid-test so every
// subsequent store call errors, exercising the "slog.Error + 500" branches
// that happy-path tests never reach. Each helper gets a fresh handler so
// the 10 req/min X-Admin-Key bucket isn't exhausted. ---

func withBrokenStore(t *testing.T, fn func(h http.Handler, s *store.Store, userID, accountID, keyID int64)) {
	t.Helper()
	h, s := setupAdminTest(t)
	userID, err := s.CreateUser("pc@example.com", "hash", "PC")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	accountID, err := s.CreateAccount("pc-acc", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	keyID, err := s.CreateKey("pc-key", "pc-hash", "sk-pc", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	s.Pool().Close()
	fn(h, s, userID, accountID, keyID)
}

func expect5xx(t *testing.T, h http.Handler, method, path, body string) {
	t.Helper()
	var buf *bytes.Buffer
	if body != "" {
		buf = bytes.NewBufferString(body)
	} else {
		buf = bytes.NewBufferString("")
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < 500 {
		t.Errorf("expected 5xx with closed store, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_Internal_CreateKey(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodPost, "/api/admin/keys", `{"name":"x","rate_limit":60}`)
	})
}
func TestAdmin_Internal_ListKeys(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodGet, "/api/admin/keys", "")
	})
}
func TestAdmin_Internal_RevokeKey(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, keyID int64) {
		expect5xx(t, h, http.MethodDelete, "/api/admin/keys/"+strconv.FormatInt(keyID, 10), "")
	})
}
func TestAdmin_Internal_GetUsage(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodGet, "/api/admin/usage", "")
	})
}
func TestAdmin_Internal_ListUsers(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodGet, "/api/admin/users", "")
	})
}
func TestAdmin_Internal_Deactivate(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, userID, _, _ int64) {
		expect5xx(t, h, http.MethodPut, "/api/admin/users/"+strconv.FormatInt(userID, 10)+"/deactivate", "")
	})
}
func TestAdmin_Internal_Activate(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, userID, _, _ int64) {
		expect5xx(t, h, http.MethodPut, "/api/admin/users/"+strconv.FormatInt(userID, 10)+"/activate", "")
	})
}
func TestAdmin_Internal_ListAccounts(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodGet, "/api/admin/accounts", "")
	})
}
func TestAdmin_Internal_GrantCredits(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, accountID, _ int64) {
		expect5xx(t, h, http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/credits", `{"amount":1}`)
	})
}
func TestAdmin_Internal_CreateAccountKey(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, accountID, _ int64) {
		expect5xx(t, h, http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/keys", `{"name":"x"}`)
	})
}
func TestAdmin_Internal_CreateRegistrationToken(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodPost, "/api/admin/registration-tokens", `{"name":"t"}`)
	})
}
func TestAdmin_Internal_ListRegistrationTokens(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodGet, "/api/admin/registration-tokens", "")
	})
}
func TestAdmin_Internal_ListPricing(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodGet, "/api/admin/pricing", "")
	})
}
func TestAdmin_Internal_UpsertPricing(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, _ int64) {
		expect5xx(t, h, http.MethodPost, "/api/admin/pricing", `{"model_id":"m","prompt_rate":0.001,"completion_rate":0.001}`)
	})
}
func TestAdmin_Internal_SetSessionLimit(t *testing.T) {
	withBrokenStore(t, func(h http.Handler, _ *store.Store, _, _, keyID int64) {
		expect5xx(t, h, http.MethodPut, "/api/admin/keys/"+strconv.FormatInt(keyID, 10)+"/session-limit", `{"limit":100}`)
	})
}

func TestSessionLimiter_HitsCap(t *testing.T) {
	lim := &sessionLimiter{buckets: make(map[string]*sessionBucket)}
	key := "cap-test"

	// Seed with very old lastRefill so huge elapsed time would otherwise
	// add tons of tokens. The cap prevents overflow.
	lim.buckets[key] = &sessionBucket{
		tokens:     0,
		lastRefill: time.Now().Add(-1 * time.Hour),
		lastAccess: time.Now().Add(-1 * time.Hour),
	}
	lim.Allow(key) // triggers refill

	got := lim.buckets[key].tokens
	if got < 0 || got > float64(PerSessionRateLimit) {
		t.Errorf("tokens should be within [0, %d], got %v", PerSessionRateLimit, got)
	}
}

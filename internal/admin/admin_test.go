package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/krishna/local-ai-proxy/internal/store"
)

const testAdminKey = "test-admin-secret-key"

func setupAdminTest(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping admin integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	wipe := func(p *pgxpool.Pool) {
		c := context.Background()
		_, _ = p.Exec(c, "DELETE FROM registration_events")
		_, _ = p.Exec(c, "DELETE FROM credit_holds")
		_, _ = p.Exec(c, "DELETE FROM credit_transactions")
		_, _ = p.Exec(c, "DELETE FROM account_usage_stats")
		_, _ = p.Exec(c, "DELETE FROM credit_balances")
		_, _ = p.Exec(c, "DELETE FROM credit_pricing")
		_, _ = p.Exec(c, "DELETE FROM registration_tokens")
		_, _ = p.Exec(c, "DELETE FROM usage_logs")
		_, _ = p.Exec(c, "DELETE FROM user_sessions")
		_, _ = p.Exec(c, "DELETE FROM api_keys")
		_, _ = p.Exec(c, "DELETE FROM users")
		_, _ = p.Exec(c, "DELETE FROM accounts")
	}

	// Clean state before and after each test, regardless of pass/fail, so
	// no data leaks between test invocations or across `go test` runs.
	pool := s.Pool()
	wipe(pool)

	t.Cleanup(func() {
		wipe(s.Pool())
		s.Close()
	})

	usageCh := make(chan store.UsageEntry, 100)
	h := NewHandler(s, testAdminKey, usageCh, Options{})
	return h, s
}

func TestAdmin_MissingAdminKey(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without admin key, got %d", rec.Code)
	}
}

func TestAdmin_WrongAdminKey(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("X-Admin-Key", "wrong-key")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong admin key, got %d", rec.Code)
	}
}

func TestAdmin_CreateKey(t *testing.T) {
	h, _ := setupAdminTest(t)

	body := `{"name":"test-key","rate_limit":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Name != "test-key" {
		t.Errorf("expected name 'test-key', got %q", resp.Name)
	}
	if resp.RateLimit != 30 {
		t.Errorf("expected rate limit 30, got %d", resp.RateLimit)
	}
	if resp.Key == "" {
		t.Error("expected non-empty key")
	}
	if resp.ID <= 0 {
		t.Errorf("expected positive ID, got %d", resp.ID)
	}
}

func TestAdmin_CreateKey_DefaultRateLimit(t *testing.T) {
	h, _ := setupAdminTest(t)

	body := `{"name":"default-rl-key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RateLimit != 60 {
		t.Errorf("expected default rate limit 60, got %d", resp.RateLimit)
	}
}

func TestAdmin_CreateKey_MissingName(t *testing.T) {
	h, _ := setupAdminTest(t)

	body := `{"rate_limit":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d", rec.Code)
	}
}

func TestAdmin_CreateKey_InvalidJSON(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString("{invalid"))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rec.Code)
	}
}

func TestAdmin_ListKeys(t *testing.T) {
	h, _ := setupAdminTest(t)

	// Create a key first
	body := `{"name":"list-test-key","rate_limit":10}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	createReq.Header.Set("X-Admin-Key", testAdminKey)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)

	// List keys (envelope=0 → raw array for this legacy-shape assertion)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys?envelope=0", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var keys []keyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(keys) == 0 {
		t.Error("expected at least one key in list")
	}
}

func TestAdmin_RevokeKey(t *testing.T) {
	h, _ := setupAdminTest(t)

	// Create a key
	body := `{"name":"revoke-test-key","rate_limit":10}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	createReq.Header.Set("X-Admin-Key", testAdminKey)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)

	var created createKeyResponse
	json.Unmarshal(createRec.Body.Bytes(), &created)

	// Revoke it
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/keys/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["status"] != "revoked" {
		t.Errorf("expected status 'revoked', got %q", resp["status"])
	}
}

func TestAdmin_RevokeKey_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/admin/keys/999999", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent key, got %d", rec.Code)
	}
}

func TestAdmin_RevokeKey_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/admin/keys/not-a-number", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid ID, got %d", rec.Code)
	}
}

func TestAdmin_GetUsage(t *testing.T) {
	h, s := setupAdminTest(t)

	// Create a key and log some usage
	body := `{"name":"usage-test-key","rate_limit":10}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	createReq.Header.Set("X-Admin-Key", testAdminKey)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)

	var created createKeyResponse
	json.Unmarshal(createRec.Body.Bytes(), &created)

	// Log usage directly via store
	_ = s.LogUsage(store.UsageEntry{
		APIKeyID:         created.ID,
		Model:            "llama3",
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		DurationMs:       100,
		Status:           "completed",
	})

	// Get usage (envelope=0 → raw array for this legacy-shape assertion)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?envelope=0", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var stats []store.UsageStat
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	// Stats should include our logged entry
	if len(stats) == 0 {
		t.Error("expected at least one usage stat")
	}
}

func TestAdmin_GetUsage_WithKeyFilter(t *testing.T) {
	h, s := setupAdminTest(t)

	// Create a key and log usage
	body := `{"name":"usage-filter-key","rate_limit":10}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	createReq.Header.Set("X-Admin-Key", testAdminKey)
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)

	var created createKeyResponse
	json.Unmarshal(createRec.Body.Bytes(), &created)

	_ = s.LogUsage(store.UsageEntry{
		APIKeyID: created.ID, Model: "llama3",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		DurationMs: 100, Status: "completed",
	})

	// Get usage with key_id filter
	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?key_id="+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestAdmin_GetUsage_WithSinceFilter(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?since=2024-01-01", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for date-only since, got %d", rec.Code)
	}
}

func TestAdmin_GetUsage_WithRFC3339Since(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?since=2024-01-01T00:00:00Z", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for RFC3339 since, got %d", rec.Code)
	}
}

func TestAdmin_GetUsage_InvalidSince(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?since=not-a-date", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid since, got %d", rec.Code)
	}
}

func TestAdmin_GetUsage_InvalidKeyID(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?key_id=abc", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid key_id, got %d", rec.Code)
	}
}

func TestAdmin_ListUsers(t *testing.T) {
	h, s := setupAdminTest(t)

	// Create users directly in the store
	_, _ = s.CreateUser("admin-list1@example.com", "hash", "User One")
	_, _ = s.CreateUser("admin-list2@example.com", "hash", "User Two")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users?envelope=0", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var users []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(users) < 2 {
		t.Errorf("expected at least 2 users, got %d", len(users))
	}
}

func TestAdmin_DeactivateUser(t *testing.T) {
	h, s := setupAdminTest(t)

	id, _ := s.CreateUser("deactivate@example.com", "hash", "Deactivate Me")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(id, 10)+"/deactivate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	user, _ := s.GetUserByID(id)
	if user.IsActive {
		t.Error("expected user to be deactivated")
	}
}

func TestAdmin_ActivateUser(t *testing.T) {
	h, s := setupAdminTest(t)

	id, _ := s.CreateUser("activate@example.com", "hash", "Activate Me")
	_ = s.SetUserActive(id, false)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(id, 10)+"/activate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	user, _ := s.GetUserByID(id)
	if !user.IsActive {
		t.Error("expected user to be activated")
	}
}

func TestAdmin_ActivateUser_NotFound(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/999999/activate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for non-existent user, got %d", rec.Code)
	}
}

func TestAdmin_ActivateUser_InvalidID(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/not-a-number/activate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid ID, got %d", rec.Code)
	}
}

func TestAdmin_CreateKey_RateLimitCap(t *testing.T) {
	h, _ := setupAdminTest(t)

	body := `{"name":"capped-key","rate_limit":10001}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for rate_limit > 10000, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	errObj, _ := errResp["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "rate_limit_too_high" {
		t.Errorf("expected error code rate_limit_too_high, got %v", errObj["code"])
	}
}

func TestAdmin_CreateKey_RateLimitBoundary(t *testing.T) {
	h, _ := setupAdminTest(t)

	body := `{"name":"boundary-key","rate_limit":10000}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 at boundary 10000, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp createKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RateLimit != 10000 {
		t.Errorf("expected stored RateLimit 10000, got %d", resp.RateLimit)
	}
}

func TestAdmin_CreateAccountKey_RateLimitCap(t *testing.T) {
	h, s := setupAdminTest(t)

	accountID, err := s.CreateAccount("cap-test-account", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	body := `{"name":"too-fast","rate_limit":10001}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/keys", bytes.NewBufferString(body))
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for rate_limit > 10000 on account key, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_RateLimiting(t *testing.T) {
	h, _ := setupAdminTest(t)

	// The admin rate limiter allows 10 requests per minute.
	// Send 10 requests which should succeed, then the 11th should fail.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
		req.Header.Set("X-Admin-Key", testAdminKey)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d should not be rate limited", i+1)
		}
	}

	// 11th request should be rate limited
	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exhausting rate limit, got %d", rec.Code)
	}
}

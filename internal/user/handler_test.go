package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func setupUserTest(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping user integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(), "DROP TABLE IF EXISTS usage_logs")
		_, _ = s.Pool().Exec(context.Background(), "DROP TABLE IF EXISTS user_sessions")
		_, _ = s.Pool().Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS user_id")
		_, _ = s.Pool().Exec(context.Background(), "DROP TABLE IF EXISTS api_keys")
		_, _ = s.Pool().Exec(context.Background(), "DROP TABLE IF EXISTS users")
		s.Close()
	})

	// Clean state
	_, _ = s.Pool().Exec(ctx, "DELETE FROM usage_logs")
	_, _ = s.Pool().Exec(ctx, "DELETE FROM user_sessions")
	_, _ = s.Pool().Exec(ctx, "DELETE FROM api_keys")
	_, _ = s.Pool().Exec(ctx, "DELETE FROM users")

	h := NewHandler(s, 0)
	return h, s
}

// registerUser is a helper that registers a user and returns the session token from login.
func registerAndLogin(t *testing.T, h http.Handler, email, password, name string) string {
	t.Helper()

	// Register
	body := `{"email":"` + email + `","password":"` + password + `","name":"` + name + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register failed: %d %s", rec.Code, rec.Body.String())
	}

	// Login
	loginBody := `{"email":"` + email + `","password":"` + password + `"}`
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}

	var resp loginResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.Token
}

// --- Registration tests ---

func TestRegister_Success(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"test@example.com","password":"securepass123","name":"Test User"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["email"] != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %v", resp["email"])
	}
	if resp["name"] != "Test User" {
		t.Errorf("expected name 'Test User', got %v", resp["name"])
	}
}

func TestRegister_DuplicateEmail(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"dup@example.com","password":"securepass123","name":"First"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Register again with same email
	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate email, got %d", rec.Code)
	}
}

func TestRegister_MissingFields(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"no-pw@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", rec.Code)
	}
}

func TestRegister_WeakPassword(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"weak@example.com","password":"short","name":"Weak"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for weak password, got %d", rec.Code)
	}
}

func TestRegister_InvalidJSON(t *testing.T) {
	h, _ := setupUserTest(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString("{invalid"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rec.Code)
	}
}

// --- Login tests ---

func TestLogin_Success(t *testing.T) {
	h, _ := setupUserTest(t)

	// Register first
	body := `{"email":"login@example.com","password":"securepass123","name":"Login User"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Login
	loginBody := `{"email":"login@example.com","password":"securepass123"}`
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp loginResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Token == "" {
		t.Error("expected non-empty token")
	}
	if resp.ExpiresIn <= 0 {
		t.Errorf("expected positive expires_in, got %d", resp.ExpiresIn)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"wrong-pw@example.com","password":"securepass123","name":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	loginBody := `{"email":"wrong-pw@example.com","password":"wrongpassword"}`
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong password, got %d", rec.Code)
	}
}

func TestLogin_NonexistentEmail(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"noone@example.com","password":"securepass123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for nonexistent email, got %d", rec.Code)
	}
}

func TestLogin_MissingFields(t *testing.T) {
	h, _ := setupUserTest(t)

	body := `{"email":"partial@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing password, got %d", rec.Code)
	}
}

func TestLogin_DisabledAccount(t *testing.T) {
	h, s := setupUserTest(t)

	// Register
	body := `{"email":"disabled@example.com","password":"securepass123","name":"Disabled"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var regResp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &regResp)
	userID := int64(regResp["id"].(float64))

	// Deactivate user
	_ = s.SetUserActive(userID, false)

	// Try login
	loginBody := `{"email":"disabled@example.com","password":"securepass123"}`
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for disabled account, got %d", rec.Code)
	}
}

// --- Logout tests ---

func TestLogout_Success(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "logout@example.com", "securepass123", "Logout User")

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Token should no longer work
	req = httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 after logout, got %d", rec.Code)
	}
}

// --- Profile tests ---

func TestGetProfile(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "profile@example.com", "securepass123", "Profile User")

	req := httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp profileResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Email != "profile@example.com" {
		t.Errorf("expected email 'profile@example.com', got %q", resp.Email)
	}
	if resp.Name != "Profile User" {
		t.Errorf("expected name 'Profile User', got %q", resp.Name)
	}
	if resp.Role != "user" {
		t.Errorf("expected role 'user', got %q", resp.Role)
	}
}

func TestGetProfile_NoAuth(t *testing.T) {
	h, _ := setupUserTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", rec.Code)
	}
}

func TestGetProfile_XSessionToken(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "xheader@example.com", "securepass123", "XHeader User")

	req := httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	req.Header.Set("X-Session-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with X-Session-Token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateProfile(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "update-profile@example.com", "securepass123", "Original Name")

	body := `{"name":"Updated Name","email":"new-email@example.com"}`
	req := httptest.NewRequest(http.MethodPut, "/api/users/profile", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify by getting profile (need to re-login with new email)
	// Actually the session is still valid, just check profile
	req = httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp profileResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Name != "Updated Name" {
		t.Errorf("expected name 'Updated Name', got %q", resp.Name)
	}
	if resp.Email != "new-email@example.com" {
		t.Errorf("expected email 'new-email@example.com', got %q", resp.Email)
	}
}

// --- Password change tests ---

func TestChangePassword(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "changepw@example.com", "securepass123", "PW User")

	body := `{"old_password":"securepass123","new_password":"newsecurepass456"}`
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Login with new password should work
	loginBody := `{"email":"changepw@example.com","password":"newsecurepass456"}`
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with new password, got %d", rec.Code)
	}
}

func TestChangePassword_WrongOld(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "wrongold@example.com", "securepass123", "WrongOld")

	body := `{"old_password":"wrongpassword","new_password":"newsecurepass456"}`
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong old password, got %d", rec.Code)
	}
}

func TestChangePassword_WeakNew(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "weaknew@example.com", "securepass123", "WeakNew")

	body := `{"old_password":"securepass123","new_password":"short"}`
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for weak new password, got %d", rec.Code)
	}
}

// --- User API key tests ---

func TestCreateKey(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "keyowner@example.com", "securepass123", "Key Owner")

	body := `{"name":"my-key","rate_limit":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Name != "my-key" {
		t.Errorf("expected name 'my-key', got %q", resp.Name)
	}
	if resp.Key == "" {
		t.Error("expected non-empty key")
	}
	if resp.RateLimit != 30 {
		t.Errorf("expected rate_limit 30, got %d", resp.RateLimit)
	}
}

func TestCreateKey_DefaultRateLimit(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "default-rl@example.com", "securepass123", "Default RL")

	body := `{"name":"default-key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp createKeyResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RateLimit != 60 {
		t.Errorf("expected default rate_limit 60, got %d", resp.RateLimit)
	}
}

func TestListKeys(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "listkeys@example.com", "securepass123", "List Keys")

	// Create two keys
	for _, name := range []string{"key-a", "key-b"} {
		body := `{"name":"` + name + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/users/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var keys []keyResponse
	json.Unmarshal(rec.Body.Bytes(), &keys)
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestRevokeKey(t *testing.T) {
	h, _ := setupUserTest(t)

	token := registerAndLogin(t, h, "revokekey@example.com", "securepass123", "Revoke Key")

	// Create a key
	body := `{"name":"to-revoke"}`
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var created createKeyResponse
	json.Unmarshal(rec.Body.Bytes(), &created)

	// Revoke it
	req = httptest.NewRequest(http.MethodDelete, "/api/users/keys/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRevokeKey_NotOwned(t *testing.T) {
	h, _ := setupUserTest(t)

	// User A creates a key
	tokenA := registerAndLogin(t, h, "usera@example.com", "securepass123", "User A")
	body := `{"name":"a-key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tokenA)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var created createKeyResponse
	json.Unmarshal(rec.Body.Bytes(), &created)

	// User B tries to revoke it
	tokenB := registerAndLogin(t, h, "userb@example.com", "securepass123", "User B")
	req = httptest.NewRequest(http.MethodDelete, "/api/keys/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 when revoking another user's key, got %d", rec.Code)
	}
}

// --- Usage tests ---

func TestGetUsage(t *testing.T) {
	h, s := setupUserTest(t)

	token := registerAndLogin(t, h, "usage@example.com", "securepass123", "Usage User")

	// Create a key
	body := `{"name":"usage-key"}`
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var created createKeyResponse
	json.Unmarshal(rec.Body.Bytes(), &created)

	// Log some usage
	_ = s.LogUsage(store.UsageEntry{
		APIKeyID: created.ID, Model: "llama3",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		DurationMs: 100, Status: "completed",
	})

	// Get usage
	req = httptest.NewRequest(http.MethodGet, "/api/users/usage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var stats []store.UsageStat
	json.Unmarshal(rec.Body.Bytes(), &stats)
	if len(stats) == 0 {
		t.Error("expected at least one usage stat")
	}
}

// --- Middleware tests ---

func TestSessionMiddleware_InvalidToken(t *testing.T) {
	h, _ := setupUserTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", rec.Code)
	}
}

func TestSessionMiddleware_MissingToken(t *testing.T) {
	h, _ := setupUserTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing token, got %d", rec.Code)
	}
}

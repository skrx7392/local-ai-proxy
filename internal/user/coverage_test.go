package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// --- getCredits ---

func TestGetCredits_Success(t *testing.T) {
	h, s := setupUserTest(t)

	token := registerAndLogin(t, h, "credits-ok@example.com", "secretpass", "Credits")

	// Grant some credits to this user's account.
	u, _ := s.GetUserByEmail("credits-ok@example.com")
	_ = s.AddCredits(*u.AccountID, 42.0, "seed")

	req := httptest.NewRequest(http.MethodGet, "/api/users/credits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["balance"] == nil {
		t.Error("expected balance in response")
	}
}

func TestGetCredits_NoAccount(t *testing.T) {
	h, s := setupUserTest(t)

	// Manually create a user WITHOUT an account.
	userID, err := s.CreateUser("no-account@example.com", "$2a$10$HqXo8eJyUbKuf/lrPT9Zg.LwcO1NR6v4A0eWHjp9u8.0X8e.oAYai", "No Account")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Manually create an active session (skipping login because bcrypt hash above is dummy).
	rawToken := "fixed-token-for-noacct-test-0123456789abcdef"
	tokenHash := auth.HashKey(rawToken)
	if err := s.CreateSession(userID, tokenHash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/users/credits", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for no account, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- getCreditTransactions ---

func TestGetCreditTransactions_Success(t *testing.T) {
	h, s := setupUserTest(t)
	token := registerAndLogin(t, h, "txn@example.com", "secretpass", "Txn")
	u, _ := s.GetUserByEmail("txn@example.com")
	_ = s.AddCredits(*u.AccountID, 10.0, "one")
	_ = s.AddCredits(*u.AccountID, 5.0, "two")

	req := httptest.NewRequest(http.MethodGet, "/api/users/credits/transactions?limit=5&offset=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) < 2 {
		t.Errorf("expected at least 2 transactions, got %d", len(list))
	}
}

func TestGetCreditTransactions_InvalidLimitFallsBack(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "txn-invalid@example.com", "secretpass", "Txn")

	// invalid limit/offset → defaults
	req := httptest.NewRequest(http.MethodGet, "/api/users/credits/transactions?limit=abc&offset=def", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetCreditTransactions_NoAccount(t *testing.T) {
	h, s := setupUserTest(t)
	userID, _ := s.CreateUser("txn-no-acct@example.com", "hash", "X")
	raw := "txn-no-acct-token-0123456789abcdef01"
	_ = s.CreateSession(userID, auth.HashKey(raw), time.Now().Add(time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/api/users/credits/transactions", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// --- registerServiceAccount ---

func TestRegisterServiceAccount_Success(t *testing.T) {
	h, s := setupUserTest(t)

	rawToken := "reg-token-abc"
	tokenHash := auth.HashKey(rawToken)
	if _, err := s.CreateRegistrationToken("svc", tokenHash, 50.0, 2, nil); err != nil {
		t.Fatalf("CreateRegistrationToken: %v", err)
	}

	body := `{"registration_token":"` + rawToken + `","name":"my-service","rate_limit":100}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["api_key"] == "" {
		t.Error("expected api_key in response")
	}
}

func TestRegisterServiceAccount_MissingFields(t *testing.T) {
	h, _ := setupUserTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/register", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", rec.Code)
	}
}

func TestRegisterServiceAccount_InvalidJSON(t *testing.T) {
	h, _ := setupUserTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/register", bytes.NewBufferString("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestRegisterServiceAccount_RateLimitTooHigh(t *testing.T) {
	h, _ := setupUserTest(t)
	body := `{"registration_token":"anything","name":"svc","rate_limit":10001}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for rate_limit>10000, got %d", rec.Code)
	}
}

func TestRegisterServiceAccount_BadToken(t *testing.T) {
	h, _ := setupUserTest(t)
	body := `{"registration_token":"not-a-real-token","name":"svc"}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad token, got %d", rec.Code)
	}
}

// --- Context accessors and session helpers ---

func TestWithUserAndWithSession(t *testing.T) {
	u := &store.User{ID: 5, Email: "x@example.com"}
	ctx := WithUser(context.Background(), u)
	if got := UserFromContext(ctx); got == nil || got.ID != 5 {
		t.Errorf("UserFromContext mismatch: %+v", got)
	}

	sess := &store.Session{ID: 9}
	ctx2 := WithSession(context.Background(), sess)
	if got := SessionFromContext(ctx2); got == nil || got.ID != 9 {
		t.Errorf("SessionFromContext mismatch: %+v", got)
	}
}

func TestSessionExpiryHelpers(t *testing.T) {
	now := time.Now()
	got := sessionExpiry()
	if got.Before(now.Add(UserSessionDuration - time.Minute)) {
		t.Error("sessionExpiry too early")
	}

	adminT := SessionExpiryFor("admin")
	if adminT.Before(now.Add(AdminSessionDuration - time.Minute)) {
		t.Error("SessionExpiryFor(admin) too early")
	}
	userT := SessionExpiryFor("user")
	if userT.Before(now.Add(UserSessionDuration - time.Minute)) {
		t.Error("SessionExpiryFor(user) too early")
	}
}

// --- Direct-handler tests: cover the "no user/session in context" fallback
// branches that normal routed requests never hit because SessionMiddleware
// rejects them upstream. ---

func callHandlerDirect(t *testing.T, hfn func(*handler, http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	s := &store.Store{}
	h := &handler{store: s}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	hfn(h, rec, req)
	return rec
}

func TestLogin_InvalidJSON(t *testing.T) {
	h, _ := setupUserTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString("{not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rec.Code)
	}
}

func TestUpdateProfile_InvalidJSON(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "up-inv@example.com", "securepass1", "U")
	req := httptest.NewRequest(http.MethodPut, "/api/users/profile", bytes.NewBufferString("bad"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestUpdateProfile_OnlyEmail(t *testing.T) {
	// Updating only email leaves name unchanged — exercises the
	// "name == '' → keep current name" branch.
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "up-email@example.com", "securepass1", "Keep Me")
	body := `{"email":"up-email-renamed@example.com"}`
	req := httptest.NewRequest(http.MethodPut, "/api/users/profile", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateProfile_OnlyName(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "up-name@example.com", "securepass1", "Old Name")
	body := `{"name":"New Name"}`
	req := httptest.NewRequest(http.MethodPut, "/api/users/profile", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestChangePassword_InvalidJSON(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "cp-inv@example.com", "securepass1", "CP")
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewBufferString("nope"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestChangePassword_MissingFields(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "cp-miss@example.com", "securepass1", "CP")
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewBufferString(`{"old_password":"x"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestCreateKey_InvalidJSON(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "ck-inv@example.com", "securepass1", "CK")
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString("bad"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestCreateKey_MissingName(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "ck-name@example.com", "securepass1", "CK")
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(`{"rate_limit":60}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestCreateKey_RateLimitCap(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "ck-cap@example.com", "securepass1", "CK")
	req := httptest.NewRequest(http.MethodPost, "/api/users/keys", bytes.NewBufferString(`{"name":"x","rate_limit":10001}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for rate_limit>10000, got %d", rec.Code)
	}
}

func TestRevokeKey_InvalidID(t *testing.T) {
	h, _ := setupUserTest(t)
	token := registerAndLogin(t, h, "rk-inv@example.com", "securepass1", "RK")
	req := httptest.NewRequest(http.MethodDelete, "/api/users/keys/not-a-number", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandlers_RequireSessionFallbacks(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*handler, http.ResponseWriter, *http.Request)
	}{
		{"logout", func(h *handler, w http.ResponseWriter, r *http.Request) { h.logout(w, r) }},
		{"getProfile", func(h *handler, w http.ResponseWriter, r *http.Request) { h.getProfile(w, r) }},
		{"updateProfile", func(h *handler, w http.ResponseWriter, r *http.Request) { h.updateProfile(w, r) }},
		{"changePassword", func(h *handler, w http.ResponseWriter, r *http.Request) { h.changePassword(w, r) }},
		{"createKey", func(h *handler, w http.ResponseWriter, r *http.Request) { h.createKey(w, r) }},
		{"listKeys", func(h *handler, w http.ResponseWriter, r *http.Request) { h.listKeys(w, r) }},
		{"revokeKey", func(h *handler, w http.ResponseWriter, r *http.Request) { h.revokeKey(w, r) }},
		{"getUsage", func(h *handler, w http.ResponseWriter, r *http.Request) { h.getUsage(w, r) }},
		{"getCredits", func(h *handler, w http.ResponseWriter, r *http.Request) { h.getCredits(w, r) }},
		{"getCreditTransactions", func(h *handler, w http.ResponseWriter, r *http.Request) { h.getCreditTransactions(w, r) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := callHandlerDirect(t, tc.fn)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: expected 401 when no user/session in context, got %d", tc.name, rec.Code)
			}
		})
	}
}

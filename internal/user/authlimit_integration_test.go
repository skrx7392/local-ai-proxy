package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/authlimit"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// setupGuardedUserTest mirrors setupUserTest but builds the handler with an
// authlimit.Guard on a controllable clock.
func setupGuardedUserTest(t *testing.T, cfg authlimit.Config) (http.Handler, *authlimit.Guard, *time.Time) {
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

	wipe := func() {
		c := context.Background()
		_, _ = s.Pool().Exec(c, "DELETE FROM registration_events")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_holds")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_transactions")
		_, _ = s.Pool().Exec(c, "DELETE FROM account_usage_stats")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_balances")
		_, _ = s.Pool().Exec(c, "DELETE FROM credit_pricing")
		_, _ = s.Pool().Exec(c, "DELETE FROM registration_tokens")
		_, _ = s.Pool().Exec(c, "DELETE FROM usage_logs")
		_, _ = s.Pool().Exec(c, "DELETE FROM user_sessions")
		_, _ = s.Pool().Exec(c, "DELETE FROM api_keys")
		_, _ = s.Pool().Exec(c, "DELETE FROM users")
		_, _ = s.Pool().Exec(c, "DELETE FROM federated_identities")
		_, _ = s.Pool().Exec(c, "DELETE FROM accounts")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})

	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	guard := authlimit.NewWithClock(cfg, func() time.Time { return now })
	h := NewHandler(s, 0, nil, guard)
	return h, guard, &now
}

func postJSON(t *testing.T, h http.Handler, path string, payload any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mustRegister(t *testing.T, h http.Handler, email, password string) {
	t.Helper()
	rec := postJSON(t, h, "/api/auth/register",
		registerRequest{Email: email, Password: password, Name: "Guard Test"}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLogin_PerEmailThrottle(t *testing.T) {
	h, _, now := setupGuardedUserTest(t, authlimit.Config{LoginPerMinEmail: 2})
	mustRegister(t, h, "throttle@example.com", "correct-password")

	// The first N attempts reach the bcrypt path (visible as 401 for a wrong
	// password), regardless of which IP they come from.
	for i := 0; i < 2; i++ {
		rec := postJSON(t, h, "/api/auth/login",
			loginRequest{Email: "throttle@example.com", Password: "wrong-password"},
			map[string]string{"X-Forwarded-For": "203.0.113.7"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, rec.Code)
		}
	}

	// Attempt N+1 is throttled even from a rotated IP.
	rec := postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "throttle@example.com", Password: "wrong-password"},
		map[string]string{"X-Forwarded-For": "198.51.100.99"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt: got %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("throttled login must set Retry-After")
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 body not JSON: %v", err)
	}
	if body.Error.Code != "rate_limit_exceeded" {
		t.Errorf("error code = %q, want rate_limit_exceeded", body.Error.Code)
	}

	// A different email is unaffected.
	rec = postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "other@example.com", Password: "wrong-password"}, nil)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("different email must not be throttled")
	}

	// After the window passes, the email may try again.
	*now = now.Add(31 * time.Second) // 2/min -> one token per 30s
	rec = postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "throttle@example.com", Password: "correct-password"}, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("post-window attempt: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestLogin_BcryptSemaphoreSaturated(t *testing.T) {
	h, guard, _ := setupGuardedUserTest(t, authlimit.Config{BcryptConcurrency: 1})
	mustRegister(t, h, "busy@example.com", "some-password")

	// Occupy the only bcrypt slot, then attempt a login: the handler must
	// reject with 429 rather than queueing unbounded bcrypt work.
	if !guard.TryAcquireBcrypt() {
		t.Fatal("test could not occupy the bcrypt slot")
	}
	rec := postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "busy@example.com", Password: "some-password"}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated login: got %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("saturated login must set Retry-After")
	}

	// Once released, the same login succeeds.
	guard.ReleaseBcrypt()
	rec = postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "busy@example.com", Password: "some-password"}, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("post-release login: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRegister_BcryptSemaphoreSaturated(t *testing.T) {
	h, guard, _ := setupGuardedUserTest(t, authlimit.Config{BcryptConcurrency: 1})

	if !guard.TryAcquireBcrypt() {
		t.Fatal("test could not occupy the bcrypt slot")
	}
	rec := postJSON(t, h, "/api/auth/register",
		registerRequest{Email: "new@example.com", Password: "some-password", Name: "N"}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated register: got %d, want 429; body=%s", rec.Code, rec.Body.String())
	}

	guard.ReleaseBcrypt()
	rec = postJSON(t, h, "/api/auth/register",
		registerRequest{Email: "new@example.com", Password: "some-password", Name: "N"}, nil)
	if rec.Code != http.StatusCreated {
		t.Errorf("post-release register: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestChangePassword_BcryptSemaphoreSaturated(t *testing.T) {
	h, guard, _ := setupGuardedUserTest(t, authlimit.Config{BcryptConcurrency: 1})
	mustRegister(t, h, "changer@example.com", "old-password-1")

	rec := postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "changer@example.com", Password: "old-password-1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d body=%s", rec.Code, rec.Body.String())
	}
	var lr loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
		t.Fatalf("login body: %v", err)
	}

	if !guard.TryAcquireBcrypt() {
		t.Fatal("test could not occupy the bcrypt slot")
	}
	body, _ := json.Marshal(changePasswordRequest{OldPassword: "old-password-1", NewPassword: "new-password-2"})
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+lr.Token)
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, req)
	if pw.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated password change: got %d, want 429; body=%s", pw.Code, pw.Body.String())
	}
	guard.ReleaseBcrypt()
}

func TestLogin_NilGuardUnlimited(t *testing.T) {
	// The plain setup passes a nil guard; hammering login must never 429.
	h, _ := setupUserTest(t)
	mustRegister(t, h, "unlimited@example.com", "some-password")

	for i := 0; i < 10; i++ {
		rec := postJSON(t, h, "/api/auth/login",
			loginRequest{Email: "unlimited@example.com", Password: "wrong"}, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i+1, rec.Code)
		}
	}
}

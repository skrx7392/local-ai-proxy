package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func loginFor(t *testing.T, h http.Handler, email, password string) string {
	t.Helper()
	rec := postJSON(t, h, "/api/auth/login", loginRequest{Email: email, Password: password}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d body=%s", rec.Code, rec.Body.String())
	}
	var lr loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
		t.Fatalf("login body: %v", err)
	}
	return lr.Token
}

func getProfileStatus(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/users/profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestChangePassword_RevokesOtherSessionsKeepsCurrent(t *testing.T) {
	h, _ := setupUserTest(t)
	mustRegister(t, h, "revoke@example.com", "old-password-1")

	tokenCurrent := loginFor(t, h, "revoke@example.com", "old-password-1")
	tokenOther := loginFor(t, h, "revoke@example.com", "old-password-1")

	// Sanity: both sessions work before the change.
	if got := getProfileStatus(t, h, tokenOther); got != http.StatusOK {
		t.Fatalf("other session pre-change: got %d, want 200", got)
	}

	body, _ := json.Marshal(changePasswordRequest{OldPassword: "old-password-1", NewPassword: "new-password-2"})
	req := httptest.NewRequest(http.MethodPut, "/api/users/password", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenCurrent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: got %d body=%s", rec.Code, rec.Body.String())
	}

	// The stolen/other session is dead; the session that changed the
	// password stays logged in.
	if got := getProfileStatus(t, h, tokenOther); got != http.StatusUnauthorized {
		t.Errorf("other session post-change: got %d, want 401", got)
	}
	if got := getProfileStatus(t, h, tokenCurrent); got != http.StatusOK {
		t.Errorf("current session post-change: got %d, want 200", got)
	}

	// New password works; old one does not.
	rec = postJSON(t, h, "/api/auth/login", loginRequest{Email: "revoke@example.com", Password: "new-password-2"}, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("login with new password: got %d", rec.Code)
	}
	rec = postJSON(t, h, "/api/auth/login", loginRequest{Email: "revoke@example.com", Password: "old-password-1"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("login with old password: got %d, want 401", rec.Code)
	}
}

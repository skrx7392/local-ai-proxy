package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// uniqueEmail returns an email guaranteed not to collide with other tests in
// the same suite (setupAdminTest does not truncate the DB between tests).
func uniqueEmail(t *testing.T, label string) string {
	t.Helper()
	buf := make([]byte, 6)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%s-%d-%s@example.com", label, time.Now().UnixNano(), hex.EncodeToString(buf))
}

// createSession creates a user with the given role, an active session, and
// returns the raw session token.
func createSession(t *testing.T, s *store.Store, label, role string, expiresIn time.Duration, active bool) string {
	t.Helper()
	email := uniqueEmail(t, label)
	userID, err := s.CreateUser(email, "hash", "Test")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if role != "user" {
		_, err := s.Pool().Exec(context.Background(), `UPDATE users SET role = $1 WHERE id = $2`, role, userID)
		if err != nil {
			t.Fatalf("set role: %v", err)
		}
	}
	if !active {
		if err := s.SetUserActive(userID, false); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
	}

	raw := randomToken(t)
	hash := auth.HashKey(raw)
	expiresAt := time.Now().Add(expiresIn)
	if err := s.CreateSession(userID, hash, expiresAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return raw
}

func randomToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

func TestAdmin_Bearer_AdminSessionAllowed(t *testing.T) {
	h, s := setupAdminTest(t)

	token := createSession(t, s, "admin-bearer-ok@example.com", "admin", time.Hour, true)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with admin Bearer token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_Bearer_NonAdminForbidden(t *testing.T) {
	h, s := setupAdminTest(t)

	token := createSession(t, s, "non-admin@example.com", "user", time.Hour, true)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin role Bearer, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_Bearer_DisabledAdminForbidden(t *testing.T) {
	h, s := setupAdminTest(t)

	token := createSession(t, s, "disabled-admin@example.com", "admin", time.Hour, false)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled admin, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_Bearer_ExpiredSession(t *testing.T) {
	h, s := setupAdminTest(t)

	// Session that's already expired
	token := createSession(t, s, "expired-admin@example.com", "admin", -time.Minute, true)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired session, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_Bearer_UnknownSession(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown session token, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_BothHeaders_XAdminKeyWins(t *testing.T) {
	h, s := setupAdminTest(t)

	// Non-admin Bearer (would 403) + valid X-Admin-Key → should pass via X-Admin-Key
	token := createSession(t, s, "hybrid@example.com", "user", time.Hour, true)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when X-Admin-Key is valid (bearer non-admin ignored), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdmin_Bearer_PerSessionRateLimit(t *testing.T) {
	h, s := setupAdminTest(t)

	token := createSession(t, s, "ratelimit-admin@example.com", "admin", time.Hour, true)

	// Per-session bucket is 300/min. Exhaust the bucket then confirm 429.
	for i := 0; i < 300; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d should not be rate limited yet", i+1)
		}
	}

	// The 301st should be rate-limited
	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exhausting per-session bucket, got %d", rec.Code)
	}
}

func TestAdmin_Bearer_BucketsAreIsolatedPerSession(t *testing.T) {
	h, s := setupAdminTest(t)

	token1 := createSession(t, s, "iso1@example.com", "admin", time.Hour, true)
	token2 := createSession(t, s, "iso2@example.com", "admin", time.Hour, true)

	// Exhaust token1's bucket
	for i := 0; i < 300; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
		req.Header.Set("Authorization", "Bearer "+token1)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	// token2 should still be able to make requests
	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token2)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("token2 should be unaffected by token1's rate limit; got %d", rec.Code)
	}
}

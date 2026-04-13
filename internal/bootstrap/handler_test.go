package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

const testBootstrapToken = "bootstrap-secret-token-for-tests"

func setupBootstrapTest(t *testing.T, tokenOverride *string) (http.Handler, *store.Store) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping bootstrap integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	pool := s.Pool()
	wipe := func() {
		c := context.Background()
		_, _ = pool.Exec(c, "DELETE FROM registration_events")
		_, _ = pool.Exec(c, "DELETE FROM credit_holds")
		_, _ = pool.Exec(c, "DELETE FROM credit_transactions")
		_, _ = pool.Exec(c, "DELETE FROM account_usage_stats")
		_, _ = pool.Exec(c, "DELETE FROM credit_balances")
		_, _ = pool.Exec(c, "DELETE FROM credit_pricing")
		_, _ = pool.Exec(c, "DELETE FROM registration_tokens")
		_, _ = pool.Exec(c, "DELETE FROM usage_logs")
		_, _ = pool.Exec(c, "DELETE FROM user_sessions")
		_, _ = pool.Exec(c, "DELETE FROM api_keys")
		_, _ = pool.Exec(c, "DELETE FROM users")
		_, _ = pool.Exec(c, "DELETE FROM accounts")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})

	tok := testBootstrapToken
	if tokenOverride != nil {
		tok = *tokenOverride
	}
	return New(s, tok), s
}

func TestBootstrap_DisabledWhenTokenEmpty(t *testing.T) {
	empty := ""
	h, _ := setupBootstrapTest(t, &empty)

	body := `{"token":"anything","email":"a@b.com","password":"password1","name":"N"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when ADMIN_BOOTSTRAP_TOKEN unset, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_Success(t *testing.T) {
	h, s := setupBootstrapTest(t, nil)

	body := `{"token":"` + testBootstrapToken + `","email":"bootstrap-ok@example.com","password":"supersecret","name":"Root"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp bootstrapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Email != "bootstrap-ok@example.com" {
		t.Errorf("unexpected email: %q", resp.Email)
	}
	if resp.ID == 0 {
		t.Error("expected non-zero user id")
	}

	// Verify admin role and active status in the DB.
	u, err := s.GetUserByID(resp.ID)
	if err != nil || u == nil {
		t.Fatalf("load user: %v", err)
	}
	if u.Role != "admin" {
		t.Errorf("expected role admin, got %q", u.Role)
	}
	if !u.IsActive {
		t.Error("expected is_active=true")
	}

	// Verify registration_events row exists.
	var count int
	err = s.Pool().QueryRow(context.Background(),
		`SELECT COUNT(*) FROM registration_events WHERE source = 'admin_bootstrap' AND user_id = $1`,
		resp.ID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count registration_events: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 registration_events row, got %d", count)
	}
}

func TestBootstrap_InvalidToken(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	body := `{"token":"wrong","email":"x@y.com","password":"password1","name":"X"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong bootstrap token, got %d", rec.Code)
	}
}

func TestBootstrap_InvalidJSON(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString("{not-json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", rec.Code)
	}
}

func TestBootstrap_MissingFields(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	body := `{"token":"` + testBootstrapToken + `","email":"x@y.com"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing fields, got %d", rec.Code)
	}
}

func TestBootstrap_WeakPassword(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	body := `{"token":"` + testBootstrapToken + `","email":"x@y.com","password":"short","name":"N"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for weak password, got %d", rec.Code)
	}
}

func TestBootstrap_DuplicateEmail(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	body := `{"token":"` + testBootstrapToken + `","email":"dup@example.com","password":"password1","name":"First"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: got %d: %s", rec.Code, rec.Body.String())
	}

	// Second attempt with same email → 409
	req = httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate email, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_Reusable(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	// First call: create admin
	body1 := `{"token":"` + testBootstrapToken + `","email":"reuse1@example.com","password":"password1","name":"First"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body1))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first call: got %d: %s", rec.Code, rec.Body.String())
	}

	// Second call with DIFFERENT email, SAME token → also 201
	body2 := `{"token":"` + testBootstrapToken + `","email":"reuse2@example.com","password":"password1","name":"Second"}`
	req = httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body2))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("second call should succeed (reusable endpoint), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrap_RateLimit(t *testing.T) {
	h, _ := setupBootstrapTest(t, nil)

	// 5 calls allowed per minute, 6th should be 429. Use invalid tokens so
	// we don't fail on duplicate emails once we exceed the bucket.
	body := `{"token":"wrong","email":"rl@example.com","password":"password1","name":"X"}`
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d should not be rate limited yet", i+1)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/bootstrap", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 after exhausting bootstrap bucket, got %d", rec.Code)
	}
}

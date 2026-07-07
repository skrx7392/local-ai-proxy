package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/krishna/local-ai-proxy/internal/authlimit"
)

// loginWith drives h.login directly (tests live in package user, so the
// unexported handler is constructable with a countable comparator).
func loginWith(t *testing.T, h *handler, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Email: email, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.login(rec, req)
	return rec
}

// Timing assertions are flaky; instead count comparator invocations and
// assert the unknown-email path does the same bcrypt work as the
// wrong-password path.
func TestLogin_UnknownEmailStillRunsComparison(t *testing.T) {
	_, s := setupUserTest(t)

	calls := 0
	h := &handler{store: s, comparePassword: func(hash, pw []byte) error {
		calls++
		return bcrypt.ErrMismatchedHashAndPassword
	}}

	rec := loginWith(t, h, "ghost@example.com", "any-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown email: got %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("comparator calls = %d, want 1 (unknown email must cost a comparison)", calls)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 body not JSON: %v", err)
	}
	if body.Error.Code != "invalid_credentials" {
		t.Errorf("error code = %q, want the same generic invalid_credentials", body.Error.Code)
	}
}

func TestLogin_UnknownAndWrongPasswordAreIndistinguishable(t *testing.T) {
	h, _ := setupUserTest(t)
	mustRegister(t, h, "real@example.com", "correct-password")

	unknown := postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "ghost@example.com", Password: "x-password"}, nil)
	wrongPw := postJSON(t, h, "/api/auth/login",
		loginRequest{Email: "real@example.com", Password: "x-password"}, nil)

	if unknown.Code != http.StatusUnauthorized || wrongPw.Code != http.StatusUnauthorized {
		t.Fatalf("codes = %d / %d, want 401 / 401", unknown.Code, wrongPw.Code)
	}

	type envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	var a, b envelope
	if err := json.Unmarshal(unknown.Body.Bytes(), &a); err != nil {
		t.Fatalf("unknown body: %v", err)
	}
	if err := json.Unmarshal(wrongPw.Body.Bytes(), &b); err != nil {
		t.Fatalf("wrong-pw body: %v", err)
	}
	if a.Error != b.Error {
		t.Errorf("error envelopes differ:\nunknown: %+v\nwrong-pw: %+v", a.Error, b.Error)
	}
}

// The dummy comparison must run inside the bcrypt semaphore (DoS backstop)
// and release it afterwards — no slot leaks on the unknown-email path.
func TestLogin_UnknownEmailUsesAndReleasesBcryptSlot(t *testing.T) {
	_, s := setupUserTest(t)
	guard := authlimit.NewWithClock(authlimit.Config{BcryptConcurrency: 1}, time.Now)

	h := &handler{store: s, guard: guard, comparePassword: bcrypt.CompareHashAndPassword}

	// Saturated slot -> unknown-email login must 429, proving the dummy
	// path respects the cap.
	if !guard.TryAcquireBcrypt() {
		t.Fatal("could not occupy bcrypt slot")
	}
	rec := loginWith(t, h, "ghost@example.com", "pw-anything")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("saturated unknown-email login: got %d, want 429", rec.Code)
	}
	guard.ReleaseBcrypt()

	// Two sequential unknown-email logins with capacity 1: if the first
	// leaked its slot, the second would 429.
	for i := 0; i < 2; i++ {
		rec := loginWith(t, h, "ghost@example.com", "pw-anything")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("login %d: got %d, want 401 (slot leak?)", i+1, rec.Code)
		}
	}
}

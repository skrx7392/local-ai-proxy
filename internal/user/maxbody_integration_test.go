package user

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/middleware"
)

// TestRegister_OversizedBodyRejected exercises the composition main.go wires:
// MaxBody middleware in front of the user handler.
func TestRegister_OversizedBodyRejected(t *testing.T) {
	inner, _ := setupUserTest(t)
	h := middleware.MaxBody(1024)(inner)

	big := bytes.Repeat([]byte("a"), 4096)
	payload, _ := json.Marshal(registerRequest{Email: "big@example.com", Password: string(big), Name: "Big"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("413 body not JSON: %v", err)
	}
	if body.Error.Code != "request_too_large" {
		t.Errorf("error code = %q, want request_too_large", body.Error.Code)
	}

	// A normal-sized register still works through the same stack.
	payload, _ = json.Marshal(registerRequest{Email: "ok@example.com", Password: "fine-password", Name: "OK"})
	req = httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(payload))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("normal register: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

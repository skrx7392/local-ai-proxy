package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/apierror"
)

// decodeHandler mimics a JSON endpoint behind the middleware.
func decodeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var dst struct {
			Name string `json:"name"`
		}
		if !apierror.DecodeJSON(w, r, &dst) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestMaxBody_UnderLimitPasses(t *testing.T) {
	h := MaxBody(1024)(decodeHandler())

	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader([]byte(`{"name":"small"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMaxBody_OverLimitReturns413Envelope(t *testing.T) {
	h := MaxBody(32)(decodeHandler())

	payload, _ := json.Marshal(map[string]string{"name": string(bytes.Repeat([]byte("a"), 64))})
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("413 body not JSON: %v", err)
	}
	if body.Error.Code != "request_too_large" {
		t.Errorf("error code = %q, want request_too_large", body.Error.Code)
	}
}

func TestMaxBody_ExactLimitPasses(t *testing.T) {
	payload := []byte(`{"name":"edge"}`)
	h := MaxBody(int64(len(payload)))(decodeHandler())

	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for body exactly at limit; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMaxBody_NilBodyDoesNotPanic(t *testing.T) {
	h := MaxBody(32)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for GET without body", rec.Code)
	}
}

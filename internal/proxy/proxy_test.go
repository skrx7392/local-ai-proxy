package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "invalid_request", "invalid_request_error", "Bad request")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected 'error' object in response")
	}
	if errObj["message"] != "Bad request" {
		t.Errorf("expected message 'Bad request', got %v", errObj["message"])
	}
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("expected type 'invalid_request_error', got %v", errObj["type"])
	}
	if errObj["code"] != "invalid_request" {
		t.Errorf("expected code 'invalid_request', got %v", errObj["code"])
	}
}

func TestWriteError_NotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusNotFound, "not_found", "invalid_request_error", "Not found")

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.Code)
	}
}

func TestWriteError_InternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}
}

func TestStripTrailingSlash(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/v1/models/", "/v1/models"},
		{"/v1/models", "/v1/models"},
		{"/", "/"},
		{"/a/", "/a"},
		{"", ""},
		{"/hello/world/", "/hello/world"},
	}

	for _, tc := range tests {
		got := StripTrailingSlash(tc.input)
		if got != tc.expected {
			t.Errorf("StripTrailingSlash(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestServeHTTP_NotFoundForUnknownPath(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler("http://localhost:11434", usageCh, 52428800)

	req := httptest.NewRequest(http.MethodGet, "/unknown/path", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown path, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object")
	}
	if errObj["code"] != "not_found" {
		t.Errorf("expected code 'not_found', got %v", errObj["code"])
	}
}

func TestServeHTTP_NotFoundForWrongMethod(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler("http://localhost:11434", usageCh, 52428800)

	// GET on chat/completions should be 404 (only POST is handled)
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for GET on /v1/chat/completions, got %d", rec.Code)
	}
}

func TestServeHTTP_NotFoundForPostModels(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler("http://localhost:11434", usageCh, 52428800)

	// POST on /v1/models should be 404 (only GET is handled)
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for POST on /v1/models, got %d", rec.Code)
	}
}

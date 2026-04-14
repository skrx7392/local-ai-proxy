package apierror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/logging"
)

func TestWriteError_NilRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, nil, http.StatusBadRequest, "bad", "invalid_request_error", "Bad")

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["error"].(map[string]any); !ok {
		t.Error("expected error object")
	}
	if _, ok := resp["request_id"]; ok {
		t.Error("request_id should be absent when request is nil")
	}
}

func TestWriteError_WithRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := logging.WithRequestID(context.Background(), "req_xyz")
	req = req.WithContext(ctx)
	WriteError(rec, req, http.StatusNotFound, "missing", "invalid_request_error", "Gone")

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["request_id"] != "req_xyz" {
		t.Errorf("expected request_id propagated, got %v", resp["request_id"])
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj["code"] != "missing" {
		t.Errorf("expected code=missing, got %v", errObj["code"])
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Error("expected Content-Type application/json")
	}
}

func TestWriteError_WithoutRequestIDInContext(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	WriteError(rec, req, http.StatusUnauthorized, "no_id", "invalid_api_key", "No id")

	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if _, ok := resp["request_id"]; ok {
		t.Error("request_id should be absent when context has none")
	}
}

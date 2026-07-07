package apierror

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testPayload struct {
	Name string `json:"name"`
}

func TestDecodeJSON_ValidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ok"}`))
	rec := httptest.NewRecorder()

	var dst testPayload
	if !DecodeJSON(rec, req, &dst) {
		t.Fatalf("DecodeJSON = false for valid body, response: %s", rec.Body.String())
	}
	if dst.Name != "ok" {
		t.Errorf("decoded Name = %q, want ok", dst.Name)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("no response should be written on success, got %s", rec.Body.String())
	}
}

func TestDecodeJSON_InvalidJSONWrites400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()

	var dst testPayload
	if DecodeJSON(rec, req, &dst) {
		t.Fatal("DecodeJSON = true for invalid body")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if body.Error.Code != "invalid_json" {
		t.Errorf("error code = %q, want invalid_json", body.Error.Code)
	}
}

func TestDecodeJSON_OversizedBodyWrites413(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 128)
	payload, _ := json.Marshal(testPayload{Name: string(big)})

	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	// Same shape the MaxBody middleware produces.
	req.Body = http.MaxBytesReader(rec, req.Body, 16)

	var dst testPayload
	if DecodeJSON(rec, req, &dst) {
		t.Fatal("DecodeJSON = true for oversized body")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body not JSON: %v", err)
	}
	if body.Error.Code != "request_too_large" {
		t.Errorf("error code = %q, want request_too_large", body.Error.Code)
	}
}

func TestDecodeJSON_EmptyBodyWrites400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
	rec := httptest.NewRecorder()

	var dst testPayload
	if DecodeJSON(rec, req, &dst) {
		t.Fatal("DecodeJSON = true for empty body")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type mockPinger struct {
	err error
}

func (m *mockPinger) Ping(ctx context.Context) error {
	return m.err
}

func TestLiveHandler_AlwaysOK(t *testing.T) {
	c := NewChecker(nil, "", nil, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/live", nil)

	c.LiveHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", body["status"])
	}
}

func TestReadyHandler_AllHealthy(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ollama.Close()

	c := NewChecker(
		&mockPinger{},
		ollama.URL,
		func() int { return 5 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("expected status 'ready', got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected 'checks' object")
	}
	dbCheck := checks["database"].(map[string]any)
	if dbCheck["status"] != "ok" {
		t.Errorf("expected database 'ok', got %v", dbCheck["status"])
	}
	ollamaCheck := checks["ollama"].(map[string]any)
	if ollamaCheck["status"] != "ok" {
		t.Errorf("expected ollama 'ok', got %v", ollamaCheck["status"])
	}
	usageCheck := checks["usage_writer"].(map[string]any)
	if usageCheck["status"] != "ok" {
		t.Errorf("expected usage_writer 'ok', got %v", usageCheck["status"])
	}
}

func TestReadyHandler_DBDown(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ollama.Close()

	c := NewChecker(
		&mockPinger{err: errors.New("connection refused")},
		ollama.URL,
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" {
		t.Errorf("expected 'not_ready', got %v", body["status"])
	}
	checks := body["checks"].(map[string]any)
	dbCheck := checks["database"].(map[string]any)
	if dbCheck["status"] != "error" {
		t.Errorf("expected database 'error', got %v", dbCheck["status"])
	}
}

func TestReadyHandler_OllamaDown(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		"http://127.0.0.1:1", // connection refused
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	checks := body["checks"].(map[string]any)
	ollamaCheck := checks["ollama"].(map[string]any)
	if ollamaCheck["status"] != "error" {
		t.Errorf("expected ollama 'error', got %v", ollamaCheck["status"])
	}
}

func TestReadyHandler_UsageChannelFull(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ollama.Close()

	c := NewChecker(
		&mockPinger{},
		ollama.URL,
		func() int { return 1000 }, // full
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	checks := body["checks"].(map[string]any)
	usageCheck := checks["usage_writer"].(map[string]any)
	if usageCheck["status"] != "error" {
		t.Errorf("expected usage_writer 'error', got %v", usageCheck["status"])
	}
}

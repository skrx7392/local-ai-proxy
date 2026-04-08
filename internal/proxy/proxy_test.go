package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
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
	h := NewHandler("http://localhost:11434", usageCh, 52428800, nil)

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
	h := NewHandler("http://localhost:11434", usageCh, 52428800, nil)

	// GET on chat/completions should be 404 (only POST is handled)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for GET on /v1/chat/completions, got %d", rec.Code)
	}
}

func TestServeHTTP_NotFoundForPostModels(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler("http://localhost:11434", usageCh, 52428800, nil)

	// POST on /v1/models should be 404 (only GET is handled)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for POST on /v1/models, got %d", rec.Code)
	}
}

// --- Mock Ollama server helpers ---

func mockOllamaChatNonStreaming(statusCode int, respBody map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusCode)
			json.NewEncoder(w).Encode(respBody)
			return
		}
		http.NotFound(w, r)
	}))
}

func mockOllamaChatStreaming() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)

			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming not supported", http.StatusInternalServerError)
				return
			}

			// Send a few SSE chunks
			chunks := []string{
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"}}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{"content":" world"}}]}`,
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			}

			for _, chunk := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				flusher.Flush()
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		http.NotFound(w, r)
	}))
}

func testAPIKey() *store.APIKey {
	return &store.APIKey{
		ID:        1,
		Name:      "test-key",
		RateLimit: 60,
	}
}

func addKeyToRequest(req *http.Request, key *store.APIKey) *http.Request {
	ctx := auth.WithKey(req.Context(), key)
	return req.WithContext(ctx)
}

// --- Models tests ---

func TestHandleModels_NilDB(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler("http://localhost:11434", usageCh, 52428800, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with nil db, got %d", rec.Code)
	}
}

// --- Non-streaming chat completions tests ---

func TestHandleChatCompletions_NonStreaming(t *testing.T) {
	ollamaResp := map[string]any{
		"id":      "chatcmpl-123",
		"object":  "chat.completion",
		"model":   "llama3:latest",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hello!"}}},
		"usage": map[string]any{
			"prompt_tokens":     10,
			"completion_tokens": 5,
			"total_tokens":      15,
		},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if body["id"] != "chatcmpl-123" {
		t.Errorf("expected id 'chatcmpl-123', got %v", body["id"])
	}

	// Check that usage was logged
	select {
	case entry := <-usageCh:
		if entry.APIKeyID != key.ID {
			t.Errorf("expected key ID %d, got %d", key.ID, entry.APIKeyID)
		}
		if entry.Status != "completed" {
			t.Errorf("expected status 'completed', got %q", entry.Status)
		}
		if entry.TotalTokens != 15 {
			t.Errorf("expected 15 total tokens, got %d", entry.TotalTokens)
		}
		if entry.PromptTokens != 10 {
			t.Errorf("expected 10 prompt tokens, got %d", entry.PromptTokens)
		}
		if entry.CompletionTokens != 5 {
			t.Errorf("expected 5 completion tokens, got %d", entry.CompletionTokens)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

func TestHandleChatCompletions_NonStreaming_NoKey(t *testing.T) {
	ollamaResp := map[string]any{
		"id":      "chatcmpl-456",
		"object":  "chat.completion",
		"model":   "llama3:latest",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hi!"}}},
		"usage": map[string]any{
			"prompt_tokens":     5,
			"completion_tokens": 2,
			"total_tokens":      7,
		},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hey"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// No usage should be logged when there's no key
	select {
	case entry := <-usageCh:
		t.Errorf("did not expect usage entry without key, got %+v", entry)
	case <-time.After(200 * time.Millisecond):
		// Expected: no usage logged
	}
}

// --- Streaming chat completions tests ---

func TestHandleChatCompletions_Streaming(t *testing.T) {
	upstream := mockOllamaChatStreaming()
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	streamTrue := true
	reqMeta := requestMeta{
		Model:  "llama3:latest",
		Stream: &streamTrue,
	}
	reqBodyBytes, _ := json.Marshal(map[string]any{
		"model":    reqMeta.Model,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(reqBodyBytes))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Verify the response contains SSE data
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Error("expected SSE data lines in response")
	}
	if !strings.Contains(body, "Hello") {
		t.Error("expected 'Hello' content in streamed response")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("expected [DONE] terminator in streamed response")
	}

	// Check usage was logged with token counts from the usage chunk
	select {
	case entry := <-usageCh:
		if entry.APIKeyID != key.ID {
			t.Errorf("expected key ID %d, got %d", key.ID, entry.APIKeyID)
		}
		if entry.Status != "completed" {
			t.Errorf("expected status 'completed', got %q", entry.Status)
		}
		if entry.TotalTokens != 15 {
			t.Errorf("expected 15 total tokens, got %d", entry.TotalTokens)
		}
		if entry.Model != "llama3:latest" {
			t.Errorf("expected model 'llama3:latest', got %q", entry.Model)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

func TestHandleChatCompletions_Streaming_NoKey(t *testing.T) {
	upstream := mockOllamaChatStreaming()
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBodyBytes, _ := json.Marshal(map[string]any{
		"model":    "llama3:latest",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(reqBodyBytes))
	req.Header.Set("Content-Type", "application/json")
	// No key in context

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// No usage should be logged
	select {
	case entry := <-usageCh:
		t.Errorf("did not expect usage entry without key, got %+v", entry)
	case <-time.After(200 * time.Millisecond):
		// Expected
	}
}

// --- Request body too large ---

func TestHandleChatCompletions_BodyTooLarge(t *testing.T) {
	// Create a mock upstream (won't actually be reached)
	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	// Set a very small max body size
	h := NewHandler(upstream.URL, usageCh, 10, nil)

	// Send a body larger than 10 bytes
	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"This is a long message that exceeds the body limit"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object")
	}
	if errObj["code"] != "request_too_large" {
		t.Errorf("expected code 'request_too_large', got %v", errObj["code"])
	}
}

// --- Upstream error (500) ---

func TestHandleChatCompletions_UpstreamError500_NonStreaming(t *testing.T) {
	errorResp := map[string]any{
		"error": map[string]any{
			"message": "Internal server error",
			"type":    "server_error",
		},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusInternalServerError, errorResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The reverse proxy passes through the upstream status code
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}

	// Usage should still be logged
	select {
	case entry := <-usageCh:
		if entry.APIKeyID != key.ID {
			t.Errorf("expected key ID %d, got %d", key.ID, entry.APIKeyID)
		}
		if entry.Status != "error" {
			// Non-streaming handler correctly marks non-200 upstream as error
			t.Errorf("expected status 'error', got %q", entry.Status)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

func TestHandleChatCompletions_UpstreamError500_Streaming(t *testing.T) {
	// Mock server that returns 500 for streaming requests
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "internal error",
		})
	}))
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBodyBytes, _ := json.Marshal(map[string]any{
		"model":    "llama3:latest",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(reqBodyBytes))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}

	// Usage should be logged with error status
	select {
	case entry := <-usageCh:
		if entry.Status != "error" {
			t.Errorf("expected status 'error', got %q", entry.Status)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

// --- Upstream connection refused ---

func TestHandleChatCompletions_UpstreamDown_NonStreaming(t *testing.T) {
	// Use a URL that won't connect (closed server)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close() // close immediately so connections fail

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstreamURL, usageCh, 52428800, nil)

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}

	// Usage should be logged with error status
	select {
	case entry := <-usageCh:
		if entry.Status != "error" {
			t.Errorf("expected status 'error', got %q", entry.Status)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

func TestHandleChatCompletions_UpstreamDown_Streaming(t *testing.T) {
	// Use a URL that won't connect
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstreamURL, usageCh, 52428800, nil)

	reqBodyBytes, _ := json.Marshal(map[string]any{
		"model":    "llama3:latest",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(reqBodyBytes))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}

	// Usage should be logged with error status
	select {
	case entry := <-usageCh:
		if entry.Status != "error" {
			t.Errorf("expected status 'error', got %q", entry.Status)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

// --- logUsage tests ---

func TestLogUsage_NilKey(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := &handler{usageCh: usageCh}

	// Should not panic and should not send to channel
	h.logUsage(nil, usageData{Model: "test"}, time.Second, "completed")

	select {
	case entry := <-usageCh:
		t.Errorf("did not expect usage entry for nil key, got %+v", entry)
	case <-time.After(100 * time.Millisecond):
		// Expected: nothing sent
	}
}

func TestLogUsage_ChannelFull(t *testing.T) {
	// Create a full channel (capacity 0, buffered 1 and fill it)
	usageCh := make(chan store.UsageEntry, 1)
	usageCh <- store.UsageEntry{} // fill it

	h := &handler{usageCh: usageCh}
	key := testAPIKey()

	// Should not block — entry is dropped silently
	h.logUsage(key, usageData{Model: "test"}, time.Second, "completed")

	// Drain the original entry
	<-usageCh

	// The dropped entry should not be in the channel
	select {
	case <-usageCh:
		t.Error("expected channel to be empty after drain")
	default:
		// Good — channel is empty
	}
}

func TestLogUsage_Success(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := &handler{usageCh: usageCh}

	key := testAPIKey()
	ud := usageData{Model: "llama3:latest"}
	ud.Usage.PromptTokens = 10
	ud.Usage.CompletionTokens = 5
	ud.Usage.TotalTokens = 15

	h.logUsage(key, ud, 500*time.Millisecond, "completed")

	select {
	case entry := <-usageCh:
		if entry.APIKeyID != key.ID {
			t.Errorf("expected key ID %d, got %d", key.ID, entry.APIKeyID)
		}
		if entry.Model != "llama3:latest" {
			t.Errorf("expected model 'llama3:latest', got %q", entry.Model)
		}
		if entry.PromptTokens != 10 {
			t.Errorf("expected 10 prompt tokens, got %d", entry.PromptTokens)
		}
		if entry.CompletionTokens != 5 {
			t.Errorf("expected 5 completion tokens, got %d", entry.CompletionTokens)
		}
		if entry.TotalTokens != 15 {
			t.Errorf("expected 15 total tokens, got %d", entry.TotalTokens)
		}
		if entry.DurationMs != 500 {
			t.Errorf("expected 500ms duration, got %d", entry.DurationMs)
		}
		if entry.Status != "completed" {
			t.Errorf("expected status 'completed', got %q", entry.Status)
		}
	case <-time.After(time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

// responseRecorder was removed — non-streaming now uses direct http.Client

// --- Non-streaming with bad JSON body (best-effort parse) ---

func TestHandleChatCompletions_InvalidJSON(t *testing.T) {
	ollamaResp := map[string]any{
		"id":     "chatcmpl-bad",
		"object": "chat.completion",
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	// Send invalid JSON that can still be read
	reqBody := `not valid json at all`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The handler does best-effort parse, so it proceeds with non-streaming
	// (stream field is nil -> not streaming)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (upstream handles the bad payload), got %d", rec.Code)
	}
}

// --- Empty body ---

func TestHandleChatCompletions_EmptyBody(t *testing.T) {
	ollamaResp := map[string]any{
		"id":     "chatcmpl-empty",
		"object": "chat.completion",
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Should handle empty body gracefully (non-streaming path)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

// --- Test the read body error path (not "too large") ---

func TestHandleChatCompletions_ReadBodyError(t *testing.T) {
	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	// Use a reader that returns an error
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", &errorReader{})
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error object")
	}
	if errObj["code"] != "invalid_request" {
		t.Errorf("expected code 'invalid_request', got %v", errObj["code"])
	}
}

// errorReader is an io.Reader that always returns an error.
type errorReader struct{}

func (r *errorReader) Read(p []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

// --- Test that stream=false explicitly goes non-streaming ---

func TestHandleChatCompletions_StreamFalse(t *testing.T) {
	ollamaResp := map[string]any{
		"id":      "chatcmpl-sf",
		"object":  "chat.completion",
		"model":   "llama3:latest",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "No stream"}}},
		"usage": map[string]any{
			"prompt_tokens":     3,
			"completion_tokens": 2,
			"total_tokens":      5,
		},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBody := `{"model":"llama3:latest","stream":false,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Verify usage was logged
	select {
	case entry := <-usageCh:
		if entry.TotalTokens != 5 {
			t.Errorf("expected 5 total tokens, got %d", entry.TotalTokens)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

// --- Test non-streaming with response that has no usage data ---

func TestHandleChatCompletions_NonStreaming_NoUsageInResponse(t *testing.T) {
	// Response with no usage field
	ollamaResp := map[string]any{
		"id":      "chatcmpl-no-usage",
		"object":  "chat.completion",
		"model":   "llama3:latest",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hey"}}},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(upstream.URL, usageCh, 52428800, nil)

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Usage should still be logged (with 0 tokens) since key is present
	select {
	case entry := <-usageCh:
		if entry.TotalTokens != 0 {
			t.Errorf("expected 0 total tokens, got %d", entry.TotalTokens)
		}
		if entry.Model != "llama3:latest" {
			t.Errorf("expected model 'llama3:latest', got %q", entry.Model)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

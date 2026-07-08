package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/logging"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", rawURL, err)
	}
	return u
}

// testNode describes one backend node for testRegistry.
type testNode struct {
	ID        int64
	Name      string
	URL       string
	Auth      string
	Timeout   time.Duration
	Models    []string
	Unhealthy bool
}

// testRegistry builds a real *registry.Registry (it satisfies the handler's
// Registry interface) with the given nodes and their health/model state.
func testRegistry(t *testing.T, nodes ...testNode) *registry.Registry {
	t.Helper()
	reg := registry.New()
	regNodes := make([]registry.Node, 0, len(nodes))
	for _, n := range nodes {
		regNodes = append(regNodes, registry.Node{
			ID:         n.ID,
			Name:       n.Name,
			BaseURL:    mustParseURL(t, n.URL),
			AuthHeader: n.Auth,
			Timeout:    n.Timeout,
		})
	}
	reg.SetNodes(regNodes)
	for _, n := range nodes {
		h := registry.HealthHealthy
		if n.Unhealthy {
			h = registry.HealthUnhealthy
		}
		reg.SetNodeState(n.ID, h, n.Models)
	}
	return reg
}

// singleNodeRegistry is the one-healthy-node shorthand used by most tests:
// node id 1, named "n1", serving the given models.
func singleNodeRegistry(t *testing.T, upstreamURL string, models ...string) *registry.Registry {
	t.Helper()
	return testRegistry(t, testNode{ID: 1, Name: "n1", URL: upstreamURL, Models: models})
}

func setupTestDB(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wipe := func() {
		pool := s.Pool()
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
	return s
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	WriteError(rec, req, http.StatusBadRequest, "invalid_request", "invalid_request_error", "Bad request")

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

	// No request_id in context → no request_id in response
	if _, exists := body["request_id"]; exists {
		t.Error("expected no request_id when context has none")
	}
}

func TestWriteError_WithRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	ctx := logging.WithRequestID(req.Context(), "req_test123")
	req = req.WithContext(ctx)

	WriteError(rec, req, http.StatusBadRequest, "invalid_request", "invalid_request_error", "Bad request")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	if body["request_id"] != "req_test123" {
		t.Errorf("expected request_id 'req_test123', got %v", body["request_id"])
	}

	// Verify error object is still present
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatal("expected 'error' object in response")
	}
	if errObj["code"] != "invalid_request" {
		t.Errorf("expected code 'invalid_request', got %v", errObj["code"])
	}
}

func TestWriteError_NotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	WriteError(rec, req, http.StatusNotFound, "not_found", "invalid_request_error", "Not found")

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.Code)
	}
}

func TestWriteError_InternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	WriteError(rec, req, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")

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
	h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

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
	h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

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
	h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

	// POST on /v1/models should be 404 (only GET is handled)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for POST on /v1/models, got %d", rec.Code)
	}
}

// --- Mock upstream node helpers ---

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

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantCode, wantType string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error response: %v (body=%s)", err, rec.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %s", rec.Body.String())
	}
	if errObj["code"] != wantCode {
		t.Errorf("expected code %q, got %v", wantCode, errObj["code"])
	}
	if errObj["type"] != wantType {
		t.Errorf("expected type %q, got %v", wantType, errObj["type"])
	}
}

// --- Request validation (step 1 of the routing flow) ---

func TestHandleChatCompletions_MalformedJSON_400(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
	}))
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(`not valid json at all`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON, got %d", rec.Code)
	}
	assertErrorCode(t, rec, "invalid_request", "invalid_request_error")
	if hits := atomic.LoadInt32(&upstreamHits); hits != 0 {
		t.Errorf("upstream must never be contacted for malformed JSON, got %d hits", hits)
	}
}

func TestHandleChatCompletions_EmptyBody_400(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty body, got %d", rec.Code)
	}
	assertErrorCode(t, rec, "invalid_request", "invalid_request_error")
}

func TestHandleChatCompletions_MissingModel_400(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"messages":[{"role":"user","content":"Hi"}]}`,
		"empty":  `{"model":"","messages":[{"role":"user","content":"Hi"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			usageCh := make(chan store.UsageEntry, 10)
			h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for missing model, got %d", rec.Code)
			}
			assertErrorCode(t, rec, "invalid_request", "invalid_request_error")
		})
	}
}

// --- Model resolution (step 3) ---

func TestHandleChatCompletions_ModelUnavailable_503(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

	reqBody := `{"model":"ghost-model","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = addKeyToRequest(req, testAPIKey())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for unavailable model, got %d", rec.Code)
	}
	assertErrorCode(t, rec, "model_unavailable", "server_error")

	// A request that never resolved a node must not log usage.
	select {
	case entry := <-usageCh:
		t.Errorf("did not expect usage entry for unresolved request, got %+v", entry)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleChatCompletions_UnhealthyNode_503(t *testing.T) {
	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{"id": "x"})
	defer upstream.Close()

	reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: upstream.URL, Models: []string{"llama3:latest"}, Unhealthy: true})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the only node is unhealthy, got %d", rec.Code)
	}
	assertErrorCode(t, rec, "model_unavailable", "server_error")
}

// --- Full request flow: non-streaming ---

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
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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

	// Check that usage was logged, attributed to the resolved node
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
		if entry.NodeID == nil || *entry.NodeID != 1 {
			t.Errorf("expected NodeID=1, got %v", entry.NodeID)
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
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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

// --- Full request flow: streaming ---

func TestHandleChatCompletions_Streaming(t *testing.T) {
	upstream := mockOllamaChatStreaming()
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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

	// Check usage was logged with token counts from the usage chunk and the
	// resolved node's ID.
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
		if entry.NodeID == nil || *entry.NodeID != 1 {
			t.Errorf("expected NodeID=1, got %v", entry.NodeID)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

func TestHandleChatCompletions_Streaming_NoKey(t *testing.T) {
	upstream := mockOllamaChatStreaming()
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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

// --- Round-robin across healthy nodes ---

func TestHandleChatCompletions_RoundRobinAcrossNodes(t *testing.T) {
	resp := map[string]any{"id": "chatcmpl-rr", "object": "chat.completion"}
	var hits1, hits2 int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits1, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits2, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer up2.Close()

	reg := testRegistry(t,
		testNode{ID: 1, Name: "n1", URL: up1.URL, Models: []string{"m"}},
		testNode{ID: 2, Name: "n2", URL: up2.URL, Models: []string{"m"}},
	)
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

	for i := 0; i < 4; i++ {
		reqBody := `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rec.Code)
		}
	}

	if h1, h2 := atomic.LoadInt32(&hits1), atomic.LoadInt32(&hits2); h1 != 2 || h2 != 2 {
		t.Errorf("expected round-robin 2/2 across nodes, got n1=%d n2=%d", h1, h2)
	}
}

// --- Upstream URL construction ---

func TestHandleChatCompletions_BaseURLPrefixPreserved(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-prefix"})
	}))
	defer upstream.Close()

	reg := singleNodeRegistry(t, upstream.URL+"/openai", "m")
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

	reqBody := `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/openai/v1/chat/completions" {
		t.Errorf("expected path-joined upstream URL '/openai/v1/chat/completions', got %q", gotPath)
	}
}

// --- Per-node auth header ---

func TestHandleChatCompletions_PerNodeAuthHeader(t *testing.T) {
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-auth"})
	}))
	defer upstream.Close()

	t.Run("node auth header set upstream", func(t *testing.T) {
		reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: upstream.URL, Auth: "Bearer node-secret", Models: []string{"m"}})
		usageCh := make(chan store.UsageEntry, 10)
		h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-api-key") // must not leak upstream
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if got := gotAuth.Load(); got != "Bearer node-secret" {
			t.Errorf("expected upstream Authorization 'Bearer node-secret', got %v", got)
		}
	})

	t.Run("no node auth means no Authorization upstream", func(t *testing.T) {
		reg := singleNodeRegistry(t, upstream.URL, "m")
		usageCh := make(chan store.UsageEntry, 10)
		h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer client-api-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if got := gotAuth.Load(); got != "" {
			t.Errorf("expected no upstream Authorization header, got %v", got)
		}
	})
}

// --- Redirect refusal (credential exfiltration guard) ---

func TestHandleChatCompletions_RedirectRefused(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-evil"})
	}))
	defer target.Close()

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer redirecting.Close()

	for name, body := range map[string]string{
		"non-streaming": `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`,
		"streaming":     `{"model":"m","stream":true,"messages":[{"role":"user","content":"Hi"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			reg := singleNodeRegistry(t, redirecting.URL, "m")
			usageCh := make(chan store.UsageEntry, 10)
			h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Errorf("expected 502 for upstream redirect, got %d", rec.Code)
			}
			if hits := atomic.LoadInt32(&targetHits); hits != 0 {
				t.Errorf("redirect target must never be contacted, got %d hits", hits)
			}
		})
	}
}

// --- Response body caps ---

func TestHandleChatCompletions_ResponseBodyCap_NonStreaming(t *testing.T) {
	big := strings.Repeat("x", 1000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(big))
	}))
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	// maxBody caps both the request body and the upstream response read.
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "m"), usageCh, 256, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if got := rec.Body.Len(); got != 256 {
		t.Errorf("expected response truncated to 256 bytes, got %d", got)
	}
}

func TestHandleChatCompletions_UpstreamErrorBodyCap(t *testing.T) {
	big := strings.Repeat("e", 1000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(big))
	}))
	defer upstream.Close()

	for name, body := range map[string]string{
		"non-streaming": `{"model":"m","messages":[]}`,
		"streaming":     `{"model":"m","stream":true,"messages":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			usageCh := make(chan store.UsageEntry, 10)
			h := NewHandler(singleNodeRegistry(t, upstream.URL, "m"), usageCh, 256, nil, nil, Options{})

			req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("expected 500 passthrough, got %d", rec.Code)
			}
			if got := rec.Body.Len(); got != 256 {
				t.Errorf("expected upstream error body capped at 256 bytes, got %d", got)
			}
		})
	}
}

// --- Per-node timeout ---

func TestUpstreamTimeout(t *testing.T) {
	if got := upstreamTimeout(registry.Node{}); got != 5*time.Minute {
		t.Errorf("expected 5m default when node Timeout==0, got %v", got)
	}
	if got := upstreamTimeout(registry.Node{Timeout: 42 * time.Second}); got != 42*time.Second {
		t.Errorf("expected node override 42s, got %v", got)
	}
}

func TestHandleChatCompletions_PerNodeTimeout_NonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: upstream.URL, Timeout: 100 * time.Millisecond, Models: []string{"m"}})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

	reqBody := `{"model":"m","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = addKeyToRequest(req, testAPIKey())

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("handler took %v; per-node 100ms timeout not applied", elapsed)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 on per-node timeout, got %d", rec.Code)
	}

	select {
	case entry := <-usageCh:
		if entry.Status != "error" {
			t.Errorf("expected status 'error' after upstream timeout, got %q", entry.Status)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

func TestHandleChatCompletions_PerNodeTimeout_StreamingMidStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer upstream.Close()

	reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: upstream.URL, Timeout: 150 * time.Millisecond, Models: []string{"m"}})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, nil, nil, Options{})

	reqBody := `{"model":"m","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = addKeyToRequest(req, testAPIKey())

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("streaming handler took %v; ctx cancellation not respected mid-stream", elapsed)
	}
	if !strings.Contains(rec.Body.String(), "first") {
		t.Error("expected first chunk to have been streamed before timeout")
	}

	select {
	case entry := <-usageCh:
		if entry.Status != "error" {
			t.Errorf("expected status 'error' after mid-stream timeout, got %q", entry.Status)
		}
		if entry.NodeID == nil || *entry.NodeID != 1 {
			t.Errorf("expected NodeID=1, got %v", entry.NodeID)
		}
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

// --- Token metrics carry the node label ---

func TestHandleChatCompletions_TokensMetricHasNodeLabel(t *testing.T) {
	ollamaResp := map[string]any{
		"id": "chatcmpl-metrics", "object": "chat.completion", "model": "llama3:latest",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hello!"}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	m := metrics.New(func() int { return 0 })
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, m, Options{})

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := testutil.ToFloat64(m.TokensTotal.WithLabelValues("llama3:latest", "prompt", "n1")); got != 10 {
		t.Errorf("expected prompt tokens 10 for node n1, got %v", got)
	}
	if got := testutil.ToFloat64(m.TokensTotal.WithLabelValues("llama3:latest", "completion", "n1")); got != 5 {
		t.Errorf("expected completion tokens 5 for node n1, got %v", got)
	}
}

// --- Request body too large ---

func TestHandleChatCompletions_BodyTooLarge(t *testing.T) {
	// Create a mock upstream (won't actually be reached)
	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	// Set a very small max body size
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 10, nil, nil, Options{})

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
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	reqBody := `{"model":"llama3:latest","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	key := testAPIKey()
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The proxy passes through the upstream status code
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
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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
	h := NewHandler(singleNodeRegistry(t, upstreamURL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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
	h := NewHandler(singleNodeRegistry(t, upstreamURL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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
	h.logUsage(nil, usageData{Model: "test"}, time.Second, "completed", 0, nil)

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
	h.logUsage(key, usageData{Model: "test"}, time.Second, "completed", 0, nil)

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

	nodeID := int64(7)
	h.logUsage(key, ud, 500*time.Millisecond, "completed", 0.12, &nodeID)

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
		if diff := entry.CreditsCharged - 0.12; diff < -0.0001 || diff > 0.0001 {
			t.Errorf("expected credits_charged=0.12, got %f", entry.CreditsCharged)
		}
		if entry.NodeID == nil || *entry.NodeID != 7 {
			t.Errorf("expected NodeID=7, got %v", entry.NodeID)
		}
	case <-time.After(time.Second):
		t.Error("timed out waiting for usage entry")
	}
}

// --- Test the read body error path (not "too large") ---

func TestHandleChatCompletions_ReadBodyError(t *testing.T) {
	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

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

// --- /v1/models ---

func TestHandleModels_NilDB(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(registry.New(), usageCh, 52428800, nil, nil, Options{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with nil db, got %d", rec.Code)
	}
}

// listModels performs GET /v1/models against h and returns id -> owned_by.
func listModels(t *testing.T, h http.Handler) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse models response: %v", err)
	}
	if body.Object != "list" {
		t.Errorf("expected object 'list', got %q", body.Object)
	}
	out := make(map[string]string, len(body.Data))
	for _, m := range body.Data {
		if m.Object != "model" {
			t.Errorf("model %q: expected object 'model', got %q", m.ID, m.Object)
		}
		out[m.ID] = m.OwnedBy
	}
	return out
}

func TestModels_IntersectionWithHealthyNodes(t *testing.T) {
	db := setupTestDB(t)
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)
	_ = db.UpsertPricing("qwen2.5-coder:7b", 0.002, 0.002, 500)

	// Only llama3.1:8b is served by a healthy node.
	reg := testRegistry(t, testNode{ID: 1, Name: "m5-max", URL: "http://n1:11434", Models: []string{"llama3.1:8b"}})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, db, nil, Options{})

	models := listModels(t, h)
	if len(models) != 1 {
		t.Fatalf("expected 1 model (intersection), got %d: %v", len(models), models)
	}
	if models["llama3.1:8b"] != "m5-max" {
		t.Errorf("expected owned_by 'm5-max' for single-node model, got %q", models["llama3.1:8b"])
	}
}

func TestModels_OwnedByMultiple(t *testing.T) {
	db := setupTestDB(t)
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	reg := testRegistry(t,
		testNode{ID: 1, Name: "n1", URL: "http://n1:11434", Models: []string{"llama3.1:8b"}},
		testNode{ID: 2, Name: "n2", URL: "http://n2:11434", Models: []string{"llama3.1:8b"}},
	)
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, db, nil, Options{})

	models := listModels(t, h)
	if models["llama3.1:8b"] != "multiple" {
		t.Errorf("expected owned_by 'multiple' when two nodes serve the model, got %q", models["llama3.1:8b"])
	}
}

func TestModels_ListAllIncludesUnavailable(t *testing.T) {
	db := setupTestDB(t)
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)
	_ = db.UpsertPricing("qwen2.5-coder:7b", 0.002, 0.002, 500)

	reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: "http://n1:11434", Models: []string{"llama3.1:8b"}})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, db, nil, Options{ModelsListAll: true})

	models := listModels(t, h)
	if len(models) != 2 {
		t.Fatalf("expected full priced catalog (2 models) with MODELS_LIST_ALL, got %d: %v", len(models), models)
	}
	if models["llama3.1:8b"] != "n1" {
		t.Errorf("expected owned_by 'n1' for available model, got %q", models["llama3.1:8b"])
	}
	if models["qwen2.5-coder:7b"] != "local" {
		t.Errorf("expected neutral owned_by 'local' for unavailable model, got %q", models["qwen2.5-coder:7b"])
	}
}

func TestModels_UnhealthyNodeExcluded(t *testing.T) {
	db := setupTestDB(t)
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: "http://n1:11434", Models: []string{"llama3.1:8b"}, Unhealthy: true})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, db, nil, Options{})

	models := listModels(t, h)
	if len(models) != 0 {
		t.Errorf("expected no models when the only node is unhealthy, got %v", models)
	}
}

// --- Credit integration tests (require DATABASE_URL) ---

func TestCreditIntegration_UnknownModel_Returns400(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("model-test@example.com", "hash", "ModelTest")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{"id": "test"})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"unknown-model","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown model, got %d", rec.Code)
	}
}

// TestCreditIntegration_ModelUnavailable_NoHoldCreated is the
// resolve-before-reserve ordering proof: a 503 model_unavailable must be
// returned BEFORE any credit hold is taken, so outages cause zero hold churn.
func TestCreditIntegration_ModelUnavailable_NoHoldCreated(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("unavail-test@example.com", "hash", "UnavailTest")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	// The model is priced but NO healthy node serves it.
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(registry.New(), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 model_unavailable, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "model_unavailable", "server_error")

	var holds int
	if err := db.Pool().QueryRow(context.Background(), "SELECT COUNT(*) FROM credit_holds").Scan(&holds); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	if holds != 0 {
		t.Errorf("expected ZERO credit holds for unresolvable model (resolve must precede reserve), got %d", holds)
	}
	bal, _ := db.GetCreditBalance(accID)
	if bal.Reserved != 0 {
		t.Errorf("expected reserved balance 0, got %f", bal.Reserved)
	}
	if bal.Balance != 1000 {
		t.Errorf("expected untouched balance 1000, got %f", bal.Balance)
	}
}

func TestCreditIntegration_InsufficientCredits_Returns402(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("insuff-test@example.com", "hash", "InsuffTest")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{"id": "test"})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Errorf("expected 402 for insufficient credits, got %d", rec.Code)
	}
}

func TestCreditIntegration_SettlesAfterResponse(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("settle-test@example.com", "hash", "SettleTest")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	ollamaResp := map[string]any{
		"id": "chatcmpl-123", "object": "chat.completion", "model": "llama3.1:8b",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hello!"}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	bal, _ := db.GetCreditBalance(accID)
	if bal.Balance >= 1000 {
		t.Errorf("expected balance < 1000 after settlement, got %f", bal.Balance)
	}
	if bal.Reserved != 0 {
		t.Errorf("expected reserved 0 after settlement, got %f", bal.Reserved)
	}

	// The proxy must forward the settled cost to the async usage writer so
	// the column `usage_logs.credits_charged` ends up non-zero, and the
	// entry must carry the resolved node's ID.
	select {
	case entry := <-usageCh:
		if entry.CreditsCharged <= 0 {
			t.Errorf("expected CreditsCharged > 0 after successful settlement, got %f", entry.CreditsCharged)
		}
		// Cost must match balance delta.
		cost := 1000 - bal.Balance
		if diff := entry.CreditsCharged - cost; diff < -0.0001 || diff > 0.0001 {
			t.Errorf("expected CreditsCharged=%f to match balance delta, got %f", cost, entry.CreditsCharged)
		}
		if entry.NodeID == nil || *entry.NodeID != 1 {
			t.Errorf("expected NodeID=1, got %v", entry.NodeID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage entry after settlement")
	}
}

func TestCreditIntegration_LegacyKeyLogsZeroCredits(t *testing.T) {
	db := setupTestDB(t)

	ollamaResp := map[string]any{
		"id": "chatcmpl-legacy", "object": "chat.completion", "model": "llama3.1:8b",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hi"}}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	// Legacy admin key: no AccountID, so credit plumbing is bypassed.
	key := &store.APIKey{ID: 1, Name: "legacy"}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	select {
	case entry := <-usageCh:
		if entry.CreditsCharged != 0 {
			t.Errorf("expected CreditsCharged=0 for legacy key, got %f", entry.CreditsCharged)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage entry")
	}
}

func TestCreditIntegration_UpstreamError_ReleasesHold(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("release-test@example.com", "hash", "ReleaseTest")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	upstream := mockOllamaChatNonStreaming(http.StatusInternalServerError, map[string]any{"error": "internal error"})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	bal, _ := db.GetCreditBalance(accID)
	if bal.Balance != 1000 {
		t.Errorf("expected balance 1000 after error release, got %f", bal.Balance)
	}
}

func TestCreditIntegration_Models_WithDB(t *testing.T) {
	db := setupTestDB(t)
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)
	_ = db.UpsertPricing("qwen2.5-coder:7b", 0.002, 0.002, 500)

	// Both priced models are served by a healthy node, so both appear.
	reg := testRegistry(t, testNode{ID: 1, Name: "n1", URL: "http://localhost:11434", Models: []string{"llama3.1:8b", "qwen2.5-coder:7b"}})
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(reg, usageCh, 52428800, db, nil, Options{})

	models := listModels(t, h)
	if len(models) != 2 {
		t.Errorf("expected 2 models, got %d: %v", len(models), models)
	}
}

func TestCreditIntegration_SessionLimit_Returns429(t *testing.T) {
	db := setupTestDB(t)
	accID, userID, _ := db.RegisterUser("session-limit@example.com", "hash", "SessLimit")
	_ = db.AddCredits(accID, 10000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	keyID, _ := db.CreateKeyForAccount(userID, accID, "limited-key", "hash-limited", "sk-lim00", 60)

	for i := 0; i < 10; i++ {
		_ = db.LogUsage(store.UsageEntry{APIKeyID: keyID, Model: "llama3.1:8b", TotalTokens: 1000, Status: "completed"})
	}

	upstream := mockOllamaChatNonStreaming(http.StatusOK, map[string]any{"id": "test"})
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	sessionLimit := 5000
	key := &store.APIKey{ID: keyID, Name: "limited-key", AccountID: &accID, SessionTokenLimit: &sessionLimit}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for session limit exceeded, got %d", rec.Code)
	}
}

func TestCreditIntegration_NonStreaming_NoUsageTokens_EstimatesFromBody(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("estimate-body@example.com", "hash", "EstBody")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	// Response without usage data — tokens will be estimated from body size
	ollamaResp := map[string]any{
		"id": "chatcmpl-no-usage", "object": "chat.completion", "model": "llama3.1:8b",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hello world!"}}},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Balance should be reduced (estimated from body)
	bal, _ := db.GetCreditBalance(accID)
	if bal.Balance >= 1000 {
		t.Errorf("expected balance < 1000 after body estimation, got %f", bal.Balance)
	}
}

func TestCreditIntegration_WithMaxTokens(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("maxtok@example.com", "hash", "MaxTok")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	ollamaResp := map[string]any{
		"id": "chatcmpl-mt", "object": "chat.completion", "model": "llama3.1:8b",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "Hi"}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	upstream := mockOllamaChatNonStreaming(http.StatusOK, ollamaResp)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	// Request with max_tokens set — should affect reserve estimate
	reqBody := `{"model":"llama3.1:8b","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestCreditIntegration_StreamingSettlement(t *testing.T) {
	db := setupTestDB(t)
	accID, _, _ := db.RegisterUser("stream-credit@example.com", "hash", "StreamCredit")
	_ = db.AddCredits(accID, 1000, "grant")
	_ = db.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500)

	upstream := mockOllamaChatStreaming()
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3.1:8b"), usageCh, 52428800, db, nil, Options{})

	reqBody := `{"model":"llama3.1:8b","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	key := &store.APIKey{ID: 1, Name: "test", AccountID: &accID}
	req = addKeyToRequest(req, key)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	bal, _ := db.GetCreditBalance(accID)
	if bal.Balance >= 1000 {
		t.Errorf("expected balance < 1000 after streaming settlement, got %f", bal.Balance)
	}
}

package proxy

// Streaming usage injection tests (docs: billing gap found 2026-07-27).
//
// OpenAI-compatible backends only emit the final stream usage chunk when the
// request sets stream_options.include_usage — a detail Open WebUI 0.10
// stopped sending, which silently degraded every streamed chat to the
// bytes-written estimate (~30-50x inflated by SSE envelope overhead). The
// proxy now injects include_usage upstream when the client didn't ask and
// suppresses the synthetic usage-only chunk so clients receive exactly the
// stream shape they requested.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// mockUsageAwareStreaming mimics real Ollama: it records the request body it
// received and emits the usage chunk ONLY when stream_options.include_usage
// is true — unlike mockOllamaChatStreaming, which sends usage
// unconditionally (the blind spot that let the billing gap ship).
func mockUsageAwareStreaming(t *testing.T) (*httptest.Server, func() []byte) {
	t.Helper()
	var mu sync.Mutex
	var lastBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body := new(bytes.Buffer)
		body.ReadFrom(r.Body)
		mu.Lock()
		lastBody = append([]byte(nil), body.Bytes()...)
		mu.Unlock()

		var req struct {
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.Unmarshal(body.Bytes(), &req)
		wantUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":" world"}}]}`)
		if wantUsage {
			fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))

	return srv, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return lastBody
	}
}

func streamRequest(t *testing.T, h http.Handler, body map[string]any) (*httptest.ResponseRecorder, chan store.UsageEntry) {
	t.Helper()
	reqBody, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req = addKeyToRequest(req, testAPIKey())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, nil
}

func waitUsage(t *testing.T, usageCh chan store.UsageEntry) store.UsageEntry {
	t.Helper()
	select {
	case entry := <-usageCh:
		return entry
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage entry")
		return store.UsageEntry{}
	}
}

func TestStreaming_InjectsUsageWhenClientDidNotAsk(t *testing.T) {
	upstream, lastBody := mockUsageAwareStreaming(t)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	rec, _ := streamRequest(t, h, map[string]any{
		"model":    "llama3:latest",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Upstream must have been asked for usage even though the client wasn't.
	var up struct {
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(lastBody(), &up); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if up.StreamOptions == nil || !up.StreamOptions.IncludeUsage {
		t.Errorf("expected stream_options.include_usage=true injected upstream, got %s", lastBody())
	}

	// Real token counts recorded, not zeros.
	entry := waitUsage(t, usageCh)
	if entry.PromptTokens != 10 || entry.CompletionTokens != 5 || entry.TotalTokens != 15 {
		t.Errorf("expected 10/5/15 tokens, got %d/%d/%d", entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens)
	}

	// The synthetic usage-only chunk must NOT reach a client that never
	// asked for it; content and terminator must.
	body := rec.Body.String()
	if strings.Contains(body, "prompt_tokens") {
		t.Errorf("synthetic usage chunk leaked to client: %s", body)
	}
	for _, want := range []string{"Hello", " world", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in relayed stream, got %s", want, body)
		}
	}
}

func TestStreaming_PassthroughWhenClientRequestedUsage(t *testing.T) {
	upstream, lastBody := mockUsageAwareStreaming(t)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	rec, _ := streamRequest(t, h, map[string]any{
		"model":          "llama3:latest",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]string{{"role": "user", "content": "Hello"}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Body forwarded with the client's own include_usage intact.
	var up struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(lastBody(), &up); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if up.StreamOptions["include_usage"] != true {
		t.Errorf("expected client's include_usage forwarded, got %s", lastBody())
	}

	// Client asked for usage → the usage chunk is theirs to keep.
	if !strings.Contains(rec.Body.String(), "prompt_tokens") {
		t.Errorf("expected usage chunk relayed to requesting client, got %s", rec.Body.String())
	}

	entry := waitUsage(t, usageCh)
	if entry.TotalTokens != 15 {
		t.Errorf("expected 15 total tokens, got %d", entry.TotalTokens)
	}
}

func TestStreaming_PreservesOtherStreamOptions(t *testing.T) {
	upstream, lastBody := mockUsageAwareStreaming(t)
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	rec, _ := streamRequest(t, h, map[string]any{
		"model":          "llama3:latest",
		"stream":         true,
		"stream_options": map[string]any{"custom_option": 7},
		"messages":       []map[string]string{{"role": "user", "content": "Hello"}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var up struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(lastBody(), &up); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if up.StreamOptions["include_usage"] != true {
		t.Errorf("expected include_usage=true added, got %s", lastBody())
	}
	if up.StreamOptions["custom_option"] != float64(7) {
		t.Errorf("expected custom_option preserved alongside injection, got %s", lastBody())
	}
}

func TestStreaming_UsageOnContentChunkNotSuppressed(t *testing.T) {
	// Some backends attach usage to the final CONTENT chunk instead of a
	// choices-empty synthetic one; that chunk carries data the client needs
	// and must never be suppressed.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","choices":[{"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	rec, _ := streamRequest(t, h, map[string]any{
		"model":    "llama3:latest",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hi"`) {
		t.Errorf("content chunk carrying usage was suppressed: %s", body)
	}
	entry := waitUsage(t, usageCh)
	if entry.TotalTokens != 5 {
		t.Errorf("expected 5 total tokens recorded, got %d", entry.TotalTokens)
	}
}

func TestStreaming_NoUsageFromUpstream_StillRelays(t *testing.T) {
	// Upstream that ignores include_usage entirely (no usage chunk): the
	// stream must relay unharmed and the entry records zero tokens (the
	// settle path's byte estimate is exercised elsewhere with a DB).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","choices":[{"delta":{"content":"Hi"}}]}`)
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(singleNodeRegistry(t, upstream.URL, "llama3:latest"), usageCh, 52428800, nil, nil, Options{})

	rec, _ := streamRequest(t, h, map[string]any{
		"model":    "llama3:latest",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("expected [DONE] relayed, got %s", rec.Body.String())
	}
	entry := waitUsage(t, usageCh)
	if entry.TotalTokens != 0 {
		t.Errorf("expected 0 tokens without upstream usage, got %d", entry.TotalTokens)
	}
}

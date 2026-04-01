package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	ollamaURL    *url.URL
	reverseProxy *httputil.ReverseProxy
	client       *http.Client
	usageCh      chan<- store.UsageEntry
	maxBody      int64
}

// requestMeta holds fields peeked from the request body.
type requestMeta struct {
	Model  string `json:"model"`
	Stream *bool  `json:"stream"`
}

// usageData is extracted from Ollama's response for logging.
type usageData struct {
	Model string
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func NewHandler(ollamaRawURL string, usageCh chan<- store.UsageEntry, maxBody int64) http.Handler {
	target, err := url.Parse(ollamaRawURL)
	if err != nil {
		log.Fatalf("invalid OLLAMA_URL: %v", err)
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		req.Header.Del("Authorization")
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	return &handler{
		ollamaURL:    target,
		reverseProxy: rp,
		client:       client,
		usageCh:      usageCh,
		maxBody:      maxBody,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only allow specific endpoints
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		h.handleChatCompletions(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		h.handleModels(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "invalid_request_error", "Not found")
	}
}

func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// Straight passthrough via reverse proxy
	h.reverseProxy.ServeHTTP(w, r)
}

func (h *handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := auth.KeyFromContext(r.Context())

	// Read and peek at request body
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "Request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid_request_error", "Failed to read request body")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var meta requestMeta
	json.Unmarshal(body, &meta) // best-effort parse

	isStream := meta.Stream != nil && *meta.Stream

	if isStream {
		h.handleStreaming(w, r, body, meta.Model, key, start)
	} else {
		h.handleNonStreaming(w, r, meta.Model, key, start)
	}
}

func (h *handler) handleNonStreaming(w http.ResponseWriter, r *http.Request, model string, key *store.APIKey, start time.Time) {
	// Use a response recorder to tee the response for usage extraction
	rec := &responseRecorder{ResponseWriter: w, body: &bytes.Buffer{}}

	h.reverseProxy.ModifyResponse = func(resp *http.Response) error {
		// Read the response body for observation
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			resp.Body = io.NopCloser(bytes.NewReader(nil))
			return nil
		}

		// Extract usage data
		var ud usageData
		if json.Unmarshal(respBody, &ud) == nil && ud.Usage.TotalTokens > 0 {
			h.logUsage(key, ud, time.Since(start), "completed")
		} else if key != nil {
			// Log with 0 tokens if extraction failed
			ud.Model = model
			h.logUsage(key, ud, time.Since(start), "completed")
		}

		// Pass through unmodified
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		resp.ContentLength = int64(len(respBody))
		return nil
	}

	h.reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy error: %v", err)
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error")
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "server_error", "Failed to connect to upstream model server")
	}

	h.reverseProxy.ServeHTTP(rec, r)
}

func (h *handler) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte, model string, key *store.APIKey, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "server_error", "Streaming not supported")
		return
	}

	// Build upstream request with context propagation
	upstreamURL := *h.ollamaURL
	upstreamURL.Path = "/v1/chat/completions"

	ctx := r.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(upReq)
	if err != nil {
		if ctx.Err() != nil {
			// Client disconnected
			if key != nil {
				h.logUsage(key, usageData{Model: model}, time.Since(start), "partial")
			}
			return
		}
		log.Printf("upstream error: %v", err)
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error")
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "server_error", "Failed to connect to upstream model server")
		return
	}
	defer resp.Body.Close()

	// Copy upstream headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		// Non-200 from upstream — pass through the error body
		io.Copy(w, resp.Body)
		flusher.Flush()
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error")
		}
		return
	}

	// Raw tee passthrough with observation
	var ud usageData
	ud.Model = model
	reader := bufio.NewReader(resp.Body)
	status := "completed"

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			flusher.Flush()

			// Observe: look for usage in the line (non-destructive)
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data: ")) {
				data := bytes.TrimPrefix(trimmed, []byte("data: "))
				if !bytes.Equal(data, []byte("[DONE]")) {
					// Try to extract usage from this chunk
					var chunk struct {
						Usage *struct {
							PromptTokens     int `json:"prompt_tokens"`
							CompletionTokens int `json:"completion_tokens"`
							TotalTokens      int `json:"total_tokens"`
						} `json:"usage"`
					}
					if json.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
						ud.Usage.PromptTokens = chunk.Usage.PromptTokens
						ud.Usage.CompletionTokens = chunk.Usage.CompletionTokens
						ud.Usage.TotalTokens = chunk.Usage.TotalTokens
					}
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				if ctx.Err() != nil {
					status = "partial"
				} else {
					status = "error"
					log.Printf("stream read error: %v", err)
				}
			}
			break
		}
	}

	if key != nil {
		h.logUsage(key, ud, time.Since(start), status)
	}
}

func (h *handler) logUsage(key *store.APIKey, ud usageData, duration time.Duration, status string) {
	if key == nil {
		return
	}
	entry := store.UsageEntry{
		APIKeyID:         key.ID,
		Model:            ud.Model,
		PromptTokens:     ud.Usage.PromptTokens,
		CompletionTokens: ud.Usage.CompletionTokens,
		TotalTokens:      ud.Usage.TotalTokens,
		DurationMs:       duration.Milliseconds(),
		Status:           status,
	}
	// Non-blocking send — drop if full
	select {
	case h.usageCh <- entry:
	default:
		log.Println("usage channel full, dropping entry")
	}
}

func writeError(w http.ResponseWriter, statusCode int, code, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// responseRecorder wraps ResponseWriter to capture the body for observation.
type responseRecorder struct {
	http.ResponseWriter
	body       *bytes.Buffer
	statusCode int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher for the recorder.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap allows http.ResponseController to access the underlying ResponseWriter.
func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// WriteError is exported for use by other packages.
func WriteError(w http.ResponseWriter, statusCode int, code, errType, message string) {
	writeError(w, statusCode, code, errType, message)
}

// StripTrailingSlash removes a single trailing slash if the path has more than just "/".
func StripTrailingSlash(path string) string {
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		return path[:len(path)-1]
	}
	return path
}

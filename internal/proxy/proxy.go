package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/credits"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	ollamaURL *url.URL
	client    *http.Client
	usageCh   chan<- store.UsageEntry
	maxBody   int64
	db        *store.Store // nil = credits disabled
}

// requestMeta holds fields peeked from the request body.
type requestMeta struct {
	Model               string `json:"model"`
	Stream              *bool  `json:"stream"`
	MaxTokens           *int   `json:"max_tokens"`
	MaxCompletionTokens *int   `json:"max_completion_tokens"`
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

func NewHandler(ollamaRawURL string, usageCh chan<- store.UsageEntry, maxBody int64, db *store.Store) http.Handler {
	target, err := url.Parse(ollamaRawURL)
	if err != nil {
		log.Fatalf("invalid OLLAMA_URL: %v", err)
	}

	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	return &handler{
		ollamaURL: target,
		client:    client,
		usageCh:   usageCh,
		maxBody:   maxBody,
		db:        db,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chat/completions":
		h.handleChatCompletions(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/models":
		h.handleModels(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "invalid_request_error", "Not found")
	}
}

func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "server_error", "Model listing unavailable")
		return
	}

	pricing, err := h.db.ListActivePricing()
	if err != nil {
		log.Printf("list pricing error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list models")
		return
	}

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}

	models := make([]modelEntry, len(pricing))
	for i, p := range pricing {
		models[i] = modelEntry{
			ID:      p.ModelID,
			Object:  "model",
			OwnedBy: "local",
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
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

	var meta requestMeta
	json.Unmarshal(body, &meta) // best-effort parse

	// --- Credit enforcement (only for keys with AccountID) ---
	var holdID int64
	var pricing *store.CreditPricing
	creditEnabled := key != nil && key.AccountID != nil && h.db != nil

	if creditEnabled {
		// Model allowlist: reject unpriced models
		pricing, err = h.db.GetPricingByModel(meta.Model)
		if err != nil {
			log.Printf("pricing lookup error: %v", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to check model pricing")
			return
		}
		if pricing == nil {
			writeError(w, http.StatusBadRequest, "unknown_model", "invalid_request_error",
				fmt.Sprintf("Model %q is not available", meta.Model))
			return
		}

		// Session limit check (best-effort)
		if key.SessionTokenLimit != nil {
			maxTokens := meta.MaxTokens
			if maxTokens == nil {
				maxTokens = meta.MaxCompletionTokens
			}
			estimatedPrompt := credits.EstimatePromptTokens(len(body))
			stats, _ := h.db.GetAccountUsageStats(*key.AccountID, meta.Model)
			estimatedCompletion := credits.EstimateCompletionTokens(maxTokens, stats, pricing)
			estimatedTotal := estimatedPrompt + estimatedCompletion

			consumed, oldest, err := h.db.GetSessionTokenUsage(key.ID, 6*time.Hour)
			if err != nil {
				log.Printf("session usage error: %v", err)
			} else if consumed+estimatedTotal > *key.SessionTokenLimit {
				retryAfter := 6 * time.Hour
				if oldest != nil {
					retryAfter = time.Until(oldest.Add(6 * time.Hour))
					if retryAfter < 0 {
						retryAfter = time.Second
					}
				}
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())))
				writeError(w, http.StatusTooManyRequests, "session_limit_exceeded", "rate_limit_error",
					"Session token limit exceeded")
				return
			}
		}

		// Estimate cost and reserve
		maxTok := meta.MaxTokens
		if maxTok == nil {
			maxTok = meta.MaxCompletionTokens
		}
		promptEst := credits.EstimatePromptTokens(len(body))
		stats, _ := h.db.GetAccountUsageStats(*key.AccountID, meta.Model)
		completionEst := credits.EstimateCompletionTokens(maxTok, stats, pricing)
		reserveAmount := credits.EstimateCost(pricing, promptEst, completionEst)

		holdID, err = h.db.ReserveCredits(*key.AccountID, reserveAmount)
		if err != nil {
			writeError(w, http.StatusPaymentRequired, "insufficient_credits", "invalid_request_error",
				"Insufficient credits for this request")
			return
		}
	}

	isStream := meta.Stream != nil && *meta.Stream
	if isStream {
		h.handleStreaming(w, r, body, meta.Model, key, start, holdID, creditEnabled, pricing)
	} else {
		h.handleNonStreaming(w, r, body, meta.Model, key, start, holdID, creditEnabled, pricing)
	}
}

func (h *handler) handleNonStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	model string, key *store.APIKey, start time.Time,
	holdID int64, creditEnabled bool, pricing *store.CreditPricing) {

	// Build upstream request (direct http.Client, no shared reverseProxy)
	upstreamURL := *h.ollamaURL
	upstreamURL.Path = "/v1/chat/completions"

	ctx := r.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(upReq)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		if ctx.Err() != nil {
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

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		log.Printf("read response error: %v", err)
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error")
		}
		writeError(w, http.StatusBadGateway, "upstream_error", "server_error", "Failed to read upstream response")
		return
	}

	// Extract usage data
	var ud usageData
	ud.Model = model
	status := "completed"
	if resp.StatusCode != http.StatusOK {
		status = "error"
	} else {
		json.Unmarshal(respBody, &ud)
	}

	// Credit settlement
	if creditEnabled {
		h.settleCredits(holdID, key, &ud, len(respBody), status, pricing)
	}

	// Log usage
	if key != nil {
		h.logUsage(key, ud, time.Since(start), status)
	}

	// Write response to client
	for hk, hv := range resp.Header {
		for _, v := range hv {
			w.Header().Add(hk, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (h *handler) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	model string, key *store.APIKey, start time.Time,
	holdID int64, creditEnabled bool, pricing *store.CreditPricing) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "server_error", "Streaming not supported")
		return
	}

	upstreamURL := *h.ollamaURL
	upstreamURL.Path = "/v1/chat/completions"

	ctx := r.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(upReq)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		if ctx.Err() != nil {
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
	for headerKey, headerValues := range resp.Header {
		for _, headerValue := range headerValues {
			w.Header().Add(headerKey, headerValue)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		io.Copy(w, resp.Body)
		flusher.Flush()
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error")
		}
		return
	}

	// Raw tee passthrough with observation
	var ud usageData
	ud.Model = model
	lineReader := bufio.NewReader(resp.Body)
	status := "completed"
	bytesWritten := 0

	for {
		line, err := lineReader.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			flusher.Flush()
			bytesWritten += len(line)

			// Observe: look for usage in the line (non-destructive)
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data: ")) {
				data := bytes.TrimPrefix(trimmed, []byte("data: "))
				if !bytes.Equal(data, []byte("[DONE]")) {
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

	// Credit settlement
	if creditEnabled {
		h.settleStreamCredits(holdID, key, &ud, bytesWritten, status, pricing)
	}

	if key != nil {
		h.logUsage(key, ud, time.Since(start), status)
	}
}

// settleCredits handles credit settlement for non-streaming responses.
func (h *handler) settleCredits(holdID int64, key *store.APIKey, ud *usageData, respBodyLen int, status string, pricing *store.CreditPricing) {
	if status == "error" && respBodyLen == 0 {
		h.db.ReleaseHold(holdID)
		return
	}

	promptTokens := ud.Usage.PromptTokens
	completionTokens := ud.Usage.CompletionTokens

	// Fallback: estimate from response body size if token extraction failed
	if ud.Usage.TotalTokens == 0 && respBodyLen > 0 {
		completionTokens = credits.EstimateFromResponseBytes(respBodyLen)
		promptTokens = 0 // prompt cost was in the reserve estimate
	}

	actualCost := credits.EstimateCost(pricing, promptTokens, completionTokens)
	if err := h.db.SettleHold(holdID, actualCost); err != nil {
		log.Printf("settle hold error: %v", err)
	}

	// Update usage stats for future estimates
	if key != nil && key.AccountID != nil && completionTokens > 0 {
		h.db.UpdateAccountUsageStats(*key.AccountID, ud.Model, completionTokens)
	}
}

// settleStreamCredits handles credit settlement for streaming responses.
func (h *handler) settleStreamCredits(holdID int64, key *store.APIKey, ud *usageData, bytesWritten int, status string, pricing *store.CreditPricing) {
	if status == "error" && bytesWritten == 0 {
		h.db.ReleaseHold(holdID)
		return
	}

	promptTokens := ud.Usage.PromptTokens
	completionTokens := ud.Usage.CompletionTokens

	// Fallback: estimate from bytes written if token extraction failed
	if ud.Usage.TotalTokens == 0 && bytesWritten > 0 {
		completionTokens = credits.EstimateFromResponseBytes(bytesWritten)
		promptTokens = 0
	}

	actualCost := credits.EstimateCost(pricing, promptTokens, completionTokens)
	if err := h.db.SettleHold(holdID, actualCost); err != nil {
		log.Printf("settle hold error: %v", err)
	}

	if key != nil && key.AccountID != nil && completionTokens > 0 {
		h.db.UpdateAccountUsageStats(*key.AccountID, ud.Model, completionTokens)
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

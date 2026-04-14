package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/credits"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	ollamaURL *url.URL
	client    *http.Client
	usageCh   chan<- store.UsageEntry
	maxBody   int64
	db        *store.Store     // nil = credits disabled
	metrics   *metrics.Metrics // nil = metrics disabled
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

func NewHandler(ollamaURL *url.URL, usageCh chan<- store.UsageEntry, maxBody int64, db *store.Store, m *metrics.Metrics) http.Handler {
	return &handler{
		ollamaURL: ollamaURL,
		client:    &http.Client{Timeout: 5 * time.Minute},
		usageCh:   usageCh,
		maxBody:   maxBody,
		db:        db,
		metrics:   m,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chat/completions":
		h.handleChatCompletions(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/models":
		h.handleModels(w, r)
	default:
		writeError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Not found")
	}
}

func (h *handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeError(w, r, http.StatusServiceUnavailable, "unavailable", "server_error", "Model listing unavailable")
		return
	}

	pricing, err := h.db.ListActivePricing()
	if err != nil {
		slog.ErrorContext(r.Context(), "list pricing error", "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list models")
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
			writeError(w, r, http.StatusRequestEntityTooLarge, "request_too_large", "invalid_request_error", "Request body too large")
			return
		}
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid_request_error", "Failed to read request body")
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
			slog.ErrorContext(r.Context(), "pricing lookup error", "error", err, "model", meta.Model)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to check model pricing")
			return
		}
		if pricing == nil {
			writeError(w, r, http.StatusBadRequest, "unknown_model", "invalid_request_error",
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
			stats, statsErr := h.db.GetAccountUsageStats(*key.AccountID, meta.Model)
			if statsErr != nil {
				slog.WarnContext(r.Context(), "session usage stats lookup error", "error", statsErr, "account_id", *key.AccountID, "model", meta.Model)
			}
			estimatedCompletion := credits.EstimateCompletionTokens(maxTokens, stats, pricing)
			estimatedTotal := estimatedPrompt + estimatedCompletion

			consumed, oldest, err := h.db.GetSessionTokenUsage(key.ID, 6*time.Hour)
			if err != nil {
				slog.ErrorContext(r.Context(), "session usage error", "error", err, "api_key_id", key.ID)
			} else if consumed+estimatedTotal > *key.SessionTokenLimit {
				retryAfter := 6 * time.Hour
				if oldest != nil {
					retryAfter = time.Until(oldest.Add(6 * time.Hour))
					if retryAfter < 0 {
						retryAfter = time.Second
					}
				}
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())))
				writeError(w, r, http.StatusTooManyRequests, "session_limit_exceeded", "rate_limit_error",
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
		stats, statsErr := h.db.GetAccountUsageStats(*key.AccountID, meta.Model)
		if statsErr != nil {
			slog.WarnContext(r.Context(), "usage stats lookup error", "error", statsErr, "account_id", *key.AccountID, "model", meta.Model)
		}
		completionEst := credits.EstimateCompletionTokens(maxTok, stats, pricing)
		reserveAmount := credits.EstimateCost(pricing, promptEst, completionEst)

		holdID, err = h.db.ReserveCredits(*key.AccountID, reserveAmount)
		if err != nil {
			writeError(w, r, http.StatusPaymentRequired, "insufficient_credits", "invalid_request_error",
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
		writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
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
				h.logUsage(key, usageData{Model: model}, time.Since(start), "partial", 0)
			}
			return
		}
		slog.ErrorContext(ctx, "upstream error", "error", err, "model", model)
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error", 0)
		}
		writeError(w, r, http.StatusBadGateway, "upstream_error", "server_error", "Failed to connect to upstream model server")
		return
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		slog.ErrorContext(ctx, "read response error", "error", err, "model", model)
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error", 0)
		}
		writeError(w, r, http.StatusBadGateway, "upstream_error", "server_error", "Failed to read upstream response")
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
	var actualCost float64
	if creditEnabled {
		if resp.StatusCode != http.StatusOK {
			// Upstream error — release hold, no charge
			h.db.ReleaseHold(holdID)
		} else {
			actualCost = h.settleCredits(holdID, key, &ud, len(respBody), pricing)
		}
	}

	// Log usage and record metrics
	if key != nil {
		h.logUsage(key, ud, time.Since(start), status, actualCost)
	}
	h.metrics.RecordTokens(model, ud.Usage.PromptTokens, ud.Usage.CompletionTokens)

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
		writeError(w, r, http.StatusInternalServerError, "streaming_unsupported", "server_error", "Streaming not supported")
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
		writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
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
				h.logUsage(key, usageData{Model: model}, time.Since(start), "partial", 0)
			}
			return
		}
		slog.ErrorContext(ctx, "upstream error", "error", err, "model", model, "stream", true)
		if key != nil {
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error", 0)
		}
		writeError(w, r, http.StatusBadGateway, "upstream_error", "server_error", "Failed to connect to upstream model server")
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
			h.logUsage(key, usageData{Model: model}, time.Since(start), "error", 0)
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
					slog.ErrorContext(ctx, "stream read error", "error", err, "model", model)
				}
			}
			break
		}
	}

	// Credit settlement
	var actualCost float64
	if creditEnabled {
		actualCost = h.settleStreamCredits(holdID, key, &ud, bytesWritten, status, pricing)
	}

	if key != nil {
		h.logUsage(key, ud, time.Since(start), status, actualCost)
	}
	h.metrics.RecordTokens(model, ud.Usage.PromptTokens, ud.Usage.CompletionTokens)
}

// settleCredits handles credit settlement for non-streaming responses and
// returns the amount actually charged (0 when the hold had already been
// released by the sweeper). Called only for successful (200) upstream
// responses.
func (h *handler) settleCredits(holdID int64, key *store.APIKey, ud *usageData, respBodyLen int, pricing *store.CreditPricing) float64 {
	promptTokens := ud.Usage.PromptTokens
	completionTokens := ud.Usage.CompletionTokens

	// Fallback: estimate from response body size if token extraction failed
	if ud.Usage.TotalTokens == 0 && respBodyLen > 0 {
		completionTokens = credits.EstimateFromResponseBytes(respBodyLen)
		promptTokens = 0 // prompt cost was in the reserve estimate
	}

	estimatedCost := credits.EstimateCost(pricing, promptTokens, completionTokens)
	charged, err := h.db.SettleHold(holdID, estimatedCost)
	if err != nil {
		slog.Error("settle hold error", "error", err, "hold_id", holdID)
	}

	// Update usage stats for future estimates
	if key != nil && key.AccountID != nil && completionTokens > 0 {
		if err := h.db.UpdateAccountUsageStats(*key.AccountID, ud.Model, completionTokens); err != nil {
			slog.Warn("update usage stats error", "error", err, "account_id", *key.AccountID, "model", ud.Model)
		}
	}
	return charged
}

// settleStreamCredits handles credit settlement for streaming responses and
// returns the amount actually charged.
func (h *handler) settleStreamCredits(holdID int64, key *store.APIKey, ud *usageData, bytesWritten int, status string, pricing *store.CreditPricing) float64 {
	if status == "error" && bytesWritten == 0 {
		h.db.ReleaseHold(holdID)
		return 0
	}

	promptTokens := ud.Usage.PromptTokens
	completionTokens := ud.Usage.CompletionTokens

	// Fallback: estimate from bytes written if token extraction failed
	if ud.Usage.TotalTokens == 0 && bytesWritten > 0 {
		completionTokens = credits.EstimateFromResponseBytes(bytesWritten)
		promptTokens = 0
	}

	estimatedCost := credits.EstimateCost(pricing, promptTokens, completionTokens)
	charged, err := h.db.SettleHold(holdID, estimatedCost)
	if err != nil {
		slog.Error("settle hold error", "error", err, "hold_id", holdID, "stream", true)
	}

	if key != nil && key.AccountID != nil && completionTokens > 0 {
		if err := h.db.UpdateAccountUsageStats(*key.AccountID, ud.Model, completionTokens); err != nil {
			slog.Warn("update usage stats error", "error", err, "account_id", *key.AccountID, "model", ud.Model)
		}
	}
	return charged
}

func (h *handler) logUsage(key *store.APIKey, ud usageData, duration time.Duration, status string, creditsCharged float64) {
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
		CreditsCharged:   creditsCharged,
	}
	select {
	case h.usageCh <- entry:
	default:
		slog.Warn("usage channel full, dropping entry", "model", ud.Model, "api_key_id", key.ID)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, statusCode int, code, errType, message string) {
	apierror.WriteError(w, r, statusCode, code, errType, message)
}

// WriteError is exported for use by other packages.
// Deprecated: Use apierror.WriteError directly for new code.
func WriteError(w http.ResponseWriter, r *http.Request, statusCode int, code, errType, message string) {
	apierror.WriteError(w, r, statusCode, code, errType, message)
}

// StripTrailingSlash removes a single trailing slash if the path has more than just "/".
func StripTrailingSlash(path string) string {
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		return path[:len(path)-1]
	}
	return path
}

// Package proxy is the OpenAI-compatible client-facing surface: it validates
// chat-completion requests, routes them by model through the node registry
// (docs/design/distributed-nodes.md, "Request routing"), enforces credits,
// and forwards to the resolved backend node.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/credits"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// defaultUpstreamTimeout is the per-request deadline applied when a node has
// no timeout_seconds override. It replaces the old client-level 5-minute
// http.Client.Timeout (which would cap long SSE streams for every node).
const defaultUpstreamTimeout = 5 * time.Minute

// errRedirectRefused is returned from the chat client's CheckRedirect: with
// a per-node auth_header attached, following a redirect could exfiltrate
// backend credentials to an attacker-influenced Location. A redirecting
// upstream fails the request (502) and the redirect target is never
// contacted.
var errRedirectRefused = errors.New("upstream redirect refused (chat forwarding never follows redirects)")

// Registry is the routing view the proxy needs from the node registry:
// Resolve for chat forwarding, Snapshot for the /v1/models intersection.
// *registry.Registry satisfies it; tests may substitute fakes.
type Registry interface {
	// Resolve returns a healthy node serving model, round-robining across
	// candidates; the returned error wraps registry.ErrModelUnavailable when
	// no healthy node with a known model list serves the model.
	Resolve(model string) (registry.Node, error)
	// Snapshot returns the current node states and model→nodes routing map.
	Snapshot() registry.RegistrySnapshot
}

// Options carries the handler's optional configuration.
type Options struct {
	// ModelsListAll (MODELS_LIST_ALL) makes GET /v1/models list every
	// actively priced model regardless of node availability. Default false:
	// only models served by at least one healthy node are listed.
	ModelsListAll bool
}

type handler struct {
	registry      Registry
	client        *http.Client
	usageCh       chan<- store.UsageEntry
	maxBody       int64
	db            *store.Store     // nil = credits disabled
	metrics       *metrics.Metrics // nil = metrics disabled
	modelsListAll bool
}

// requestMeta holds fields peeked from the request body.
type requestMeta struct {
	Model               string `json:"model"`
	Stream              *bool  `json:"stream"`
	MaxTokens           *int   `json:"max_tokens"`
	MaxCompletionTokens *int   `json:"max_completion_tokens"`
}

// usageData is extracted from the backend's response for logging.
type usageData struct {
	Model string
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// NewHandler builds the client-facing proxy handler. reg must be non-nil (it
// is the routing source of truth); db and m may be nil to disable credits
// and metrics respectively. maxBody caps the request body AND the upstream
// non-streaming/error response reads (MAX_REQUEST_BODY, default 50MB).
//
// The shared upstream client deliberately has NO client-level Timeout —
// per-request context deadlines carry each node's budget (see
// upstreamTimeout) so long SSE streams are never capped by a client-wide
// value — and never follows redirects (credential exfiltration guard).
func NewHandler(reg Registry, usageCh chan<- store.UsageEntry, maxBody int64, db *store.Store, m *metrics.Metrics, opts Options) http.Handler {
	return &handler{
		registry: reg,
		client: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errRedirectRefused
			},
		},
		usageCh:       usageCh,
		maxBody:       maxBody,
		db:            db,
		metrics:       m,
		modelsListAll: opts.ModelsListAll,
	}
}

// upstreamTimeout returns the per-request deadline for a node: its
// configured timeout when positive, else the 5-minute default.
func upstreamTimeout(node registry.Node) time.Duration {
	if node.Timeout > 0 {
		return node.Timeout
	}
	return defaultUpstreamTimeout
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

// handleModels lists models with active pricing that are served by at least
// one healthy node (intersection of the pricing catalog and the registry
// snapshot). With ModelsListAll the full priced catalog is returned instead.
// owned_by is the node name when exactly one healthy node serves the model,
// "multiple" when more than one does, and the neutral legacy value "local"
// for a model listed under ModelsListAll that no healthy node serves.
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

	snap := h.registry.Snapshot()

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}

	models := make([]modelEntry, 0, len(pricing))
	for _, p := range pricing {
		servedBy := snap.Models[p.ModelID]
		if len(servedBy) == 0 && !h.modelsListAll {
			continue
		}
		ownedBy := "local"
		switch {
		case len(servedBy) == 1:
			ownedBy = servedBy[0].Name
		case len(servedBy) > 1:
			ownedBy = "multiple"
		}
		models = append(models, modelEntry{
			ID:      p.ModelID,
			Object:  "model",
			OwnedBy: ownedBy,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   models,
	})
}

// handleChatCompletions implements the routed request flow — the ORDER is
// part of the design (docs/design/distributed-nodes.md, "Request flow"):
//
//  1. peek + validate the body (malformed JSON / missing model → 400),
//  2. pricing check (credit-enabled keys only),
//  3. Resolve(model) → 503 model_unavailable BEFORE any credit hold, so
//     node outages cause zero hold churn,
//  4. reserve credits,
//  5. forward to the resolved node.
func (h *handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := auth.KeyFromContext(r.Context())

	// Billing account: the resolution attached by the billing middleware
	// (end-user account on trusted keys) when present, else the key's own
	// account. Reserve, settle, usage stats, and the usage row all follow it.
	var bill billingInfo
	if key != nil {
		bill.AccountID = key.AccountID
	}
	if res, ok := billing.FromContext(r.Context()); ok {
		id := res.AccountID
		bill = billingInfo{AccountID: &id, AllowanceManaged: res.AllowanceManaged}
	}

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

	// (1) Validate: the body must be JSON naming a model — routing needs it,
	// so parse errors can no longer be silently delegated upstream.
	var meta requestMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid_request_error", "Request body must be valid JSON")
		return
	}
	if strings.TrimSpace(meta.Model) == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "invalid_request_error", `Missing required "model" field`)
		return
	}

	// --- Credit enforcement (only for keys with a billing account) ---
	var pricing *store.CreditPricing
	creditEnabled := key != nil && bill.AccountID != nil && h.db != nil

	if creditEnabled {
		// (2) Model allowlist: reject unpriced models
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
			stats, statsErr := h.db.GetAccountUsageStats(*bill.AccountID, meta.Model)
			if statsErr != nil {
				slog.WarnContext(r.Context(), "session usage stats lookup error", "error", statsErr, "account_id", *bill.AccountID, "model", meta.Model)
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
	}

	// (3) Resolve the model to a healthy node BEFORE reserving credits: an
	// outage must not create (and then strand) holds behind the sweeper.
	node, err := h.registry.Resolve(meta.Model)
	if err != nil {
		if errors.Is(err, registry.ErrModelUnavailable) {
			writeError(w, r, http.StatusServiceUnavailable, "model_unavailable", "server_error",
				fmt.Sprintf("No healthy backend node currently serves model %q", meta.Model))
			return
		}
		slog.ErrorContext(r.Context(), "model resolution error", "error", err, "model", meta.Model)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to route request")
		return
	}
	slog.DebugContext(r.Context(), "routed chat request", "model", meta.Model, "node", node.Name)

	// (4) Estimate cost and reserve
	var holdID int64
	if creditEnabled {
		maxTok := meta.MaxTokens
		if maxTok == nil {
			maxTok = meta.MaxCompletionTokens
		}
		promptEst := credits.EstimatePromptTokens(len(body))
		stats, statsErr := h.db.GetAccountUsageStats(*bill.AccountID, meta.Model)
		if statsErr != nil {
			slog.WarnContext(r.Context(), "usage stats lookup error", "error", statsErr, "account_id", *bill.AccountID, "model", meta.Model)
		}
		completionEst := credits.EstimateCompletionTokens(maxTok, stats, pricing)
		reserveAmount := credits.EstimateCost(pricing, promptEst, completionEst)

		holdID, err = h.db.ReserveCredits(*bill.AccountID, reserveAmount)
		if err != nil {
			if bill.AllowanceManaged {
				writeError(w, r, http.StatusPaymentRequired, "monthly_limit_reached", "invalid_request_error",
					"Monthly usage limit reached — resets next month")
				return
			}
			writeError(w, r, http.StatusPaymentRequired, "insufficient_credits", "invalid_request_error",
				"Insufficient credits for this request")
			return
		}
	}

	// (5) Forward
	isStream := meta.Stream != nil && *meta.Stream
	if isStream {
		h.handleStreaming(w, r, body, meta.Model, node, key, bill, start, holdID, creditEnabled, pricing)
	} else {
		h.handleNonStreaming(w, r, body, meta.Model, node, key, bill, start, holdID, creditEnabled, pricing)
	}
}

// billingInfo carries the resolved billing account through the request flow.
// AccountID nil = credit-disabled path (no db or misprovisioned key —
// creditEnabled is false and no holds are taken).
type billingInfo struct {
	AccountID        *int64
	AllowanceManaged bool
}

// newUpstreamRequest builds the forwarded request for a node: the chat path
// is path-JOINED onto the node's base URL (a base may carry a prefix like
// /openai — assigning url.Path wholesale would drop it), the node's
// auth_header becomes the Authorization header when set (the client's own
// Authorization was stripped by auth middleware and is never forwarded),
// and the node's timeout budget is applied as a context deadline. The
// returned cancel must be called once the response is fully consumed.
func (h *handler) newUpstreamRequest(ctx context.Context, node registry.Node, body []byte) (*http.Request, context.CancelFunc, error) {
	upCtx, cancel := context.WithTimeout(ctx, upstreamTimeout(node))
	upstreamURL := node.BaseURL.JoinPath("v1/chat/completions")
	req, err := http.NewRequestWithContext(upCtx, http.MethodPost, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if node.AuthHeader != "" {
		req.Header.Set("Authorization", node.AuthHeader)
	}
	return req, cancel, nil
}

// copyResponseHeaders copies upstream response headers except
// Content-Length: capped bodies may be shorter than the upstream declared,
// and a stale Content-Length would corrupt the client connection. Callers
// set their own when the final body length is known.
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if http.CanonicalHeaderKey(k) == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (h *handler) handleNonStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	model string, node registry.Node, key *store.APIKey, bill billingInfo, start time.Time,
	holdID int64, creditEnabled bool, pricing *store.CreditPricing) {

	nodeID := node.ID
	upReq, cancel, err := h.newUpstreamRequest(r.Context(), node, body)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
		return
	}
	defer cancel()

	resp, err := h.client.Do(upReq)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		// Distinguish the client going away (log partial, no response
		// possible) from an upstream failure — including the per-node
		// timeout — which still owes the client a 502.
		if r.Context().Err() != nil {
			if key != nil {
				h.logUsage(key, bill, usageData{Model: model}, time.Since(start), "partial", 0, &nodeID)
			}
			return
		}
		slog.ErrorContext(r.Context(), "upstream error", "error", err, "model", model, "node", node.Name)
		if key != nil {
			h.logUsage(key, bill, usageData{Model: model}, time.Since(start), "error", 0, &nodeID)
		}
		writeError(w, r, http.StatusBadGateway, "upstream_error", "server_error", "Failed to connect to upstream model server")
		return
	}
	defer resp.Body.Close()

	// Read response body through the size cap (upstream success AND error
	// bodies): an oversized body is truncated at maxBody.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBody))
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		slog.ErrorContext(r.Context(), "read response error", "error", err, "model", model, "node", node.Name)
		if key != nil {
			h.logUsage(key, bill, usageData{Model: model}, time.Since(start), "error", 0, &nodeID)
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
			actualCost = h.settleCredits(holdID, bill, &ud, len(respBody), pricing)
		}
	}

	// Log usage and record metrics
	if key != nil {
		h.logUsage(key, bill, ud, time.Since(start), status, actualCost, &nodeID)
	}
	h.metrics.RecordTokens(model, node.Name, ud.Usage.PromptTokens, ud.Usage.CompletionTokens)

	// Write response to client
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

func (h *handler) handleStreaming(w http.ResponseWriter, r *http.Request, body []byte,
	model string, node registry.Node, key *store.APIKey, bill billingInfo, start time.Time,
	holdID int64, creditEnabled bool, pricing *store.CreditPricing) {

	nodeID := node.ID
	flusher, ok := w.(http.Flusher)
	if !ok {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		writeError(w, r, http.StatusInternalServerError, "streaming_unsupported", "server_error", "Streaming not supported")
		return
	}

	upReq, cancel, err := h.newUpstreamRequest(r.Context(), node, body)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create upstream request")
		return
	}
	defer cancel()

	resp, err := h.client.Do(upReq)
	if err != nil {
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		if r.Context().Err() != nil {
			if key != nil {
				h.logUsage(key, bill, usageData{Model: model}, time.Since(start), "partial", 0, &nodeID)
			}
			return
		}
		slog.ErrorContext(r.Context(), "upstream error", "error", err, "model", model, "node", node.Name, "stream", true)
		if key != nil {
			h.logUsage(key, bill, usageData{Model: model}, time.Since(start), "error", 0, &nodeID)
		}
		writeError(w, r, http.StatusBadGateway, "upstream_error", "server_error", "Failed to connect to upstream model server")
		return
	}
	defer resp.Body.Close()

	// Copy upstream headers (sans Content-Length: error bodies are capped)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		// Upstream error body: pass through, capped like non-streaming reads.
		io.Copy(w, io.LimitReader(resp.Body, h.maxBody))
		flusher.Flush()
		if creditEnabled {
			h.db.ReleaseHold(holdID)
		}
		if key != nil {
			h.logUsage(key, bill, usageData{Model: model}, time.Since(start), "error", 0, &nodeID)
		}
		return
	}

	// Raw tee passthrough with observation. Reads honour the request context
	// (including the per-node deadline): when it fires, ReadBytes returns an
	// error and the loop ends.
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
				if r.Context().Err() != nil {
					status = "partial"
				} else {
					status = "error"
					slog.ErrorContext(r.Context(), "stream read error", "error", err, "model", model, "node", node.Name)
				}
			}
			break
		}
	}

	// Credit settlement
	var actualCost float64
	if creditEnabled {
		actualCost = h.settleStreamCredits(holdID, bill, &ud, bytesWritten, status, pricing)
	}

	if key != nil {
		h.logUsage(key, bill, ud, time.Since(start), status, actualCost, &nodeID)
	}
	h.metrics.RecordTokens(model, node.Name, ud.Usage.PromptTokens, ud.Usage.CompletionTokens)
}

// settleCredits handles credit settlement for non-streaming responses and
// returns the amount actually charged (0 when the hold had already been
// released by the sweeper). Called only for successful (200) upstream
// responses.
func (h *handler) settleCredits(holdID int64, bill billingInfo, ud *usageData, respBodyLen int, pricing *store.CreditPricing) float64 {
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
	if bill.AccountID != nil && completionTokens > 0 {
		if err := h.db.UpdateAccountUsageStats(*bill.AccountID, ud.Model, completionTokens); err != nil {
			slog.Warn("update usage stats error", "error", err, "account_id", *bill.AccountID, "model", ud.Model)
		}
	}
	return charged
}

// settleStreamCredits handles credit settlement for streaming responses and
// returns the amount actually charged.
func (h *handler) settleStreamCredits(holdID int64, bill billingInfo, ud *usageData, bytesWritten int, status string, pricing *store.CreditPricing) float64 {
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

	if bill.AccountID != nil && completionTokens > 0 {
		if err := h.db.UpdateAccountUsageStats(*bill.AccountID, ud.Model, completionTokens); err != nil {
			slog.Warn("update usage stats error", "error", err, "account_id", *bill.AccountID, "model", ud.Model)
		}
	}
	return charged
}

// logUsage queues one usage entry. nodeID attributes the entry to the node
// that served the request; requests that never resolved a node pass nil.
func (h *handler) logUsage(key *store.APIKey, bill billingInfo, ud usageData, duration time.Duration, status string, creditsCharged float64, nodeID *int64) {
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
		NodeID:           nodeID,
		AccountID:        bill.AccountID,
	}
	select {
	case h.usageCh <- entry:
	default:
		slog.Warn("usage channel full, dropping entry", "model", ud.Model, "api_key_id", key.ID)
		h.metrics.RecordUsageDrop()
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

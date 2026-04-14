package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/ratelimit"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	store    *store.Store
	adminKey string
	usageCh  chan<- store.UsageEntry

	// X-Admin-Key rate limiter: 10 req/min, single bucket shared across all scripts/automation.
	rateLimitMu       sync.Mutex
	rateLimitTokens   float64
	rateLimitLastTime time.Time

	// Bearer session rate limiter: 300 req/min, one bucket per session (keyed by token hash).
	sessionLimiter *sessionLimiter
}

type adminSessionCtxKey struct{}

// AdminSessionFromContext retrieves the admin session attached by authMiddleware,
// or nil if the request was authenticated via X-Admin-Key.
func AdminSessionFromContext(ctx context.Context) *store.Session {
	s, _ := ctx.Value(adminSessionCtxKey{}).(*store.Session)
	return s
}

type createKeyRequest struct {
	Name      string `json:"name"`
	RateLimit int    `json:"rate_limit"`
}

type createKeyResponse struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Key       string `json:"key"`
	KeyPrefix string `json:"key_prefix"`
	RateLimit int    `json:"rate_limit"`
}

type keyResponse struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"`
	RateLimit int       `json:"rate_limit"`
	CreatedAt time.Time `json:"created_at"`
	Revoked   bool      `json:"revoked"`
}

func NewHandler(dataStore *store.Store, adminKey string, usageCh chan<- store.UsageEntry) http.Handler {
	handler := &handler{
		store:             dataStore,
		adminKey:          adminKey,
		usageCh:           usageCh,
		rateLimitTokens:   10,
		rateLimitLastTime: time.Now(),
		sessionLimiter:    newSessionLimiter(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/admin/keys", handler.createKey)
	mux.HandleFunc("GET /api/admin/keys", handler.listKeys)
	mux.HandleFunc("DELETE /api/admin/keys/{id}", handler.revokeKey)
	mux.HandleFunc("GET /api/admin/usage", handler.getUsage)
	mux.HandleFunc("GET /api/admin/usage/summary", handler.getUsageSummary)
	mux.HandleFunc("GET /api/admin/usage/by-model", handler.getUsageByModel)
	mux.HandleFunc("GET /api/admin/usage/by-user", handler.getUsageByUser)
	mux.HandleFunc("GET /api/admin/usage/timeseries", handler.getUsageTimeseries)
	mux.HandleFunc("GET /api/admin/users", handler.listUsers)
	mux.HandleFunc("PUT /api/admin/users/{id}/activate", handler.activateUser)
	mux.HandleFunc("PUT /api/admin/users/{id}/deactivate", handler.deactivateUser)

	// Credit management
	mux.HandleFunc("GET /api/admin/accounts", handler.listAccounts)
	mux.HandleFunc("POST /api/admin/accounts/{id}/credits", handler.grantCredits)
	mux.HandleFunc("POST /api/admin/accounts/{id}/keys", handler.createAccountKey)

	// Registration tokens
	mux.HandleFunc("POST /api/admin/registration-tokens", handler.createRegistrationToken)
	mux.HandleFunc("GET /api/admin/registration-tokens", handler.listRegistrationTokens)
	mux.HandleFunc("DELETE /api/admin/registration-tokens/{id}", handler.revokeRegistrationToken)

	// Pricing
	mux.HandleFunc("GET /api/admin/pricing", handler.listPricing)
	mux.HandleFunc("POST /api/admin/pricing", handler.upsertPricing)
	mux.HandleFunc("DELETE /api/admin/pricing/{id}", handler.deletePricing)

	// Session limits
	mux.HandleFunc("PUT /api/admin/keys/{id}/session-limit", handler.setSessionLimit)

	return handler.authMiddleware(mux)
}

func (h *handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// X-Admin-Key path takes precedence over Bearer. Used by scripts,
		// emergency access, and the bootstrap flow.
		if provided := r.Header.Get("X-Admin-Key"); provided != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(h.adminKey)) != 1 {
				proxy.WriteError(w, r, http.StatusUnauthorized, "invalid_admin_key", "invalid_api_key", "Invalid admin key")
				return
			}
			if !h.adminAllow() {
				w.Header().Set("Retry-After", "6")
				proxy.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded", "Admin rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Bearer session path: used by the admin UI via the next-auth BFF.
		rawToken := extractBearer(r)
		if rawToken == "" {
			proxy.WriteError(w, r, http.StatusUnauthorized, "missing_admin_key", "invalid_api_key", "Missing X-Admin-Key header or Bearer session token")
			return
		}
		tokenHash := auth.HashKey(rawToken)
		session, err := h.store.GetSessionByTokenHash(tokenHash)
		if err != nil {
			slog.ErrorContext(r.Context(), "admin session lookup error", "error", err)
			proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")
			return
		}
		if session == nil {
			proxy.WriteError(w, r, http.StatusUnauthorized, "invalid_session", "invalid_api_key", "Invalid session token")
			return
		}
		if time.Now().After(session.ExpiresAt) {
			if err := h.store.DeleteSession(tokenHash); err != nil {
				slog.WarnContext(r.Context(), "delete expired admin session", "error", err)
			}
			proxy.WriteError(w, r, http.StatusUnauthorized, "session_expired", "invalid_api_key", "Session has expired")
			return
		}
		u, err := h.store.GetUserByID(session.UserID)
		if err != nil {
			slog.ErrorContext(r.Context(), "admin user lookup error", "error", err)
			proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")
			return
		}
		if u == nil {
			proxy.WriteError(w, r, http.StatusUnauthorized, "user_not_found", "invalid_api_key", "User not found")
			return
		}
		if !u.IsActive {
			proxy.WriteError(w, r, http.StatusForbidden, "account_disabled", "invalid_request_error", "Account is disabled")
			return
		}
		if u.Role != "admin" {
			proxy.WriteError(w, r, http.StatusForbidden, "not_admin", "invalid_request_error", "Admin role required")
			return
		}
		if !h.sessionLimiter.Allow(tokenHash) {
			w.Header().Set("Retry-After", "1")
			proxy.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded", "Admin rate limit exceeded")
			return
		}

		ctx := context.WithValue(r.Context(), adminSessionCtxKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func extractBearer(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(authHeader, "Bearer ")
}

func (h *handler) adminAllow() bool {
	h.rateLimitMu.Lock()
	defer h.rateLimitMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(h.rateLimitLastTime).Seconds()
	h.rateLimitTokens = min(10, h.rateLimitTokens+elapsed*(10.0/60.0))
	h.rateLimitLastTime = now

	if h.rateLimitTokens >= 1 {
		h.rateLimitTokens--
		return true
	}
	return false
}

func (h *handler) createKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Name == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_name", "invalid_request_error", "name is required")
		return
	}
	rateLimit, err := ratelimit.ApplyConfigDefaultsAndCap(req.RateLimit)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "rate_limit_too_high", "invalid_request_error", "rate_limit must be <= 10000")
		return
	}
	req.RateLimit = rateLimit

	// Generate random key
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		slog.ErrorContext(r.Context(), "crypto/rand error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate key")
		return
	}
	rawKey := "sk-" + hex.EncodeToString(rawBytes)
	keyPrefix := rawKey[:11] // "sk-" + first 8 hex chars
	keyHash := auth.HashKey(rawKey)

	id, err := h.store.CreateKey(req.Name, keyHash, keyPrefix, req.RateLimit)
	if err != nil {
		slog.ErrorContext(r.Context(), "create key error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(createKeyResponse{
		ID:        id,
		Name:      req.Name,
		Key:       rawKey,
		KeyPrefix: keyPrefix,
		RateLimit: req.RateLimit,
	})
}

func (h *handler) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.ListKeys()
	if err != nil {
		slog.ErrorContext(r.Context(), "list keys error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list keys")
		return
	}

	resp := make([]keyResponse, len(keys))
	for i, apiKey := range keys {
		resp[i] = keyResponse{
			ID:        apiKey.ID,
			Name:      apiKey.Name,
			KeyPrefix: apiKey.KeyPrefix,
			RateLimit: apiKey.RateLimit,
			CreatedAt: apiKey.CreatedAt,
			Revoked:   apiKey.Revoked,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid key ID")
		return
	}

	if err := h.store.RevokeKey(id); err != nil {
		if err.Error() == "key not found" {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Key not found")
			return
		}
		slog.ErrorContext(r.Context(), "revoke key error", "error", err, "key_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to revoke key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

func (h *handler) getUsage(w http.ResponseWriter, r *http.Request) {
	var keyID *int64
	var since *time.Time

	if v := r.URL.Query().Get("key_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			proxy.WriteError(w, r, http.StatusBadRequest, "invalid_key_id", "invalid_request_error", "Invalid key_id parameter")
			return
		}
		keyID = &id
	}

	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			// Try date-only format
			t, err = time.Parse("2006-01-02", v)
			if err != nil {
				proxy.WriteError(w, r, http.StatusBadRequest, "invalid_since", "invalid_request_error", "Invalid since parameter (use RFC3339 or YYYY-MM-DD)")
				return
			}
		}
		since = &t
	}

	stats, err := h.store.GetUsageStats(keyID, since)
	if err != nil {
		slog.ErrorContext(r.Context(), "usage stats error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage stats")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers()
	if err != nil {
		slog.ErrorContext(r.Context(), "list users error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list users")
		return
	}

	type userResponse struct {
		ID        int64  `json:"id"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		Role      string `json:"role"`
		IsActive  bool   `json:"is_active"`
		CreatedAt string `json:"created_at"`
	}

	resp := make([]userResponse, len(users))
	for i, u := range users {
		resp[i] = userResponse{
			ID:        u.ID,
			Email:     u.Email,
			Name:      u.Name,
			Role:      u.Role,
			IsActive:  u.IsActive,
			CreatedAt: u.CreatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) activateUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid user ID")
		return
	}

	// Activation is monotonic (can only add admins) and doesn't need the guardrail.
	if err := h.store.SetUserActive(id, true); err != nil {
		if err.Error() == "user not found" {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "User not found")
			return
		}
		slog.ErrorContext(r.Context(), "activate user error", "error", err, "user_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update user")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "activated"})
}

func (h *handler) deactivateUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid user ID")
		return
	}

	if err := h.store.DeactivateUserGuarded(id); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "User not found")
			return
		}
		if errors.Is(err, store.ErrLastActiveAdmin) {
			proxy.WriteError(w, r, http.StatusConflict, "last_admin", "invalid_request_error", "Cannot remove the last active admin")
			return
		}
		slog.ErrorContext(r.Context(), "deactivate user error", "error", err, "user_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update user")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deactivated"})
}

// --- Credit management ---

func (h *handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.store.ListAccountsWithBalances()
	if err != nil {
		slog.ErrorContext(r.Context(), "list accounts error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list accounts")
		return
	}

	type accountResponse struct {
		ID        int64   `json:"id"`
		Name      string  `json:"name"`
		Type      string  `json:"type"`
		IsActive  bool    `json:"is_active"`
		Balance   float64 `json:"balance"`
		Reserved  float64 `json:"reserved"`
		Available float64 `json:"available"`
		CreatedAt string  `json:"created_at"`
	}

	resp := make([]accountResponse, len(accounts))
	for i, a := range accounts {
		resp[i] = accountResponse{
			ID:        a.ID,
			Name:      a.Name,
			Type:      a.Type,
			IsActive:  a.IsActive,
			Balance:   a.Balance,
			Reserved:  a.Reserved,
			Available: a.Balance - a.Reserved,
			CreatedAt: a.CreatedAt.Format(time.RFC3339),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) grantCredits(w http.ResponseWriter, r *http.Request) {
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid account ID")
		return
	}

	var req struct {
		Amount      float64 `json:"amount"`
		Description string  `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Amount == 0 {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_amount", "invalid_request_error", "amount must be non-zero")
		return
	}

	if err := h.store.AddCredits(accountID, req.Amount, req.Description); err != nil {
		slog.ErrorContext(r.Context(), "grant credits error", "error", err, "account_id", accountID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to grant credits")
		return
	}

	bal, _ := h.store.GetCreditBalance(accountID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "granted",
		"amount":  req.Amount,
		"balance": bal.Balance,
	})
}

func (h *handler) createAccountKey(w http.ResponseWriter, r *http.Request) {
	accountID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid account ID")
		return
	}

	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Name == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_name", "invalid_request_error", "name is required")
		return
	}
	rateLimit, err := ratelimit.ApplyConfigDefaultsAndCap(req.RateLimit)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "rate_limit_too_high", "invalid_request_error", "rate_limit must be <= 10000")
		return
	}
	req.RateLimit = rateLimit

	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		slog.ErrorContext(r.Context(), "crypto/rand error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate key")
		return
	}
	rawKey := "sk-" + hex.EncodeToString(rawBytes)
	keyPrefix := rawKey[:11]
	keyHash := auth.HashKey(rawKey)

	id, err := h.store.CreateKeyForAccountOnly(accountID, req.Name, keyHash, keyPrefix, req.RateLimit)
	if err != nil {
		slog.ErrorContext(r.Context(), "create account key error", "error", err, "account_id", accountID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(createKeyResponse{
		ID:        id,
		Name:      req.Name,
		Key:       rawKey,
		KeyPrefix: keyPrefix,
		RateLimit: req.RateLimit,
	})
}

// --- Registration tokens ---

func (h *handler) createRegistrationToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string  `json:"name"`
		CreditGrant float64 `json:"credit_grant"`
		MaxUses     int     `json:"max_uses"`
		ExpiresAt   *string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Name == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_name", "invalid_request_error", "name is required")
		return
	}
	if req.MaxUses <= 0 {
		req.MaxUses = 1
	}

	// Generate token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.ErrorContext(r.Context(), "crypto/rand error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate token")
		return
	}
	rawToken := "reg-" + hex.EncodeToString(tokenBytes)
	tokenHash := auth.HashKey(rawToken)

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			proxy.WriteError(w, r, http.StatusBadRequest, "invalid_expires_at", "invalid_request_error", "expires_at must be RFC3339")
			return
		}
		expiresAt = &t
	}

	id, err := h.store.CreateRegistrationToken(req.Name, tokenHash, req.CreditGrant, req.MaxUses, expiresAt)
	if err != nil {
		slog.ErrorContext(r.Context(), "create registration token error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create token")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"id":           id,
		"name":         req.Name,
		"token":        rawToken,
		"credit_grant": req.CreditGrant,
		"max_uses":     req.MaxUses,
	})
}

func (h *handler) listRegistrationTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := h.store.ListRegistrationTokens()
	if err != nil {
		slog.ErrorContext(r.Context(), "list registration tokens error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list tokens")
		return
	}

	type tokenResponse struct {
		ID          int64   `json:"id"`
		Name        string  `json:"name"`
		CreditGrant float64 `json:"credit_grant"`
		MaxUses     int     `json:"max_uses"`
		Uses        int     `json:"uses"`
		CreatedAt   string  `json:"created_at"`
		ExpiresAt   *string `json:"expires_at"`
		Revoked     bool    `json:"revoked"`
	}

	resp := make([]tokenResponse, len(tokens))
	for i, t := range tokens {
		tr := tokenResponse{
			ID:          t.ID,
			Name:        t.Name,
			CreditGrant: t.CreditGrant,
			MaxUses:     t.MaxUses,
			Uses:        t.Uses,
			CreatedAt:   t.CreatedAt.Format(time.RFC3339),
			Revoked:     t.Revoked,
		}
		if t.ExpiresAt != nil {
			s := t.ExpiresAt.Format(time.RFC3339)
			tr.ExpiresAt = &s
		}
		resp[i] = tr
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) revokeRegistrationToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid token ID")
		return
	}

	if err := h.store.RevokeRegistrationToken(id); err != nil {
		if err.Error() == "registration token not found" {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Token not found")
			return
		}
		slog.ErrorContext(r.Context(), "revoke registration token error", "error", err, "token_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to revoke token")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

// --- Pricing ---

func (h *handler) listPricing(w http.ResponseWriter, r *http.Request) {
	pricing, err := h.store.ListActivePricing()
	if err != nil {
		slog.ErrorContext(r.Context(), "list pricing error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list pricing")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pricing)
}

func (h *handler) upsertPricing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ModelID           string  `json:"model_id"`
		PromptRate        float64 `json:"prompt_rate"`
		CompletionRate    float64 `json:"completion_rate"`
		TypicalCompletion int     `json:"typical_completion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.ModelID == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_model_id", "invalid_request_error", "model_id is required")
		return
	}
	if req.PromptRate <= 0 || req.CompletionRate <= 0 {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_rates", "invalid_request_error", "prompt_rate and completion_rate must be positive")
		return
	}
	if req.TypicalCompletion <= 0 {
		req.TypicalCompletion = 500
	}

	if err := h.store.UpsertPricing(req.ModelID, req.PromptRate, req.CompletionRate, req.TypicalCompletion); err != nil {
		slog.ErrorContext(r.Context(), "upsert pricing error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update pricing")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

func (h *handler) deletePricing(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid pricing ID")
		return
	}

	if err := h.store.DeletePricing(id); err != nil {
		if err.Error() == "pricing not found" {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Pricing not found")
			return
		}
		slog.ErrorContext(r.Context(), "delete pricing error", "error", err, "pricing_id", id)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to delete pricing")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// --- Session limits ---

func (h *handler) setSessionLimit(w http.ResponseWriter, r *http.Request) {
	keyID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid key ID")
		return
	}

	var req struct {
		Limit *int `json:"limit"` // null = remove limit
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}

	if err := h.store.SetSessionTokenLimit(keyID, req.Limit); err != nil {
		if err.Error() == "key not found" {
			proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Key not found")
			return
		}
		slog.ErrorContext(r.Context(), "set session limit error", "error", err, "key_id", keyID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to set session limit")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "updated", "limit": req.Limit})
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	store    *store.Store
	adminKey string
	usageCh  chan<- store.UsageEntry

	// Admin rate limiter: 10 req/min, single bucket
	rateLimitMu       sync.Mutex
	rateLimitTokens   float64
	rateLimitLastTime time.Time
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/keys", handler.createKey)
	mux.HandleFunc("GET /admin/keys", handler.listKeys)
	mux.HandleFunc("DELETE /admin/keys/{id}", handler.revokeKey)
	mux.HandleFunc("GET /admin/usage", handler.getUsage)
	mux.HandleFunc("GET /admin/users", handler.listUsers)
	mux.HandleFunc("PUT /admin/users/{id}/activate", handler.activateUser)
	mux.HandleFunc("PUT /admin/users/{id}/deactivate", handler.deactivateUser)

	return handler.authMiddleware(mux)
}

func (h *handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Admin-Key")
		if provided == "" {
			proxy.WriteError(w, http.StatusUnauthorized, "missing_admin_key", "invalid_api_key", "Missing X-Admin-Key header")
			return
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(h.adminKey)) != 1 {
			proxy.WriteError(w, http.StatusUnauthorized, "invalid_admin_key", "invalid_api_key", "Invalid admin key")
			return
		}

		// Admin rate limiting: 10 req/min
		if !h.adminAllow() {
			w.Header().Set("Retry-After", "6")
			proxy.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded", "Admin rate limit exceeded")
			return
		}

		next.ServeHTTP(w, r)
	})
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
		proxy.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Name == "" {
		proxy.WriteError(w, http.StatusBadRequest, "missing_name", "invalid_request_error", "name is required")
		return
	}
	if req.RateLimit <= 0 {
		req.RateLimit = 60
	}

	// Generate random key
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		log.Printf("crypto/rand error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate key")
		return
	}
	rawKey := "sk-" + hex.EncodeToString(rawBytes)
	keyPrefix := rawKey[:11] // "sk-" + first 8 hex chars
	keyHash := auth.HashKey(rawKey)

	id, err := h.store.CreateKey(req.Name, keyHash, keyPrefix, req.RateLimit)
	if err != nil {
		log.Printf("create key error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create key")
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
		log.Printf("list keys error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list keys")
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
		proxy.WriteError(w, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid key ID")
		return
	}

	if err := h.store.RevokeKey(id); err != nil {
		if err.Error() == "key not found" {
			proxy.WriteError(w, http.StatusNotFound, "not_found", "invalid_request_error", "Key not found")
			return
		}
		log.Printf("revoke key error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to revoke key")
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
			proxy.WriteError(w, http.StatusBadRequest, "invalid_key_id", "invalid_request_error", "Invalid key_id parameter")
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
				proxy.WriteError(w, http.StatusBadRequest, "invalid_since", "invalid_request_error", "Invalid since parameter (use RFC3339 or YYYY-MM-DD)")
				return
			}
		}
		since = &t
	}

	stats, err := h.store.GetUsageStats(keyID, since)
	if err != nil {
		log.Printf("usage stats error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage stats")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers()
	if err != nil {
		log.Printf("list users error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list users")
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
	h.setUserActiveHandler(w, r, true)
}

func (h *handler) deactivateUser(w http.ResponseWriter, r *http.Request) {
	h.setUserActiveHandler(w, r, false)
}

func (h *handler) setUserActiveHandler(w http.ResponseWriter, r *http.Request, active bool) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid user ID")
		return
	}

	if err := h.store.SetUserActive(id, active); err != nil {
		if err.Error() == "user not found" {
			proxy.WriteError(w, http.StatusNotFound, "not_found", "invalid_request_error", "User not found")
			return
		}
		log.Printf("set user active error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update user")
		return
	}

	status := "activated"
	if !active {
		status = "deactivated"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

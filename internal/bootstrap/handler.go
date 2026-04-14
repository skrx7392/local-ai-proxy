// Package bootstrap serves the one-door admin creation endpoint used both
// to seed the very first admin and to recover from total lockout.
//
// Unlike the rest of /api/admin, this handler is mounted OUTSIDE the admin
// authMiddleware chain — it is its own authentication path, gated by a
// pre-shared ADMIN_BOOTSTRAP_TOKEN environment variable.
package bootstrap

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// RateLimitPerMinute is the shared bucket size for the bootstrap endpoint.
// Low by design: this endpoint is called by operators during disaster
// recovery, not by automation, so a tight ceiling makes bruteforce
// attempts against a still-set ADMIN_BOOTSTRAP_TOKEN impractical.
const RateLimitPerMinute = 5

type handler struct {
	store *store.Store
	token string

	bucketMu   sync.Mutex
	tokens     float64
	lastRefill time.Time
}

type bootstrapRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type bootstrapResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

// New returns an http.Handler that serves POST /api/admin/bootstrap.
//
// When adminBootstrapToken is empty the handler returns 404 for every
// request — the operator rotates the env var in to enable the endpoint
// and rotates it out again when done.
func New(dataStore *store.Store, adminBootstrapToken string) http.Handler {
	return &handler{
		store:      dataStore,
		token:      adminBootstrapToken,
		tokens:     RateLimitPerMinute,
		lastRefill: time.Now(),
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Endpoint is disabled unless ADMIN_BOOTSTRAP_TOKEN is set.
	// Return 404 rather than 401 so it's indistinguishable from a
	// missing route when the feature isn't in use.
	if h.token == "" {
		apierror.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Not found")
		return
	}

	if !h.allow() {
		w.Header().Set("Retry-After", "12")
		apierror.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded", "Bootstrap rate limit exceeded")
		slog.InfoContext(r.Context(), "admin bootstrap attempt", "outcome", "rate_limited")
		return
	}

	var req bootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.WriteError(w, r, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}

	if req.Token == "" || req.Email == "" || req.Password == "" || req.Name == "" {
		apierror.WriteError(w, r, http.StatusBadRequest, "missing_fields", "invalid_request_error", "token, email, password, and name are required")
		return
	}
	if len(req.Password) < 8 {
		apierror.WriteError(w, r, http.StatusBadRequest, "weak_password", "invalid_request_error", "Password must be at least 8 characters")
		return
	}

	if subtle.ConstantTimeCompare([]byte(req.Token), []byte(h.token)) != 1 {
		apierror.WriteError(w, r, http.StatusUnauthorized, "invalid_bootstrap_token", "invalid_api_key", "Invalid bootstrap token")
		slog.InfoContext(r.Context(), "admin bootstrap attempt", "outcome", "invalid_token", "email", req.Email)
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		slog.ErrorContext(r.Context(), "bcrypt error", "error", err)
		apierror.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to hash password")
		return
	}

	userID, err := h.store.CreateAdminBootstrap(req.Email, string(hashed), req.Name)
	if err != nil {
		if errors.Is(err, store.ErrEmailExists) {
			apierror.WriteError(w, r, http.StatusConflict, "email_exists", "invalid_request_error", "Email already registered")
			slog.InfoContext(r.Context(), "admin bootstrap attempt", "outcome", "email_exists", "email", req.Email)
			return
		}
		slog.ErrorContext(r.Context(), "admin bootstrap error", "error", err)
		apierror.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create admin")
		return
	}

	slog.InfoContext(r.Context(), "admin bootstrap attempt", "outcome", "success", "email", req.Email, "user_id", userID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(bootstrapResponse{ID: userID, Email: req.Email})
}

func (h *handler) allow() bool {
	h.bucketMu.Lock()
	defer h.bucketMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(h.lastRefill).Seconds()
	h.tokens = math.Min(float64(RateLimitPerMinute), h.tokens+elapsed*(float64(RateLimitPerMinute)/60.0))
	h.lastRefill = now

	if h.tokens >= 1 {
		h.tokens--
		return true
	}
	return false
}

package user

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/authlimit"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/ratelimit"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	store              *store.Store
	defaultCreditGrant float64
	metrics            *metrics.Metrics
	guard              *authlimit.Guard
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginUser struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type loginResponse struct {
	Token     string     `json:"token"`
	ExpiresIn int        `json:"expires_in"` // seconds
	User      *loginUser `json:"user,omitempty"`
}

type profileResponse struct {
	ID        int64  `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

type updateProfileRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
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
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
	RateLimit int    `json:"rate_limit"`
	CreatedAt string `json:"created_at"`
	Revoked   bool   `json:"revoked"`
}

// NewHandler builds the user-facing API handler. guard throttles the
// bcrypt-heavy endpoints (per-email login attempts, global bcrypt
// concurrency); nil disables both limits.
func NewHandler(dataStore *store.Store, defaultCreditGrant float64, m *metrics.Metrics, guard *authlimit.Guard) http.Handler {
	h := &handler{store: dataStore, defaultCreditGrant: defaultCreditGrant, metrics: m, guard: guard}

	mux := http.NewServeMux()

	// Public auth routes (no session required)
	mux.HandleFunc("POST /api/auth/register", h.register)
	mux.HandleFunc("POST /api/auth/login", h.login)

	// Service account registration (public, token-gated)
	mux.HandleFunc("POST /api/accounts/register", h.registerServiceAccount)

	// Session-authenticated routes
	sessionAuth := SessionMiddleware(dataStore)
	mux.Handle("POST /api/auth/logout", sessionAuth(http.HandlerFunc(h.logout)))
	mux.Handle("GET /api/users/profile", sessionAuth(http.HandlerFunc(h.getProfile)))
	mux.Handle("PUT /api/users/profile", sessionAuth(http.HandlerFunc(h.updateProfile)))
	mux.Handle("PUT /api/users/password", sessionAuth(http.HandlerFunc(h.changePassword)))
	mux.Handle("POST /api/users/keys", sessionAuth(http.HandlerFunc(h.createKey)))
	mux.Handle("GET /api/users/keys", sessionAuth(http.HandlerFunc(h.listKeys)))
	mux.Handle("DELETE /api/users/keys/{id}", sessionAuth(http.HandlerFunc(h.revokeKey)))
	mux.Handle("GET /api/users/usage", sessionAuth(http.HandlerFunc(h.getUsage)))
	mux.Handle("GET /api/users/credits", sessionAuth(http.HandlerFunc(h.getCredits)))
	mux.Handle("GET /api/users/credits/transactions", sessionAuth(http.HandlerFunc(h.getCreditTransactions)))

	return mux
}

// writeThrottled emits the standard 429 envelope with a Retry-After header
// and bumps the rate-limit reject metric.
func (h *handler) writeThrottled(w http.ResponseWriter, r *http.Request, retryAfter float64, message string) {
	h.metrics.RecordRateLimitReject()
	w.Header().Set("Retry-After", fmt.Sprintf("%.0f", math.Ceil(retryAfter)))
	proxy.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded", message)
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" || req.Name == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_fields", "invalid_request_error", "email, password, and name are required")
		return
	}
	if len(req.Password) < 8 {
		proxy.WriteError(w, r, http.StatusBadRequest, "weak_password", "invalid_request_error", "Password must be at least 8 characters")
		return
	}

	if !h.guard.TryAcquireBcrypt() {
		h.writeThrottled(w, r, 1, "Server is busy, retry shortly")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	h.guard.ReleaseBcrypt()
	if err != nil {
		slog.ErrorContext(r.Context(), "bcrypt error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to hash password")
		return
	}

	accountID, userID, err := h.store.RegisterUser(req.Email, string(hash), req.Name)
	if err != nil {
		// Check for duplicate email (unique constraint violation)
		proxy.WriteError(w, r, http.StatusConflict, "email_exists", "invalid_request_error", "Email already registered")
		return
	}
	h.metrics.RecordRegistration("user_signup")

	// Grant default credits if configured
	if h.defaultCreditGrant > 0 {
		if err := h.store.AddCredits(accountID, h.defaultCreditGrant, "registration bonus"); err != nil {
			slog.ErrorContext(r.Context(), "grant default credits error", "error", err, "account_id", accountID)
			// Don't fail registration for this — account is already created
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": userID, "account_id": accountID, "email": req.Email, "name": req.Name})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_fields", "invalid_request_error", "email and password are required")
		return
	}

	// Per-email throttle: IP rotation cannot keep hammering one account.
	if ok, retryAfter := h.guard.AllowLoginEmail(req.Email); !ok {
		h.writeThrottled(w, r, retryAfter, "Too many login attempts for this account, retry later")
		return
	}

	user, err := h.store.GetUserByEmail(req.Email)
	if err != nil {
		slog.ErrorContext(r.Context(), "get user error", "error", err, "email", req.Email)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")
		return
	}
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid_request_error", "Invalid email or password")
		return
	}
	if !user.IsActive {
		proxy.WriteError(w, r, http.StatusForbidden, "account_disabled", "invalid_request_error", "Account is disabled")
		return
	}

	if !h.guard.TryAcquireBcrypt() {
		h.writeThrottled(w, r, 1, "Server is busy, retry shortly")
		return
	}
	compareErr := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password))
	h.guard.ReleaseBcrypt()
	if compareErr != nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "invalid_credentials", "invalid_request_error", "Invalid email or password")
		return
	}

	// Generate session token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.ErrorContext(r.Context(), "crypto/rand error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate session")
		return
	}
	rawToken := hex.EncodeToString(tokenBytes)
	tokenHash := auth.HashKey(rawToken)

	duration := SessionDurationFor(user.Role)
	expiresAt := time.Now().Add(duration)
	if err := h.store.CreateSession(user.ID, tokenHash, expiresAt); err != nil {
		slog.ErrorContext(r.Context(), "create session error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create session")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(loginResponse{
		Token:     rawToken,
		ExpiresIn: int(duration.Seconds()),
		User: &loginUser{
			ID:    user.ID,
			Email: user.Email,
			Role:  user.Role,
		},
	})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	session := SessionFromContext(r.Context())
	if session == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_session", "invalid_request_error", "No active session")
		return
	}

	if err := h.store.DeleteSession(session.TokenHash); err != nil {
		slog.ErrorContext(r.Context(), "delete session error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to logout")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
}

func (h *handler) getProfile(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(profileResponse{
		ID:        user.ID,
		Email:     user.Email,
		Name:      user.Name,
		Role:      user.Role,
		IsActive:  user.IsActive,
		CreatedAt: user.CreatedAt.Format("2006-01-02T15:04:05Z"),
	})
}

func (h *handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	var req updateProfileRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}

	// Use current values as defaults
	name := req.Name
	email := req.Email
	if name == "" {
		name = user.Name
	}
	if email == "" {
		email = user.Email
	}

	if err := h.store.UpdateUserProfile(user.ID, name, email); err != nil {
		slog.ErrorContext(r.Context(), "update profile error", "error", err, "user_id", user.ID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update profile")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	var req changePasswordRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}
	if req.OldPassword == "" || req.NewPassword == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_fields", "invalid_request_error", "old_password and new_password are required")
		return
	}
	if len(req.NewPassword) < 8 {
		proxy.WriteError(w, r, http.StatusBadRequest, "weak_password", "invalid_request_error", "New password must be at least 8 characters")
		return
	}

	// One slot covers both bcrypt operations; there is no I/O between them.
	if !h.guard.TryAcquireBcrypt() {
		h.writeThrottled(w, r, 1, "Server is busy, retry shortly")
		return
	}
	compareErr := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.OldPassword))
	if compareErr != nil {
		h.guard.ReleaseBcrypt()
		proxy.WriteError(w, r, http.StatusUnauthorized, "wrong_password", "invalid_request_error", "Current password is incorrect")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	h.guard.ReleaseBcrypt()
	if err != nil {
		slog.ErrorContext(r.Context(), "bcrypt error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to hash password")
		return
	}

	if err := h.store.UpdateUserPassword(user.ID, string(hash)); err != nil {
		slog.ErrorContext(r.Context(), "update password error", "error", err, "user_id", user.ID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update password")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "password_changed"})
}

func (h *handler) createKey(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	var req createKeyRequest
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_name", "invalid_request_error", "name is required")
		return
	}
	rl, err := ratelimit.ApplyConfigDefaultsAndCap(req.RateLimit)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "rate_limit_too_high", "invalid_request_error", "rate_limit must be <= 10000")
		return
	}
	req.RateLimit = rl

	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		slog.ErrorContext(r.Context(), "crypto/rand error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate key")
		return
	}
	rawKey := "sk-" + hex.EncodeToString(rawBytes)
	keyPrefix := rawKey[:11]
	keyHash := auth.HashKey(rawKey)

	var (
		id     int64
		keyErr error
	)
	if user.AccountID != nil {
		id, keyErr = h.store.CreateKeyForAccount(user.ID, *user.AccountID, req.Name, keyHash, keyPrefix, req.RateLimit)
	} else {
		id, keyErr = h.store.CreateKeyForUser(user.ID, req.Name, keyHash, keyPrefix, req.RateLimit)
	}
	if keyErr != nil {
		slog.ErrorContext(r.Context(), "create key error", "error", keyErr, "user_id", user.ID)
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
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	keys, err := h.store.ListKeysByUser(user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "list keys error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list keys")
		return
	}

	resp := make([]keyResponse, len(keys))
	for i, k := range keys {
		resp[i] = keyResponse{
			ID:        k.ID,
			Name:      k.Name,
			KeyPrefix: k.KeyPrefix,
			RateLimit: k.RateLimit,
			CreatedAt: k.CreatedAt.Format("2006-01-02T15:04:05Z"),
			Revoked:   k.Revoked,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	idStr := r.PathValue("id")
	keyID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid key ID")
		return
	}

	// Verify the key belongs to this user
	keys, err := h.store.ListKeysByUser(user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "list keys error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to verify key ownership")
		return
	}

	found := false
	for _, k := range keys {
		if k.ID == keyID {
			found = true
			break
		}
	}
	if !found {
		proxy.WriteError(w, r, http.StatusNotFound, "not_found", "invalid_request_error", "Key not found")
		return
	}

	if err := h.store.RevokeKey(keyID); err != nil {
		slog.ErrorContext(r.Context(), "revoke key error", "error", err, "key_id", keyID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to revoke key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

func (h *handler) getUsage(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	// Get all keys for this user, then get usage for each
	keys, err := h.store.ListKeysByUser(user.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "list keys error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get keys")
		return
	}

	var allStats []store.UsageStat
	for _, k := range keys {
		keyID := k.ID
		stats, err := h.store.GetUsageStats(&keyID, nil)
		if err != nil {
			slog.ErrorContext(r.Context(), "usage stats error", "error", err)
			proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage stats")
			return
		}
		allStats = append(allStats, stats...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(allStats)
}

func (h *handler) getCredits(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}
	if user.AccountID == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "no_account", "invalid_request_error", "No account associated with this user")
		return
	}

	bal, err := h.store.GetCreditBalance(*user.AccountID)
	if err != nil {
		slog.ErrorContext(r.Context(), "get credit balance error", "error", err, "account_id", *user.AccountID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get credit balance")
		return
	}
	if bal == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "no_balance", "invalid_request_error", "No credit balance found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"balance":   bal.Balance,
		"reserved":  bal.Reserved,
		"available": bal.Balance - bal.Reserved,
	})
}

func (h *handler) getCreditTransactions(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, r, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}
	if user.AccountID == nil {
		proxy.WriteError(w, r, http.StatusNotFound, "no_account", "invalid_request_error", "No account associated with this user")
		return
	}

	limit := 20
	offset := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if o, err := strconv.Atoi(v); err == nil && o >= 0 {
			offset = o
		}
	}

	txns, err := h.store.GetCreditTransactions(*user.AccountID, limit, offset)
	if err != nil {
		slog.ErrorContext(r.Context(), "get credit transactions error", "error", err, "account_id", *user.AccountID)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get transactions")
		return
	}

	type txnResponse struct {
		ID           int64   `json:"id"`
		Amount       float64 `json:"amount"`
		BalanceAfter float64 `json:"balance_after"`
		Type         string  `json:"type"`
		Description  *string `json:"description"`
		CreatedAt    string  `json:"created_at"`
	}

	resp := make([]txnResponse, len(txns))
	for i, t := range txns {
		resp[i] = txnResponse{
			ID:           t.ID,
			Amount:       t.Amount,
			BalanceAfter: t.BalanceAfter,
			Type:         t.Type,
			Description:  t.Description,
			CreatedAt:    t.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *handler) registerServiceAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token     string `json:"registration_token"`
		Name      string `json:"name"`
		RateLimit int    `json:"rate_limit"`
	}
	if !apierror.DecodeJSON(w, r, &req) {
		return
	}
	if req.Token == "" || req.Name == "" {
		proxy.WriteError(w, r, http.StatusBadRequest, "missing_fields", "invalid_request_error", "registration_token and name are required")
		return
	}
	rl, err := ratelimit.ApplyConfigDefaultsAndCap(req.RateLimit)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "rate_limit_too_high", "invalid_request_error", "rate_limit must be <= 10000")
		return
	}
	req.RateLimit = rl

	// Generate API key
	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		slog.ErrorContext(r.Context(), "crypto/rand error", "error", err)
		proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate key")
		return
	}
	rawKey := "sk-" + hex.EncodeToString(rawBytes)
	keyPrefix := rawKey[:11]
	keyHash := auth.HashKey(rawKey)
	tokenHash := auth.HashKey(req.Token)

	accountID, keyID, creditGrant, err := h.store.RegisterServiceAccount(
		tokenHash, req.Name, keyHash, keyPrefix, req.RateLimit)
	if err != nil {
		proxy.WriteError(w, r, http.StatusBadRequest, "registration_failed", "invalid_request_error", err.Error())
		return
	}
	h.metrics.RecordRegistration("service_registration")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"account_id": accountID,
		"key_id":     keyID,
		"api_key":    rawKey,
		"key_prefix": keyPrefix,
		"credits":    creditGrant,
	})
}

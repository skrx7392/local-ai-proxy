package user

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"golang.org/x/crypto/bcrypt"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/store"
)

type handler struct {
	store *store.Store
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

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"` // seconds
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

func NewHandler(dataStore *store.Store) http.Handler {
	h := &handler{store: dataStore}

	mux := http.NewServeMux()

	// Public auth routes (no session required)
	mux.HandleFunc("POST /api/auth/register", h.register)
	mux.HandleFunc("POST /api/auth/login", h.login)

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

	return mux
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Email == "" || req.Password == "" || req.Name == "" {
		proxy.WriteError(w, http.StatusBadRequest, "missing_fields", "invalid_request_error", "email, password, and name are required")
		return
	}
	if len(req.Password) < 8 {
		proxy.WriteError(w, http.StatusBadRequest, "weak_password", "invalid_request_error", "Password must be at least 8 characters")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("bcrypt error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to hash password")
		return
	}

	accountID, userID, err := h.store.RegisterUser(req.Email, string(hash), req.Name)
	if err != nil {
		// Check for duplicate email (unique constraint violation)
		proxy.WriteError(w, http.StatusConflict, "email_exists", "invalid_request_error", "Email already registered")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"id": userID, "account_id": accountID, "email": req.Email, "name": req.Name})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.Email == "" || req.Password == "" {
		proxy.WriteError(w, http.StatusBadRequest, "missing_fields", "invalid_request_error", "email and password are required")
		return
	}

	user, err := h.store.GetUserByEmail(req.Email)
	if err != nil {
		log.Printf("get user error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")
		return
	}
	if user == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid_request_error", "Invalid email or password")
		return
	}
	if !user.IsActive {
		proxy.WriteError(w, http.StatusForbidden, "account_disabled", "invalid_request_error", "Account is disabled")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		proxy.WriteError(w, http.StatusUnauthorized, "invalid_credentials", "invalid_request_error", "Invalid email or password")
		return
	}

	// Generate session token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Printf("crypto/rand error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate session")
		return
	}
	rawToken := hex.EncodeToString(tokenBytes)
	tokenHash := auth.HashKey(rawToken)

	expiresAt := sessionExpiry()
	if err := h.store.CreateSession(user.ID, tokenHash, expiresAt); err != nil {
		log.Printf("create session error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to create session")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(loginResponse{
		Token:     rawToken,
		ExpiresIn: int(sessionDuration.Seconds()),
	})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	session := SessionFromContext(r.Context())
	if session == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "no_session", "invalid_request_error", "No active session")
		return
	}

	if err := h.store.DeleteSession(session.TokenHash); err != nil {
		log.Printf("delete session error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to logout")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
}

func (h *handler) getProfile(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
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
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	var req updateProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
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
		log.Printf("update profile error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update profile")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_json", "invalid_request_error", "Invalid JSON body")
		return
	}
	if req.OldPassword == "" || req.NewPassword == "" {
		proxy.WriteError(w, http.StatusBadRequest, "missing_fields", "invalid_request_error", "old_password and new_password are required")
		return
	}
	if len(req.NewPassword) < 8 {
		proxy.WriteError(w, http.StatusBadRequest, "weak_password", "invalid_request_error", "New password must be at least 8 characters")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.OldPassword)); err != nil {
		proxy.WriteError(w, http.StatusUnauthorized, "wrong_password", "invalid_request_error", "Current password is incorrect")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("bcrypt error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to hash password")
		return
	}

	if err := h.store.UpdateUserPassword(user.ID, string(hash)); err != nil {
		log.Printf("update password error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to update password")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "password_changed"})
}

func (h *handler) createKey(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

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

	rawBytes := make([]byte, 32)
	if _, err := rand.Read(rawBytes); err != nil {
		log.Printf("crypto/rand error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to generate key")
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
		log.Printf("create key error: %v", keyErr)
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
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	keys, err := h.store.ListKeysByUser(user.ID)
	if err != nil {
		log.Printf("list keys error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to list keys")
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
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	idStr := r.PathValue("id")
	keyID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		proxy.WriteError(w, http.StatusBadRequest, "invalid_id", "invalid_request_error", "Invalid key ID")
		return
	}

	// Verify the key belongs to this user
	keys, err := h.store.ListKeysByUser(user.ID)
	if err != nil {
		log.Printf("list keys error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to verify key ownership")
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
		proxy.WriteError(w, http.StatusNotFound, "not_found", "invalid_request_error", "Key not found")
		return
	}

	if err := h.store.RevokeKey(keyID); err != nil {
		log.Printf("revoke key error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to revoke key")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

func (h *handler) getUsage(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		proxy.WriteError(w, http.StatusUnauthorized, "no_user", "invalid_request_error", "Not authenticated")
		return
	}

	// Get all keys for this user, then get usage for each
	keys, err := h.store.ListKeysByUser(user.ID)
	if err != nil {
		log.Printf("list keys error: %v", err)
		proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get keys")
		return
	}

	var allStats []store.UsageStat
	for _, k := range keys {
		keyID := k.ID
		stats, err := h.store.GetUsageStats(&keyID, nil)
		if err != nil {
			log.Printf("usage stats error: %v", err)
			proxy.WriteError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to get usage stats")
			return
		}
		allStats = append(allStats, stats...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(allStats)
}

package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/krishna/local-ai-proxy/internal/store"
)

type contextKey struct{}

// Middleware returns HTTP middleware that validates Bearer tokens against the store.
func Middleware(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearer(r)
			if token == "" {
				writeAuthError(w, "Missing API key in Authorization header")
				return
			}

			hash := hashKey(token)
			key, err := db.GetKeyByHash(hash)
			if err != nil {
				writeAuthError(w, "Internal authentication error")
				return
			}
			if key == nil {
				writeAuthError(w, "Invalid API key")
				return
			}

			ctx := context.WithValue(r.Context(), contextKey{}, key)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// KeyFromContext retrieves the authenticated API key from the request context.
func KeyFromContext(ctx context.Context) *store.APIKey {
	key, _ := ctx.Value(contextKey{}).(*store.APIKey)
	return key
}

// WithKey returns a new context that carries the given API key.
// This is intended for testing and internal use.
func WithKey(ctx context.Context, key *store.APIKey) context.Context {
	return context.WithValue(ctx, contextKey{}, key)
}

// HashKey computes the SHA-256 hash of a raw API key. Exported for use by admin.
func HashKey(raw string) string {
	return hashKey(raw)
}

func hashKey(raw string) string {
	hashBytes := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hashBytes[:])
}

func extractBearer(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(authHeader, "Bearer ")
}

func writeAuthError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":{"message":"` + message + `","type":"invalid_api_key","code":"invalid_api_key"}}`))
}

package user

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/proxy"
	"github.com/krishna/local-ai-proxy/internal/store"
)

const (
	UserSessionDuration  = 7 * 24 * time.Hour
	AdminSessionDuration = 6 * time.Hour
)

// sessionDuration is retained for existing callers that don't know the role.
// New code should prefer SessionDurationFor(role).
const sessionDuration = UserSessionDuration

type userContextKey struct{}
type sessionContextKey struct{}

// SessionDurationFor returns how long a newly minted session should live based
// on the user's role. Admin sessions expire faster to limit blast radius.
func SessionDurationFor(role string) time.Duration {
	if role == "admin" {
		return AdminSessionDuration
	}
	return UserSessionDuration
}

func sessionExpiry() time.Time {
	return time.Now().Add(sessionDuration)
}

// SessionExpiryFor returns the absolute expiry time for a newly minted session
// given the user's role.
func SessionExpiryFor(role string) time.Time {
	return time.Now().Add(SessionDurationFor(role))
}

// SessionMiddleware returns HTTP middleware that validates session tokens.
func SessionMiddleware(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractSessionToken(r)
			if token == "" {
				proxy.WriteError(w, r, http.StatusUnauthorized, "missing_session", "invalid_request_error", "Missing session token")
				return
			}

			tokenHash := auth.HashKey(token)
			session, err := db.GetSessionByTokenHash(tokenHash)
			if err != nil {
				proxy.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Internal error")
				return
			}
			if session == nil {
				proxy.WriteError(w, r, http.StatusUnauthorized, "invalid_session", "invalid_request_error", "Invalid or expired session")
				return
			}

			// Check expiry
			if time.Now().After(session.ExpiresAt) {
				// Clean up expired session
				if err := db.DeleteSession(tokenHash); err != nil {
					slog.WarnContext(r.Context(), "failed to delete expired session", "error", err)
				}
				proxy.WriteError(w, r, http.StatusUnauthorized, "session_expired", "invalid_request_error", "Session has expired")
				return
			}

			// Load user
			user, err := db.GetUserByID(session.UserID)
			if err != nil || user == nil {
				proxy.WriteError(w, r, http.StatusUnauthorized, "user_not_found", "invalid_request_error", "User not found")
				return
			}
			if !user.IsActive {
				proxy.WriteError(w, r, http.StatusForbidden, "account_disabled", "invalid_request_error", "Account is disabled")
				return
			}

			ctx := context.WithValue(r.Context(), userContextKey{}, user)
			ctx = context.WithValue(ctx, sessionContextKey{}, session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserFromContext retrieves the authenticated user from the request context.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(userContextKey{}).(*store.User)
	return u
}

// SessionFromContext retrieves the session from the request context.
func SessionFromContext(ctx context.Context) *store.Session {
	s, _ := ctx.Value(sessionContextKey{}).(*store.Session)
	return s
}

// WithUser returns a new context carrying the given user (for testing).
func WithUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, userContextKey{}, u)
}

// WithSession returns a new context carrying the given session (for testing).
func WithSession(ctx context.Context, s *store.Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, s)
}

func extractSessionToken(r *http.Request) string {
	// Try Authorization: Bearer <token> first
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		return strings.TrimPrefix(authHeader, "Bearer ")
	}
	// Fall back to X-Session-Token header
	return r.Header.Get("X-Session-Token")
}

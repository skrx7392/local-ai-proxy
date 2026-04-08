package requestid

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"

	"github.com/krishna/local-ai-proxy/internal/logging"
)

var validIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.\-]+$`)

// Middleware wraps an http.Handler to assign a unique request ID.
// If a valid X-Request-ID header is present (non-empty, <= 128 chars,
// alphanumeric/underscore/dot/dash), it is reused. Otherwise a new
// ID is generated as "req_" + 32 hex chars (16 random bytes).
// The ID is stored in the request context and set as a response header.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !isValidID(id) {
			id = generateID()
		}

		w.Header().Set("X-Request-ID", id)
		ctx := logging.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isValidID(id string) bool {
	return id != "" && len(id) <= 128 && validIDPattern.MatchString(id)
}

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

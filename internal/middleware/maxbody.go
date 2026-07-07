package middleware

import "net/http"

// MaxBody caps the readable request body at limit bytes for every request
// passing through it. Reads past the cap fail with *http.MaxBytesError,
// which apierror.DecodeJSON maps to a 413 envelope. Intended for the JSON
// API mounts; the chat proxy keeps its own larger cap (MAX_REQUEST_BODY).
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

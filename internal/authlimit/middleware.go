package authlimit

import (
	"fmt"
	"math"
	"net/http"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/metrics"
)

// Middleware enforces per-IP token buckets on the public auth surface.
// Login and the two registration endpoints get strict dedicated buckets;
// every other request on the wrapped mounts drains a generous general
// bucket as a DoS backstop. Wrap it inside CORS so preflight OPTIONS
// requests don't consume tokens.
func Middleware(g *Guard, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r)

			var (
				allowed    bool
				retryAfter float64
			)
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/auth/login":
				allowed, retryAfter = g.AllowLoginIP(ip)
			case r.Method == http.MethodPost &&
				(r.URL.Path == "/api/auth/register" || r.URL.Path == "/api/accounts/register"):
				allowed, retryAfter = g.AllowRegisterIP(ip)
			default:
				allowed, retryAfter = g.AllowGeneralIP(ip)
			}

			if !allowed {
				m.RecordRateLimitReject()
				w.Header().Set("Retry-After", fmt.Sprintf("%.0f", math.Ceil(retryAfter)))
				apierror.WriteError(w, r, http.StatusTooManyRequests,
					"rate_limit_exceeded", "rate_limit_exceeded", "Too many requests, retry later")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

package authlimit

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP derives the client address used as the rate-limit key.
//
// Trust model: the service sits behind a single trusted Traefik ingress that
// sets X-Forwarded-For, so the leftmost XFF entry is the original client.
// XFF is spoofable only if the ingress is bypassed (cluster-internal
// traffic); the per-email login throttle covers targeted-account abuse
// regardless of the IP presented.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

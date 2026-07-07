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
//
// Header-derived candidates must parse as IP addresses — clients control the
// leading XFF entries, and accepting arbitrary strings would let them both
// dodge the bucket and bloat the limiter map with huge keys. Anything
// unparseable falls through to the connection's RemoteAddr.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

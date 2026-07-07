package authlimit

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP_XForwardedForLeftmost(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.42.0.1:55555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1, 10.0.0.2")

	if got := ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want leftmost XFF entry 203.0.113.7", got)
	}
}

func TestClientIP_XForwardedForSingleEntryWithSpaces(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.Header.Set("X-Forwarded-For", "  198.51.100.9  ")

	if got := ClientIP(r); got != "198.51.100.9" {
		t.Errorf("ClientIP = %q, want trimmed 198.51.100.9", got)
	}
}

func TestClientIP_FallsBackToXRealIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "10.42.0.1:55555"
	r.Header.Set("X-Real-IP", "198.51.100.23")

	if got := ClientIP(r); got != "198.51.100.23" {
		t.Errorf("ClientIP = %q, want X-Real-IP 198.51.100.23", got)
	}
}

func TestClientIP_FallsBackToRemoteAddrHost(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.44:12345"

	if got := ClientIP(r); got != "192.0.2.44" {
		t.Errorf("ClientIP = %q, want RemoteAddr host 192.0.2.44", got)
	}
}

func TestClientIP_RemoteAddrWithoutPort(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.44"

	if got := ClientIP(r); got != "192.0.2.44" {
		t.Errorf("ClientIP = %q, want raw RemoteAddr 192.0.2.44", got)
	}
}

func TestClientIP_GarbageXFFFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.44:12345"
	// Clients control the leading XFF entries; junk must not become a
	// rate-limit key.
	r.Header.Set("X-Forwarded-For", "not-an-ip-aaaaaaaaaaaaaaaaaaaaaaaa, 10.0.0.1")

	if got := ClientIP(r); got != "192.0.2.44" {
		t.Errorf("ClientIP = %q, want RemoteAddr host for unparseable XFF", got)
	}
}

func TestClientIP_GarbageXRealIPFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.44:12345"
	r.Header.Set("X-Real-IP", "junk-value")

	if got := ClientIP(r); got != "192.0.2.44" {
		t.Errorf("ClientIP = %q, want RemoteAddr host for unparseable X-Real-IP", got)
	}
}

func TestClientIP_IPv6XFF(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.44:12345"
	r.Header.Set("X-Forwarded-For", "2001:db8::7")

	if got := ClientIP(r); got != "2001:db8::7" {
		t.Errorf("ClientIP = %q, want 2001:db8::7", got)
	}
}

func TestClientIP_EmptyXFFIgnored(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.44:12345"
	r.Header.Set("X-Forwarded-For", "")

	if got := ClientIP(r); got != "192.0.2.44" {
		t.Errorf("ClientIP = %q, want RemoteAddr host when XFF empty", got)
	}
}

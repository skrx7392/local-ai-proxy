package requestid

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/logging"
)

func TestMiddleware_GeneratesID(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = logging.RequestIDFromContext(r.Context())
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if capturedID == "" {
		t.Fatal("expected request ID in context")
	}
	if !regexp.MustCompile(`^req_[0-9a-f]{32}$`).MatchString(capturedID) {
		t.Errorf("unexpected request ID format: %q", capturedID)
	}
	if rec.Header().Get("X-Request-ID") != capturedID {
		t.Errorf("expected X-Request-ID header %q, got %q", capturedID, rec.Header().Get("X-Request-ID"))
	}
}

func TestMiddleware_RespectsValidIncoming(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = logging.RequestIDFromContext(r.Context())
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", "my-custom-id-123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if capturedID != "my-custom-id-123" {
		t.Errorf("expected 'my-custom-id-123', got %q", capturedID)
	}
	if rec.Header().Get("X-Request-ID") != "my-custom-id-123" {
		t.Errorf("expected response header 'my-custom-id-123', got %q", rec.Header().Get("X-Request-ID"))
	}
}

func TestMiddleware_RejectsInvalidIncoming(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"too long", string(make([]byte, 129))},
		{"invalid chars", "req id with spaces"},
		{"special chars", "req_id<script>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var capturedID string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedID = logging.RequestIDFromContext(r.Context())
			})

			handler := Middleware(inner)
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tc.value != "" {
				req.Header.Set("X-Request-ID", tc.value)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			// Should have generated a new ID
			if !regexp.MustCompile(`^req_[0-9a-f]{32}$`).MatchString(capturedID) {
				t.Errorf("expected generated ID for invalid input %q, got %q", tc.value, capturedID)
			}
		})
	}
}

func TestMiddleware_AllowsDotsAndDashes(t *testing.T) {
	var capturedID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = logging.RequestIDFromContext(r.Context())
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", "trace-id_123.abc")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if capturedID != "trace-id_123.abc" {
		t.Errorf("expected 'trace-id_123.abc', got %q", capturedID)
	}
}

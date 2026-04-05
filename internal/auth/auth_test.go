package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func TestHashKey_Deterministic(t *testing.T) {
	h1 := HashKey("my-secret-key")
	h2 := HashKey("my-secret-key")
	if h1 != h2 {
		t.Errorf("HashKey not deterministic: %q != %q", h1, h2)
	}
}

func TestHashKey_DifferentInputs(t *testing.T) {
	h1 := HashKey("key-one")
	h2 := HashKey("key-two")
	if h1 == h2 {
		t.Error("HashKey produced same output for different inputs")
	}
}

func TestHashKey_Length(t *testing.T) {
	h := HashKey("test")
	// SHA-256 produces 64 hex characters
	if len(h) != 64 {
		t.Errorf("expected hash length 64, got %d", len(h))
	}
}

func TestKeyFromContext_Roundtrip(t *testing.T) {
	key := &store.APIKey{
		ID:        42,
		Name:      "test-key",
		RateLimit: 60,
	}

	ctx := context.WithValue(context.Background(), contextKey{}, key)
	got := KeyFromContext(ctx)
	if got == nil {
		t.Fatal("KeyFromContext returned nil")
	}
	if got.ID != 42 {
		t.Errorf("expected ID 42, got %d", got.ID)
	}
	if got.Name != "test-key" {
		t.Errorf("expected name 'test-key', got %q", got.Name)
	}
}

func TestKeyFromContext_EmptyContext(t *testing.T) {
	got := KeyFromContext(context.Background())
	if got != nil {
		t.Errorf("expected nil from empty context, got %+v", got)
	}
}

func setupStoreForAuth(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping auth integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
	})

	return s
}

func TestMiddleware_ValidKey(t *testing.T) {
	s := setupStoreForAuth(t)

	rawKey := "sk-test-valid-auth-key-12345678"
	hash := HashKey(rawKey)
	_, err := s.CreateKey("auth-test", hash, rawKey[:11], 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	handler := Middleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := KeyFromContext(r.Context())
		if key == nil {
			t.Error("expected key in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_InvalidKey(t *testing.T) {
	s := setupStoreForAuth(t)

	handler := Middleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer invalid-key-that-does-not-exist")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_MissingHeader(t *testing.T) {
	s := setupStoreForAuth(t)

	handler := Middleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	// No Authorization header
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_MalformedHeader(t *testing.T) {
	s := setupStoreForAuth(t)

	handler := Middleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Basic some-creds") // Not "Bearer"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for non-Bearer auth, got %d", rec.Code)
	}
}

package config

import (
	"testing"
)

func TestLoad_MissingAdminKey(t *testing.T) {
	t.Setenv("ADMIN_KEY", "")
	t.Setenv("DATABASE_URL", "postgres://localhost/test")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing ADMIN_KEY")
	}
	if got := err.Error(); got != "ADMIN_KEY environment variable is required" {
		t.Errorf("unexpected error: %v", got)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	t.Setenv("ADMIN_KEY", "secret")
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing DATABASE_URL")
	}
	if got := err.Error(); got != "DATABASE_URL environment variable is required" {
		t.Errorf("unexpected error: %v", got)
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("ADMIN_KEY", "my-admin-key")
	t.Setenv("DATABASE_URL", "postgres://localhost/testdb")
	t.Setenv("PORT", "")
	t.Setenv("OLLAMA_URL", "")
	t.Setenv("CORS_ORIGINS", "")
	t.Setenv("MAX_REQUEST_BODY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("expected default port '8080', got %q", cfg.Port)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("expected default OllamaURL, got %q", cfg.OllamaURL)
	}
	if cfg.CORSOrigins != "*" {
		t.Errorf("expected default CORSOrigins '*', got %q", cfg.CORSOrigins)
	}
	if cfg.MaxRequestBody != 52428800 {
		t.Errorf("expected default MaxRequestBody 52428800, got %d", cfg.MaxRequestBody)
	}
	if cfg.AdminKey != "my-admin-key" {
		t.Errorf("expected AdminKey 'my-admin-key', got %q", cfg.AdminKey)
	}
	if cfg.DatabaseURL != "postgres://localhost/testdb" {
		t.Errorf("expected DatabaseURL, got %q", cfg.DatabaseURL)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	t.Setenv("ADMIN_KEY", "custom-admin")
	t.Setenv("DATABASE_URL", "postgres://user:pass@host/db")
	t.Setenv("PORT", "3000")
	t.Setenv("OLLAMA_URL", "http://ollama:11434")
	t.Setenv("CORS_ORIGINS", "https://example.com")
	t.Setenv("MAX_REQUEST_BODY", "1048576")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != "3000" {
		t.Errorf("expected port '3000', got %q", cfg.Port)
	}
	if cfg.OllamaURL != "http://ollama:11434" {
		t.Errorf("expected custom OllamaURL, got %q", cfg.OllamaURL)
	}
	if cfg.CORSOrigins != "https://example.com" {
		t.Errorf("expected custom CORSOrigins, got %q", cfg.CORSOrigins)
	}
	if cfg.MaxRequestBody != 1048576 {
		t.Errorf("expected MaxRequestBody 1048576, got %d", cfg.MaxRequestBody)
	}
}

func TestLoad_InvalidMaxRequestBody(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("MAX_REQUEST_BODY", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid MAX_REQUEST_BODY")
	}
}

package config

import (
	"os"
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
	t.Setenv("LOG_LEVEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("expected default port '8080', got %q", cfg.Port)
	}
	// OLLAMA_URL no longer defaults to localhost: unset (or explicitly
	// empty) means no implicit node is ever synthesized.
	if cfg.OllamaURL != "" {
		t.Errorf("expected empty OllamaURL when unset, got %q", cfg.OllamaURL)
	}
	if cfg.OllamaURLSet {
		t.Error("expected OllamaURLSet=false when OLLAMA_URL is unset")
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
	if cfg.LogLevel != "info" {
		t.Errorf("expected default LogLevel 'info', got %q", cfg.LogLevel)
	}
}

func TestLoad_AdminBootstrapToken_DefaultEmpty(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("ADMIN_BOOTSTRAP_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminBootstrapToken != "" {
		t.Errorf("expected empty AdminBootstrapToken, got %q", cfg.AdminBootstrapToken)
	}
}

func TestLoad_AdminBootstrapToken_WhenSet(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("ADMIN_BOOTSTRAP_TOKEN", "super-secret-bootstrap")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminBootstrapToken != "super-secret-bootstrap" {
		t.Errorf("expected AdminBootstrapToken 'super-secret-bootstrap', got %q", cfg.AdminBootstrapToken)
	}
}

func TestLoad_CustomLogLevel(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected LogLevel 'debug', got %q", cfg.LogLevel)
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
	if !cfg.OllamaURLSet {
		t.Error("expected OllamaURLSet=true when OLLAMA_URL is explicitly set")
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

func TestLoad_MaxJSONBodyDefaultAndCustom(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("MAX_JSON_REQUEST_BODY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxJSONBody != 1048576 {
		t.Errorf("MaxJSONBody = %d, want default 1048576", cfg.MaxJSONBody)
	}

	t.Setenv("MAX_JSON_REQUEST_BODY", "2097152")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxJSONBody != 2097152 {
		t.Errorf("MaxJSONBody = %d, want 2097152", cfg.MaxJSONBody)
	}
}

func TestLoad_MaxJSONBodyRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"not-a-number", "0", "-5"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("ADMIN_KEY", "key")
			t.Setenv("DATABASE_URL", "postgres://localhost/db")
			t.Setenv("MAX_JSON_REQUEST_BODY", bad)

			if _, err := Load(); err == nil {
				t.Fatalf("expected error for MAX_JSON_REQUEST_BODY=%s", bad)
			}
		})
	}
}

func TestLoad_OllamaURL_UnsetMeansNoSynthesis(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	// t.Setenv registers restoration; Unsetenv makes the variable truly
	// absent (LookupEnv ok=false), not merely empty.
	t.Setenv("OLLAMA_URL", "placeholder")
	os.Unsetenv("OLLAMA_URL")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaURLSet {
		t.Error("expected OllamaURLSet=false when OLLAMA_URL is absent")
	}
	if cfg.OllamaURL != "" {
		t.Errorf("expected empty OllamaURL, got %q", cfg.OllamaURL)
	}
}

func TestLoad_OllamaURL_ExplicitlyEmptyTreatedAsUnset(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("OLLAMA_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaURLSet {
		t.Error("expected OllamaURLSet=false for explicitly empty OLLAMA_URL")
	}
}

func TestLoad_OllamaURL_ExplicitPresence(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("OLLAMA_URL", "http://localhost:11434")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.OllamaURLSet {
		t.Error("expected OllamaURLSet=true when OLLAMA_URL is set")
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q, want http://localhost:11434", cfg.OllamaURL)
	}
}

func TestLoad_NodesFile(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("NODES_FILE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodesFile != "" {
		t.Errorf("expected empty NodesFile default, got %q", cfg.NodesFile)
	}

	t.Setenv("NODES_FILE", "/etc/laip/nodes.json")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodesFile != "/etc/laip/nodes.json" {
		t.Errorf("NodesFile = %q, want /etc/laip/nodes.json", cfg.NodesFile)
	}
}

func TestLoad_AuthRateLimitDefaults(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("AUTH_RATELIMIT_LOGIN_PER_MIN", "")
	t.Setenv("AUTH_RATELIMIT_LOGIN_EMAIL_PER_MIN", "")
	t.Setenv("AUTH_RATELIMIT_REGISTER_PER_MIN", "")
	t.Setenv("AUTH_RATELIMIT_GENERAL_PER_MIN", "")
	t.Setenv("AUTH_BCRYPT_MAX_CONCURRENT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthLoginPerMinIP != 5 {
		t.Errorf("AuthLoginPerMinIP = %d, want default 5", cfg.AuthLoginPerMinIP)
	}
	if cfg.AuthLoginPerMinEmail != 5 {
		t.Errorf("AuthLoginPerMinEmail = %d, want default 5", cfg.AuthLoginPerMinEmail)
	}
	if cfg.AuthRegisterPerMinIP != 3 {
		t.Errorf("AuthRegisterPerMinIP = %d, want default 3", cfg.AuthRegisterPerMinIP)
	}
	if cfg.AuthGeneralPerMinIP != 120 {
		t.Errorf("AuthGeneralPerMinIP = %d, want default 120", cfg.AuthGeneralPerMinIP)
	}
	if cfg.AuthBcryptConcurrency != 8 {
		t.Errorf("AuthBcryptConcurrency = %d, want default 8", cfg.AuthBcryptConcurrency)
	}
}

func TestLoad_AuthRateLimitCustomValues(t *testing.T) {
	t.Setenv("ADMIN_KEY", "key")
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("AUTH_RATELIMIT_LOGIN_PER_MIN", "10")
	t.Setenv("AUTH_RATELIMIT_LOGIN_EMAIL_PER_MIN", "7")
	t.Setenv("AUTH_RATELIMIT_REGISTER_PER_MIN", "2")
	t.Setenv("AUTH_RATELIMIT_GENERAL_PER_MIN", "300")
	t.Setenv("AUTH_BCRYPT_MAX_CONCURRENT", "4")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuthLoginPerMinIP != 10 || cfg.AuthLoginPerMinEmail != 7 ||
		cfg.AuthRegisterPerMinIP != 2 || cfg.AuthGeneralPerMinIP != 300 ||
		cfg.AuthBcryptConcurrency != 4 {
		t.Errorf("custom auth limits not applied: %+v", cfg)
	}
}

func TestLoad_AuthRateLimitRejectsInvalid(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"AUTH_RATELIMIT_LOGIN_PER_MIN", "not-a-number"},
		{"AUTH_RATELIMIT_LOGIN_EMAIL_PER_MIN", "0"},
		{"AUTH_RATELIMIT_REGISTER_PER_MIN", "-3"},
		{"AUTH_RATELIMIT_GENERAL_PER_MIN", "1.5"},
		{"AUTH_BCRYPT_MAX_CONCURRENT", "0"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv("ADMIN_KEY", "key")
			t.Setenv("DATABASE_URL", "postgres://localhost/db")
			t.Setenv(tc.key, tc.value)

			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%s", tc.key, tc.value)
			}
		})
	}
}

package nodesource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/config"
)

// writeNodesFile writes content to a temp file and returns its path.
func writeNodesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing nodes file: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// JSON parsing
// ---------------------------------------------------------------------------

func TestLoadDeclared_ValidFile(t *testing.T) {
	path := writeNodesFile(t, `{
		"nodes": [
			{"name": "m5-max", "base_url": "http://100.101.2.3:11434", "backend_type": "ollama",
			 "timeout_seconds": 900},
			{"name": "cloud", "base_url": "https://api.example.com", "backend_type": "openai_compat",
			 "auth_header": "Bearer sk-test-1234567890", "static_models": ["gpt-4o-mini"],
			 "health_path": "/healthz"}
		]
	}`)

	declared, err := loadDeclared(config.Config{NodesFile: path})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if len(declared) != 2 {
		t.Fatalf("expected 2 declared nodes, got %d", len(declared))
	}

	n := declared[0]
	if n.Name != "m5-max" || n.BaseURL != "http://100.101.2.3:11434" || n.BackendType != "ollama" {
		t.Errorf("unexpected first node: %+v", n)
	}
	if n.TimeoutSeconds == nil || *n.TimeoutSeconds != 900 {
		t.Errorf("expected timeout_seconds 900, got %v", n.TimeoutSeconds)
	}
	if n.Source != "config" {
		t.Errorf("expected source=config, got %q", n.Source)
	}
	if !n.Enabled {
		t.Error("declared nodes must be enabled")
	}

	c := declared[1]
	if c.AuthHeader == nil || *c.AuthHeader != "Bearer sk-test-1234567890" {
		t.Errorf("unexpected auth_header: %v", c.AuthHeader)
	}
	if len(c.StaticModels) != 1 || c.StaticModels[0] != "gpt-4o-mini" {
		t.Errorf("unexpected static_models: %v", c.StaticModels)
	}
	if c.HealthPath == nil || *c.HealthPath != "/healthz" {
		t.Errorf("unexpected health_path: %v", c.HealthPath)
	}
}

func TestLoadDeclared_EmptyNodesList(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": []}`)
	declared, err := loadDeclared(config.Config{NodesFile: path})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if len(declared) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(declared))
	}
}

func TestLoadDeclared_UnknownFieldRejected(t *testing.T) {
	path := writeNodesFile(t, `{
		"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "ollama",
		           "timeout_secs": 5}]
	}`)
	_, err := loadDeclared(config.Config{NodesFile: path})
	if err == nil {
		t.Fatal("expected error for unknown field 'timeout_secs'")
	}
	if !strings.Contains(err.Error(), "timeout_secs") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestLoadDeclared_TopLevelUnknownFieldRejected(t *testing.T) {
	path := writeNodesFile(t, `{"nodez": []}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for unknown top-level field 'nodez'")
	}
}

func TestLoadDeclared_InvalidJSON(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadDeclared_TrailingDataRejected(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": []}{"nodes": []}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for trailing data after JSON document")
	}
}

func TestLoadDeclared_UnreadableFile(t *testing.T) {
	_, err := loadDeclared(config.Config{NodesFile: filepath.Join(t.TempDir(), "does-not-exist.json")})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// ---------------------------------------------------------------------------
// ${VAR} expansion
// ---------------------------------------------------------------------------

func TestLoadDeclared_EnvExpansion(t *testing.T) {
	t.Setenv("BE5_TEST_CLOUD_KEY", "sk-secret-abcdef123456")
	t.Setenv("BE5_TEST_HOST", "api.example.com")
	path := writeNodesFile(t, `{
		"nodes": [{"name": "cloud", "base_url": "https://${BE5_TEST_HOST}",
		           "backend_type": "openai_compat",
		           "auth_header": "Bearer ${BE5_TEST_CLOUD_KEY}"}]
	}`)

	declared, err := loadDeclared(config.Config{NodesFile: path})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if declared[0].BaseURL != "https://api.example.com" {
		t.Errorf("base_url not expanded: %q", declared[0].BaseURL)
	}
	if declared[0].AuthHeader == nil || *declared[0].AuthHeader != "Bearer sk-secret-abcdef123456" {
		t.Errorf("auth_header not expanded: %v", declared[0].AuthHeader)
	}
}

func TestLoadDeclared_EnvExpansionInStaticModels(t *testing.T) {
	t.Setenv("BE5_TEST_MODEL", "llama3:8b")
	path := writeNodesFile(t, `{
		"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "ollama",
		           "static_models": ["${BE5_TEST_MODEL}"]}]
	}`)

	declared, err := loadDeclared(config.Config{NodesFile: path})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if len(declared[0].StaticModels) != 1 || declared[0].StaticModels[0] != "llama3:8b" {
		t.Errorf("static_models not expanded: %v", declared[0].StaticModels)
	}
}

func TestLoadDeclared_UndefinedVarFailsNamingVariable(t *testing.T) {
	os.Unsetenv("BE5_TEST_UNDEFINED_VAR")
	path := writeNodesFile(t, `{
		"nodes": [{"name": "cloud", "base_url": "https://api.example.com",
		           "backend_type": "openai_compat",
		           "auth_header": "Bearer ${BE5_TEST_UNDEFINED_VAR}"}]
	}`)

	_, err := loadDeclared(config.Config{NodesFile: path})
	if err == nil {
		t.Fatal("expected error for undefined ${BE5_TEST_UNDEFINED_VAR}")
	}
	if !strings.Contains(err.Error(), "BE5_TEST_UNDEFINED_VAR") {
		t.Errorf("error should name the undefined variable, got: %v", err)
	}
}

func TestLoadDeclared_DefinedEmptyVarSubstitutes(t *testing.T) {
	// A variable that is set-but-empty is defined; only truly undefined
	// variables fail. Empty expansion surfaces via field validation instead.
	t.Setenv("BE5_TEST_EMPTY", "")
	path := writeNodesFile(t, `{
		"nodes": [{"name": "a${BE5_TEST_EMPTY}", "base_url": "http://h:1", "backend_type": "ollama"}]
	}`)
	declared, err := loadDeclared(config.Config{NodesFile: path})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if declared[0].Name != "a" {
		t.Errorf("Name = %q, want \"a\"", declared[0].Name)
	}
}

func TestLoadDeclared_UnterminatedVarRejected(t *testing.T) {
	path := writeNodesFile(t, `{
		"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "ollama",
		           "auth_header": "Bearer ${BROKEN"}]
	}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for unterminated ${")
	}
}

// ---------------------------------------------------------------------------
// Validation (store validators must reject bad declarations at load time)
// ---------------------------------------------------------------------------

func TestLoadDeclared_BadBaseURLRejected(t *testing.T) {
	cases := []string{
		`{"nodes": [{"name": "a", "base_url": "not a url", "backend_type": "ollama"}]}`,
		`{"nodes": [{"name": "a", "base_url": "http://h:11434/v1", "backend_type": "ollama"}]}`,
		`{"nodes": [{"name": "a", "base_url": "", "backend_type": "ollama"}]}`,
		`{"nodes": [{"name": "a", "base_url": "ftp://h/x", "backend_type": "ollama"}]}`,
	}
	for _, c := range cases {
		path := writeNodesFile(t, c)
		if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
			t.Errorf("expected validation error for %s", c)
		}
	}
}

func TestLoadDeclared_BadBackendTypeRejected(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "vllm"}]}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for backend_type=vllm")
	}
}

func TestLoadDeclared_EmptyBackendTypeDefaultsToOllama(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "a", "base_url": "http://h:1"}]}`)
	declared, err := loadDeclared(config.Config{NodesFile: path})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if declared[0].BackendType != "ollama" {
		t.Errorf("BackendType = %q, want ollama", declared[0].BackendType)
	}
}

func TestLoadDeclared_MissingNameRejected(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"base_url": "http://h:1", "backend_type": "ollama"}]}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestLoadDeclared_BadTimeoutRejected(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "a", "base_url": "http://h:1", "timeout_seconds": 0}]}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for timeout_seconds=0")
	}
}

func TestLoadDeclared_BadHealthPathRejected(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "a", "base_url": "http://h:1", "health_path": "healthz"}]}`)
	if _, err := loadDeclared(config.Config{NodesFile: path}); err == nil {
		t.Fatal("expected error for health_path without leading slash")
	}
}

func TestLoadDeclared_DuplicateNamesRejected(t *testing.T) {
	path := writeNodesFile(t, `{
		"nodes": [
			{"name": "dup", "base_url": "http://h1:1", "backend_type": "ollama"},
			{"name": "dup", "base_url": "http://h2:1", "backend_type": "ollama"}
		]
	}`)
	_, err := loadDeclared(config.Config{NodesFile: path})
	if err == nil {
		t.Fatal("expected error for duplicate node names in file")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should name the duplicated node, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// OLLAMA_URL synthesis (load stage — merge behavior is covered in sync_test.go)
// ---------------------------------------------------------------------------

func TestLoadDeclared_OllamaURLSynthesis(t *testing.T) {
	declared, err := loadDeclared(config.Config{OllamaURL: "http://localhost:11434", OllamaURLSet: true})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if len(declared) != 1 {
		t.Fatalf("expected 1 synthesized node, got %d", len(declared))
	}
	n := declared[0]
	if n.Name != "default" || n.BaseURL != "http://localhost:11434" || n.BackendType != "ollama" {
		t.Errorf("unexpected synthesized node: %+v", n)
	}
	if n.Source != "config" || !n.Enabled {
		t.Errorf("synthesized node must be enabled with source=config: %+v", n)
	}
}

func TestLoadDeclared_NoOllamaURLNoSynthesis(t *testing.T) {
	declared, err := loadDeclared(config.Config{OllamaURL: "", OllamaURLSet: false})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if len(declared) != 0 {
		t.Errorf("expected zero declared nodes without OLLAMA_URL or NODES_FILE, got %d", len(declared))
	}
}

func TestLoadDeclared_InvalidOllamaURLRejected(t *testing.T) {
	_, err := loadDeclared(config.Config{OllamaURL: "not a url", OllamaURLSet: true})
	if err == nil {
		t.Fatal("expected error for invalid OLLAMA_URL")
	}
	if !strings.Contains(err.Error(), "OLLAMA_URL") {
		t.Errorf("error should mention OLLAMA_URL, got: %v", err)
	}
}

func TestLoadDeclared_FileNodePlusSynthesis(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "m5-max", "base_url": "http://h:1", "backend_type": "ollama"}]}`)
	declared, err := loadDeclared(config.Config{
		NodesFile:    path,
		OllamaURL:    "http://localhost:11434",
		OllamaURLSet: true,
	})
	if err != nil {
		t.Fatalf("loadDeclared: %v", err)
	}
	if len(declared) != 2 {
		t.Fatalf("expected file node + synthesized node, got %d", len(declared))
	}
}

func TestLoadDeclared_FileDefaultCollidesWithSynthesis(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "default", "base_url": "http://h:1", "backend_type": "ollama"}]}`)
	_, err := loadDeclared(config.Config{
		NodesFile:    path,
		OllamaURL:    "http://localhost:11434",
		OllamaURLSet: true,
	})
	if err == nil {
		t.Fatal("expected error when file declares 'default' and OLLAMA_URL synthesis is active")
	}
	if !strings.Contains(err.Error(), "default") || !strings.Contains(err.Error(), "OLLAMA_URL") {
		t.Errorf("error should explain the default/OLLAMA_URL collision, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fail-fast: SyncDeclaredNodes must fail before touching the store when the
// declaration itself is invalid (nil store proves no DB access happened).
// ---------------------------------------------------------------------------

func TestSyncDeclaredNodes_FailsFastOnInvalidFile(t *testing.T) {
	path := writeNodesFile(t, `{"nodes": [{"name": "a", "base_url": "not a url"}]}`)
	err := SyncDeclaredNodes(context.Background(), nil, config.Config{NodesFile: path})
	if err == nil {
		t.Fatal("expected startup error for invalid nodes file")
	}
}

func TestSyncDeclaredNodes_FailsFastOnUnreadableFile(t *testing.T) {
	err := SyncDeclaredNodes(context.Background(), nil, config.Config{
		NodesFile: filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil {
		t.Fatal("expected startup error for unreadable nodes file")
	}
}

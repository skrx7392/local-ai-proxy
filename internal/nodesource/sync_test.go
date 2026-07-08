package nodesource

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/config"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// setupSyncStore connects to the integration-test database, wiping the nodes
// table before and after each test (same pattern as internal/store tests).
func setupSyncStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping nodesource integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	wipe := func() {
		c := context.Background()
		// usage_logs references nodes; delete referencing rows first.
		_, _ = s.Pool().Exec(c, "DELETE FROM usage_logs")
		_, _ = s.Pool().Exec(c, "DELETE FROM nodes")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})
	return s
}

func syncFile(t *testing.T, s *store.Store, content string) error {
	t.Helper()
	return SyncDeclaredNodes(context.Background(), s, config.Config{NodesFile: writeNodesFile(t, content)})
}

func mustSyncFile(t *testing.T, s *store.Store, content string) {
	t.Helper()
	if err := syncFile(t, s, content); err != nil {
		t.Fatalf("SyncDeclaredNodes: %v", err)
	}
}

func nodesByName(t *testing.T, s *store.Store) map[string]store.Node {
	t.Helper()
	nodes, err := s.ListNodesWithSecrets()
	if err != nil {
		t.Fatalf("ListNodesWithSecrets: %v", err)
	}
	m := make(map[string]store.Node, len(nodes))
	for _, n := range nodes {
		m[n.Name] = n
	}
	return m
}

// ---------------------------------------------------------------------------
// Merge rule: create
// ---------------------------------------------------------------------------

func TestSync_CreatesFileNodes(t *testing.T) {
	s := setupSyncStore(t)
	t.Setenv("BE5_SYNC_KEY", "sk-live-1234567890abcd")
	mustSyncFile(t, s, `{
		"nodes": [
			{"name": "m5-max", "base_url": "http://100.101.2.3:11434", "backend_type": "ollama",
			 "timeout_seconds": 900},
			{"name": "cloud", "base_url": "https://api.example.com", "backend_type": "openai_compat",
			 "auth_header": "Bearer ${BE5_SYNC_KEY}", "static_models": ["gpt-4o-mini"]}
		]
	}`)

	byName := nodesByName(t, s)
	if len(byName) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(byName))
	}

	m5 := byName["m5-max"]
	if m5.Source != "config" || !m5.Enabled {
		t.Errorf("m5-max should be enabled config node: %+v", m5)
	}
	if m5.TimeoutSeconds == nil || *m5.TimeoutSeconds != 900 {
		t.Errorf("m5-max timeout: %v", m5.TimeoutSeconds)
	}

	cloud := byName["cloud"]
	if cloud.AuthHeader == nil || *cloud.AuthHeader != "Bearer sk-live-1234567890abcd" {
		t.Errorf("cloud auth_header should be stored raw and expanded, got %v", cloud.AuthHeader)
	}
	if len(cloud.StaticModels) != 1 || cloud.StaticModels[0] != "gpt-4o-mini" {
		t.Errorf("cloud static_models: %v", cloud.StaticModels)
	}
}

// ---------------------------------------------------------------------------
// Merge rule: update existing config node
// ---------------------------------------------------------------------------

func TestSync_UpdatesExistingConfigNode(t *testing.T) {
	s := setupSyncStore(t)
	mustSyncFile(t, s, `{"nodes": [{"name": "a", "base_url": "http://old-host:11434", "backend_type": "ollama"}]}`)

	before := nodesByName(t, s)["a"]

	mustSyncFile(t, s, `{"nodes": [{"name": "a", "base_url": "http://new-host:11434", "backend_type": "ollama",
	                                "timeout_seconds": 300}]}`)

	after := nodesByName(t, s)
	if len(after) != 1 {
		t.Fatalf("expected 1 node after update, got %d", len(after))
	}
	a := after["a"]
	if a.ID != before.ID {
		t.Errorf("update must reuse the same row: id %d -> %d", before.ID, a.ID)
	}
	if a.BaseURL != "http://new-host:11434" {
		t.Errorf("BaseURL = %q, want http://new-host:11434", a.BaseURL)
	}
	if a.TimeoutSeconds == nil || *a.TimeoutSeconds != 300 {
		t.Errorf("TimeoutSeconds = %v, want 300", a.TimeoutSeconds)
	}
	if a.Source != "config" || !a.Enabled {
		t.Errorf("updated node should stay enabled config: %+v", a)
	}
}

func TestSync_UpdateClearsRemovedAuthHeader(t *testing.T) {
	s := setupSyncStore(t)
	mustSyncFile(t, s, `{"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "openai_compat",
	                                "auth_header": "Bearer sk-old-1234567890"}]}`)
	mustSyncFile(t, s, `{"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "openai_compat"}]}`)

	a := nodesByName(t, s)["a"]
	if a.AuthHeader != nil {
		t.Errorf("auth_header should be cleared when removed from file, got %v", *a.AuthHeader)
	}
}

// ---------------------------------------------------------------------------
// Merge rule: disable config nodes absent from the file
// ---------------------------------------------------------------------------

func TestSync_DisablesConfigNodesAbsentFromFile(t *testing.T) {
	s := setupSyncStore(t)
	mustSyncFile(t, s, `{"nodes": [
		{"name": "keep", "base_url": "http://h1:1", "backend_type": "ollama"},
		{"name": "drop", "base_url": "http://h2:1", "backend_type": "ollama"}
	]}`)
	mustSyncFile(t, s, `{"nodes": [{"name": "keep", "base_url": "http://h1:1", "backend_type": "ollama"}]}`)

	byName := nodesByName(t, s)
	if !byName["keep"].Enabled {
		t.Error("keep should remain enabled")
	}
	drop := byName["drop"]
	if drop.Enabled {
		t.Error("drop should be disabled after removal from file")
	}
	if drop.Source != "config" {
		t.Errorf("drop keeps source=config, got %q", drop.Source)
	}
}

func TestSync_ReenablesNodeRestoredToFile(t *testing.T) {
	s := setupSyncStore(t)
	mustSyncFile(t, s, `{"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "ollama"}]}`)
	mustSyncFile(t, s, `{"nodes": []}`)
	if nodesByName(t, s)["a"].Enabled {
		t.Fatal("a should be disabled after removal")
	}
	mustSyncFile(t, s, `{"nodes": [{"name": "a", "base_url": "http://h:1", "backend_type": "ollama"}]}`)
	if !nodesByName(t, s)["a"].Enabled {
		t.Error("a should be re-enabled when restored to the file")
	}
}

// ---------------------------------------------------------------------------
// Merge rule: API-sourced nodes untouched
// ---------------------------------------------------------------------------

func TestSync_APINodesUntouched(t *testing.T) {
	s := setupSyncStore(t)
	auth := "Bearer sk-api-1234567890"
	if _, err := s.CreateNode(store.Node{
		Name: "api-node", BaseURL: "http://api-host:11434", BackendType: "ollama", AuthHeader: &auth,
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	mustSyncFile(t, s, `{"nodes": [{"name": "file-node", "base_url": "http://h:1", "backend_type": "ollama"}]}`)

	api := nodesByName(t, s)["api-node"]
	if api.Source != "api" {
		t.Fatalf("api-node source = %q", api.Source)
	}
	if !api.Enabled {
		t.Error("api-node must not be disabled by file sync")
	}
	if api.BaseURL != "http://api-host:11434" {
		t.Errorf("api-node base_url changed: %q", api.BaseURL)
	}
	if api.AuthHeader == nil || *api.AuthHeader != auth {
		t.Errorf("api-node auth_header changed: %v", api.AuthHeader)
	}
}

// ---------------------------------------------------------------------------
// Merge rule: name collision with API-sourced node is a hard startup error
// ---------------------------------------------------------------------------

func TestSync_APINameCollisionFailsStartup(t *testing.T) {
	s := setupSyncStore(t)
	if _, err := s.CreateNode(store.Node{
		Name: "shared", BaseURL: "http://api-host:11434", BackendType: "ollama",
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	err := syncFile(t, s, `{"nodes": [
		{"name": "brand-new", "base_url": "http://h9:1", "backend_type": "ollama"},
		{"name": "shared", "base_url": "http://other:11434", "backend_type": "ollama"}
	]}`)
	if err == nil {
		t.Fatal("expected startup error for name collision with API-sourced node")
	}
	if !strings.Contains(err.Error(), "shared") {
		t.Errorf("collision error should name the node, got: %v", err)
	}

	// Collision is detected before any mutation: no partial apply.
	byName := nodesByName(t, s)
	if _, ok := byName["brand-new"]; ok {
		t.Error("no nodes may be created when the sync fails on a collision")
	}
	if byName["shared"].BaseURL != "http://api-host:11434" {
		t.Errorf("API node must be unchanged, got %q", byName["shared"].BaseURL)
	}
}

// ---------------------------------------------------------------------------
// Merge rule: idempotent re-run
// ---------------------------------------------------------------------------

func TestSync_IdempotentRerun(t *testing.T) {
	s := setupSyncStore(t)
	file := `{"nodes": [
		{"name": "a", "base_url": "http://h1:1", "backend_type": "ollama"},
		{"name": "b", "base_url": "http://h2:1", "backend_type": "openai_compat",
		 "auth_header": "Bearer sk-fixed-1234567890", "static_models": ["m1", "m2"]}
	]}`
	mustSyncFile(t, s, file)
	first := nodesByName(t, s)
	mustSyncFile(t, s, file)
	second := nodesByName(t, s)

	if len(second) != 2 {
		t.Fatalf("re-run changed node count: %d", len(second))
	}
	for name, f := range first {
		g := second[name]
		if g.ID != f.ID {
			t.Errorf("%s: id changed on re-run (%d -> %d)", name, f.ID, g.ID)
		}
		if g.BaseURL != f.BaseURL || g.BackendType != f.BackendType || g.Enabled != f.Enabled {
			t.Errorf("%s: fields changed on re-run: %+v -> %+v", name, f, g)
		}
		if (g.AuthHeader == nil) != (f.AuthHeader == nil) ||
			(g.AuthHeader != nil && *g.AuthHeader != *f.AuthHeader) {
			t.Errorf("%s: auth_header changed on re-run", name)
		}
	}
}

// ---------------------------------------------------------------------------
// OLLAMA_URL synthesis merges exactly like a file node
// ---------------------------------------------------------------------------

func TestSync_OllamaURLCreatesDefaultNode(t *testing.T) {
	s := setupSyncStore(t)
	err := SyncDeclaredNodes(context.Background(), s, config.Config{
		OllamaURL: "http://localhost:11434", OllamaURLSet: true,
	})
	if err != nil {
		t.Fatalf("SyncDeclaredNodes: %v", err)
	}

	def := nodesByName(t, s)["default"]
	if def.Name != "default" || def.BaseURL != "http://localhost:11434" || def.BackendType != "ollama" {
		t.Errorf("unexpected default node: %+v", def)
	}
	if def.Source != "config" || !def.Enabled {
		t.Errorf("default node must be enabled config-sourced: %+v", def)
	}
}

func TestSync_UnsetOllamaURLNoSynthesisZeroNodes(t *testing.T) {
	s := setupSyncStore(t)
	err := SyncDeclaredNodes(context.Background(), s, config.Config{OllamaURLSet: false})
	if err != nil {
		t.Fatalf("SyncDeclaredNodes: %v", err)
	}
	if n := nodesByName(t, s); len(n) != 0 {
		t.Errorf("fresh install with no sources must have zero nodes, got %d", len(n))
	}
}

func TestSync_RemovingOllamaURLDisablesDefaultNode(t *testing.T) {
	s := setupSyncStore(t)
	if err := SyncDeclaredNodes(context.Background(), s, config.Config{
		OllamaURL: "http://localhost:11434", OllamaURLSet: true,
	}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Restart without OLLAMA_URL: the synthesized node is config-sourced and
	// now absent from the declared set, so it is disabled like any file node.
	if err := SyncDeclaredNodes(context.Background(), s, config.Config{OllamaURLSet: false}); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if nodesByName(t, s)["default"].Enabled {
		t.Error("default node should be disabled once OLLAMA_URL is removed")
	}
}

func TestSync_OllamaURLCollidesWithAPIDefaultNode(t *testing.T) {
	s := setupSyncStore(t)
	if _, err := s.CreateNode(store.Node{
		Name: "default", BaseURL: "http://api-host:11434", BackendType: "ollama",
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	err := SyncDeclaredNodes(context.Background(), s, config.Config{
		OllamaURL: "http://localhost:11434", OllamaURLSet: true,
	})
	if err == nil {
		t.Fatal("expected collision error: synthesized 'default' vs API-sourced 'default'")
	}
}

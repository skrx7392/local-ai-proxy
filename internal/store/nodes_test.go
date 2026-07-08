package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Pure validation tests (no database required)
// ---------------------------------------------------------------------------

func TestCanonicalizeBaseURL_Valid(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://100.101.2.3:11434", "http://100.101.2.3:11434"},
		{"https://api.example.com", "https://api.example.com"},
		{"https://host/openai", "https://host/openai"},
		{"http://host:11434/", "http://host:11434"},
		{"https://host/openai/", "https://host/openai"},
		{"https://host/openai///", "https://host/openai"},
		{"HTTP://Host:11434", "http://host:11434"},
		{"  http://host:11434  ", "http://host:11434"},
		{"http://ollama.models.svc.cluster.local:11434", "http://ollama.models.svc.cluster.local:11434"},
	}
	for _, c := range cases {
		got, err := CanonicalizeBaseURL(c.in)
		if err != nil {
			t.Errorf("CanonicalizeBaseURL(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CanonicalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeBaseURL_Rejects(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"no scheme", "host:11434"},
		{"relative", "not a url"},
		{"ftp scheme", "ftp://host/x"},
		{"file scheme", "file:///etc/passwd"},
		{"empty host", "http://"},
		{"userinfo", "http://user:pass@host:11434"},
		{"query", "http://host/?x=1"},
		{"bare query", "http://host?x=1"},
		{"fragment", "http://host/#frag"},
		{"v1 suffix", "http://host:11434/v1"},
		{"v1 suffix trailing slash", "http://host:11434/v1/"},
		{"v1 suffix with prefix path", "https://host/openai/v1"},
	}
	for _, c := range cases {
		if _, err := CanonicalizeBaseURL(c.in); err == nil {
			t.Errorf("%s: CanonicalizeBaseURL(%q) succeeded, want error", c.name, c.in)
		}
	}
}

func TestCanonicalizeBaseURL_V1HintInError(t *testing.T) {
	_, err := CanonicalizeBaseURL("http://host:11434/v1")
	if err == nil {
		t.Fatal("expected error for /v1 suffix")
	}
	if !strings.Contains(err.Error(), "/v1") {
		t.Errorf("error %q should mention /v1 so the fix is obvious", err)
	}
}

func TestValidateAuthHeader(t *testing.T) {
	valid := []string{
		"Bearer sk-1234567890abcd",
		"Basic dXNlcjpwYXNz",
		"X-Token abc_def-123",
	}
	for _, v := range valid {
		if err := ValidateAuthHeader(v); err != nil {
			t.Errorf("ValidateAuthHeader(%q): unexpected error: %v", v, err)
		}
	}

	invalid := []struct {
		name, in string
	}{
		{"empty", ""},
		{"CRLF injection", "Bearer a\r\nX-Injected: evil"},
		{"bare LF", "Bearer a\nb"},
		{"bare CR", "Bearer a\rb"},
		{"NUL", "Bearer \x00"},
		{"tab", "Bearer a\tb"},
		{"non-ascii masked value", "Bearer sk-…abcd"},
	}
	for _, c := range invalid {
		if err := ValidateAuthHeader(c.in); err == nil {
			t.Errorf("%s: ValidateAuthHeader(%q) succeeded, want error", c.name, c.in)
		}
	}
}

func TestValidateHealthPath(t *testing.T) {
	valid := []string{"/", "/healthz", "/api/health", "/health-check_v2"}
	for _, v := range valid {
		if err := ValidateHealthPath(v); err != nil {
			t.Errorf("ValidateHealthPath(%q): unexpected error: %v", v, err)
		}
	}

	invalid := []struct {
		name, in string
	}{
		{"empty", ""},
		{"no leading slash", "healthz"},
		{"protocol-relative", "//evil.com/x"},
		{"absolute url", "http://evil.com/x"},
		{"https url", "https://evil.com/x"},
		{"query", "/x?y=1"},
		{"bare query", "/x?"},
		{"fragment", "/x#f"},
		{"CRLF", "/x\r\n"},
		{"space", "/x y"},
	}
	for _, c := range invalid {
		if err := ValidateHealthPath(c.in); err == nil {
			t.Errorf("%s: ValidateHealthPath(%q) succeeded, want error", c.name, c.in)
		}
	}
}

func TestMaskAuthHeader(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Long value: prefix + ellipsis + last 4, same spirit as api_keys.key_prefix.
		{"Bearer sk-1234567890abcd", "Bearer sk-…abcd"},
		// Short values reveal nothing.
		{"Bearer x", "…"},
		{"short", "…"},
	}
	for _, c := range cases {
		if got := maskAuthHeader(c.in); got != c.want {
			t.Errorf("maskAuthHeader(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Database-backed CRUD tests
// ---------------------------------------------------------------------------

func TestCreateNode_RoundTrip(t *testing.T) {
	s := setupTestStore(t)

	ah := "Bearer sk-1234567890abcd"
	hp := "/healthz"
	ts := 900
	id, err := s.CreateNode(Node{
		Name:           "m5-max",
		BaseURL:        "http://100.101.2.3:11434",
		BackendType:    "openai_compat",
		AuthHeader:     &ah,
		StaticModels:   []string{"gpt-4o-mini", "qwen3-coder:30b"},
		HealthPath:     &hp,
		TimeoutSeconds: &ts,
		Source:         "config",
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatal("expected node, got nil")
	}
	if got.Name != "m5-max" {
		t.Errorf("Name = %q, want m5-max", got.Name)
	}
	if got.BaseURL != "http://100.101.2.3:11434" {
		t.Errorf("BaseURL = %q", got.BaseURL)
	}
	if got.BackendType != "openai_compat" {
		t.Errorf("BackendType = %q, want openai_compat", got.BackendType)
	}
	if len(got.StaticModels) != 2 || got.StaticModels[0] != "gpt-4o-mini" || got.StaticModels[1] != "qwen3-coder:30b" {
		t.Errorf("StaticModels = %v", got.StaticModels)
	}
	if got.HealthPath == nil || *got.HealthPath != "/healthz" {
		t.Errorf("HealthPath = %v", got.HealthPath)
	}
	if got.TimeoutSeconds == nil || *got.TimeoutSeconds != 900 {
		t.Errorf("TimeoutSeconds = %v", got.TimeoutSeconds)
	}
	if !got.Enabled {
		t.Error("expected Enabled = true")
	}
	if got.Source != "config" {
		t.Errorf("Source = %q, want config", got.Source)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("expected timestamps to be set")
	}
}

func TestCreateNode_Defaults(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateNode(Node{Name: "default-node", BaseURL: "http://localhost:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	got, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.BackendType != "ollama" {
		t.Errorf("BackendType = %q, want default ollama", got.BackendType)
	}
	if got.Source != "api" {
		t.Errorf("Source = %q, want default api", got.Source)
	}
	if !got.Enabled {
		t.Error("expected Enabled = true by default")
	}
	if got.AuthHeader != nil {
		t.Errorf("AuthHeader = %v, want nil", got.AuthHeader)
	}
	if got.StaticModels != nil {
		t.Errorf("StaticModels = %v, want nil (discovery enabled)", got.StaticModels)
	}
	if got.HealthPath != nil || got.TimeoutSeconds != nil {
		t.Errorf("HealthPath/TimeoutSeconds should be nil, got %v/%v", got.HealthPath, got.TimeoutSeconds)
	}
}

func TestCreateNode_CanonicalizesBaseURL(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateNode(Node{Name: "slashy", BaseURL: "http://host:11434/"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	got, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.BaseURL != "http://host:11434" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", got.BaseURL)
	}
}

func TestCreateNode_DuplicateName(t *testing.T) {
	s := setupTestStore(t)

	if _, err := s.CreateNode(Node{Name: "dup", BaseURL: "http://a:11434"}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	_, err := s.CreateNode(Node{Name: "dup", BaseURL: "http://b:11434"})
	if err != ErrNodeNameExists {
		t.Fatalf("expected ErrNodeNameExists, got %v", err)
	}
}

func TestCreateNode_ValidationRejections(t *testing.T) {
	s := setupTestStore(t)

	ahBad := "Bearer a\r\nX: y"
	hpBad := "http://evil.com/x"
	tsBad := 0
	cases := []struct {
		name string
		node Node
	}{
		{"empty name", Node{Name: "", BaseURL: "http://a:11434"}},
		{"blank name", Node{Name: "   ", BaseURL: "http://a:11434"}},
		{"bad base_url scheme", Node{Name: "n1", BaseURL: "ftp://a"}},
		{"base_url v1 suffix", Node{Name: "n2", BaseURL: "http://a:11434/v1"}},
		{"bad backend_type", Node{Name: "n3", BaseURL: "http://a:11434", BackendType: "vllm"}},
		{"bad source", Node{Name: "n4", BaseURL: "http://a:11434", Source: "magic"}},
		{"header injection", Node{Name: "n5", BaseURL: "http://a:11434", AuthHeader: &ahBad}},
		{"bad health_path", Node{Name: "n6", BaseURL: "http://a:11434", HealthPath: &hpBad}},
		{"zero timeout", Node{Name: "n7", BaseURL: "http://a:11434", TimeoutSeconds: &tsBad}},
	}
	for _, c := range cases {
		if _, err := s.CreateNode(c.node); err == nil {
			t.Errorf("%s: CreateNode succeeded, want error", c.name)
		}
	}
}

func TestGetNode_NotFound(t *testing.T) {
	s := setupTestStore(t)

	got, err := s.GetNode(999999)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing node, got %+v", got)
	}
}

func TestUpdateNode(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateNode(Node{Name: "before", BaseURL: "http://a:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	created, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}

	// NOW() is transaction-start time with microsecond resolution; a short
	// sleep guarantees the update lands at a strictly later timestamp.
	time.Sleep(25 * time.Millisecond)

	ah := "Bearer sk-9876543210wxyz"
	ts := 120
	err = s.UpdateNode(Node{
		ID:             id,
		Name:           "after",
		BaseURL:        "https://b.example.com/openai/",
		BackendType:    "openai_compat",
		AuthHeader:     &ah,
		StaticModels:   []string{"gpt-4o-mini"},
		TimeoutSeconds: &ts,
		Enabled:        true,
	})
	if err != nil {
		t.Fatalf("UpdateNode: %v", err)
	}

	got, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.Name != "after" {
		t.Errorf("Name = %q, want after", got.Name)
	}
	if got.BaseURL != "https://b.example.com/openai" {
		t.Errorf("BaseURL = %q, want canonicalized https://b.example.com/openai", got.BaseURL)
	}
	if got.BackendType != "openai_compat" {
		t.Errorf("BackendType = %q", got.BackendType)
	}
	if len(got.StaticModels) != 1 || got.StaticModels[0] != "gpt-4o-mini" {
		t.Errorf("StaticModels = %v", got.StaticModels)
	}
	if got.TimeoutSeconds == nil || *got.TimeoutSeconds != 120 {
		t.Errorf("TimeoutSeconds = %v", got.TimeoutSeconds)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("UpdatedAt %v should be after original %v", got.UpdatedAt, created.UpdatedAt)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt changed on update: %v -> %v", created.CreatedAt, got.CreatedAt)
	}
}

func TestUpdateNode_NotFound(t *testing.T) {
	s := setupTestStore(t)

	err := s.UpdateNode(Node{ID: 999999, Name: "ghost", BaseURL: "http://a:11434", Enabled: true})
	if err != ErrNodeNotFound {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
}

func TestUpdateNode_DuplicateName(t *testing.T) {
	s := setupTestStore(t)

	if _, err := s.CreateNode(Node{Name: "first", BaseURL: "http://a:11434"}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	id2, err := s.CreateNode(Node{Name: "second", BaseURL: "http://b:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	err = s.UpdateNode(Node{ID: id2, Name: "first", BaseURL: "http://b:11434", Enabled: true})
	if err != ErrNodeNameExists {
		t.Fatalf("expected ErrNodeNameExists, got %v", err)
	}
}

func TestDisableNode(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateNode(Node{Name: "victim", BaseURL: "http://a:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	created, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}

	time.Sleep(25 * time.Millisecond)

	if err := s.DisableNode(id); err != nil {
		t.Fatalf("DisableNode: %v", err)
	}

	got, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got == nil {
		t.Fatal("node must still exist after DisableNode (soft delete)")
	}
	if got.Enabled {
		t.Error("expected Enabled = false after DisableNode")
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Errorf("UpdatedAt %v should advance on disable", got.UpdatedAt)
	}
}

func TestDisableNode_NotFound(t *testing.T) {
	s := setupTestStore(t)

	if err := s.DisableNode(999999); err != ErrNodeNotFound {
		t.Fatalf("expected ErrNodeNotFound, got %v", err)
	}
}

func TestListNodes_MasksAuthHeader(t *testing.T) {
	s := setupTestStore(t)

	ah := "Bearer sk-1234567890abcd"
	if _, err := s.CreateNode(Node{Name: "with-auth", BaseURL: "http://a:11434", AuthHeader: &ah}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if _, err := s.CreateNode(Node{Name: "no-auth", BaseURL: "http://b:11434"}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	// Ordered by id.
	if nodes[0].Name != "with-auth" || nodes[1].Name != "no-auth" {
		t.Errorf("unexpected order: %q, %q", nodes[0].Name, nodes[1].Name)
	}
	if nodes[0].AuthHeader == nil {
		t.Fatal("masked AuthHeader should be non-nil when a secret is set")
	}
	if *nodes[0].AuthHeader != "Bearer sk-…abcd" {
		t.Errorf("AuthHeader = %q, want masked \"Bearer sk-…abcd\"", *nodes[0].AuthHeader)
	}
	if strings.Contains(*nodes[0].AuthHeader, "1234567890") {
		t.Error("ListNodes leaked the raw secret")
	}
	if nodes[1].AuthHeader != nil {
		t.Errorf("no-auth node AuthHeader = %v, want nil", nodes[1].AuthHeader)
	}
}

func TestGetNode_MasksAuthHeader(t *testing.T) {
	s := setupTestStore(t)

	ah := "Bearer sk-1234567890abcd"
	id, err := s.CreateNode(Node{Name: "masked", BaseURL: "http://a:11434", AuthHeader: &ah})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	got, err := s.GetNode(id)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.AuthHeader == nil || *got.AuthHeader != "Bearer sk-…abcd" {
		t.Errorf("GetNode AuthHeader = %v, want masked", got.AuthHeader)
	}
}

func TestNodesWithSecrets_ReturnRawAuthHeader(t *testing.T) {
	s := setupTestStore(t)

	ah := "Bearer sk-1234567890abcd"
	id, err := s.CreateNode(Node{Name: "raw", BaseURL: "http://a:11434", AuthHeader: &ah})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	got, err := s.GetNodeWithSecrets(id)
	if err != nil {
		t.Fatalf("GetNodeWithSecrets: %v", err)
	}
	if got == nil || got.AuthHeader == nil || *got.AuthHeader != ah {
		t.Fatalf("GetNodeWithSecrets AuthHeader = %v, want raw value", got.AuthHeader)
	}

	nodes, err := s.ListNodesWithSecrets()
	if err != nil {
		t.Fatalf("ListNodesWithSecrets: %v", err)
	}
	if len(nodes) != 1 || nodes[0].AuthHeader == nil || *nodes[0].AuthHeader != ah {
		t.Fatalf("ListNodesWithSecrets = %+v, want raw auth header", nodes)
	}
}

// ---------------------------------------------------------------------------
// Usage attribution
// ---------------------------------------------------------------------------

func TestLogUsage_WithNodeID(t *testing.T) {
	s := setupTestStore(t)

	keyID, err := s.CreateKey("usage-key", "hash-node-usage", "sk-node", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	nodeID, err := s.CreateNode(Node{Name: "attributed", BaseURL: "http://a:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	err = s.LogUsage(UsageEntry{
		APIKeyID:         keyID,
		Model:            "qwen3-coder:30b",
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
		DurationMs:       100,
		Status:           "completed",
		NodeID:           &nodeID,
	})
	if err != nil {
		t.Fatalf("LogUsage: %v", err)
	}

	var got *int64
	err = s.pool.QueryRow(context.Background(),
		`SELECT node_id FROM usage_logs WHERE api_key_id = $1`, keyID).Scan(&got)
	if err != nil {
		t.Fatalf("query node_id: %v", err)
	}
	if got == nil || *got != nodeID {
		t.Fatalf("node_id = %v, want %d", got, nodeID)
	}
}

func TestLogUsage_WithoutNodeID(t *testing.T) {
	s := setupTestStore(t)

	keyID, err := s.CreateKey("usage-key-nil", "hash-node-nil", "sk-nil", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	err = s.LogUsage(UsageEntry{
		APIKeyID:    keyID,
		Model:       "llama3.2:3b",
		TotalTokens: 5,
		Status:      "completed",
	})
	if err != nil {
		t.Fatalf("LogUsage: %v", err)
	}

	var got *int64
	err = s.pool.QueryRow(context.Background(),
		`SELECT node_id FROM usage_logs WHERE api_key_id = $1`, keyID).Scan(&got)
	if err != nil {
		t.Fatalf("query node_id: %v", err)
	}
	if got != nil {
		t.Fatalf("node_id = %d, want NULL", *got)
	}
}

// ---------------------------------------------------------------------------
// Schema-level guarantees
// ---------------------------------------------------------------------------

func TestNodesSchema_CheckConstraints(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name, sql string
	}{
		{"bad backend_type", `INSERT INTO nodes (name, base_url, backend_type) VALUES ('c1', 'http://a', 'bogus')`},
		{"bad source", `INSERT INTO nodes (name, base_url, source) VALUES ('c2', 'http://a', 'bogus')`},
		{"zero timeout", `INSERT INTO nodes (name, base_url, timeout_seconds) VALUES ('c3', 'http://a', 0)`},
		{"negative timeout", `INSERT INTO nodes (name, base_url, timeout_seconds) VALUES ('c4', 'http://a', -5)`},
	}
	for _, c := range cases {
		if _, err := s.pool.Exec(ctx, c.sql); err == nil {
			t.Errorf("%s: insert succeeded, want CHECK constraint violation", c.name)
		}
	}
}

func TestMigrate_NodesSchemaIdempotent(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping store integration test")
	}
	ctx := context.Background()

	// Applying the schema twice against the same database must be clean:
	// New() runs the full embedded schema.sql on every call.
	s1, err := New(ctx, dbURL)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	s1.Close()

	s2, err := New(ctx, dbURL)
	if err != nil {
		t.Fatalf("second New (re-migration): %v", err)
	}
	t.Cleanup(s2.Close)

	var exists bool
	if err := s2.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'nodes')`).Scan(&exists); err != nil {
		t.Fatalf("query nodes table existence: %v", err)
	}
	if !exists {
		t.Fatal("expected nodes table to exist after migration")
	}

	if err := s2.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		  WHERE table_name = 'usage_logs' AND column_name = 'node_id')`).Scan(&exists); err != nil {
		t.Fatalf("query node_id column existence: %v", err)
	}
	if !exists {
		t.Fatal("expected usage_logs.node_id column to exist after migration")
	}

	if err := s2.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'idx_usage_logs_node_created')`).Scan(&exists); err != nil {
		t.Fatalf("query index existence: %v", err)
	}
	if !exists {
		t.Fatal("expected idx_usage_logs_node_created index to exist after migration")
	}
}

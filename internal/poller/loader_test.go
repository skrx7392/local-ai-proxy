package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func TestLoad_MapsStoreRowsToRegistryNodes(t *testing.T) {
	full := store.Node{
		ID:             1,
		Name:           "mac-studio",
		BaseURL:        "http://100.101.2.3:11434",
		BackendType:    "ollama",
		AuthHeader:     strPtr("Bearer raw-secret"),
		TimeoutSeconds: intPtr(600),
		Enabled:        true,
	}
	disabled := store.Node{
		ID:          2,
		Name:        "retired",
		BaseURL:     "http://gone:11434",
		BackendType: "ollama",
		Enabled:     false,
	}
	minimal := store.Node{
		ID:          3,
		Name:        "cloud",
		BaseURL:     "https://api.example.com/openai",
		BackendType: "openai_compat",
		Enabled:     true,
	}

	p, reg, _ := newTestPoller(t, []store.Node{full, disabled, minimal}, Options{})
	specs, err := p.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2 (disabled node excluded)", len(specs))
	}

	snap := reg.Snapshot()
	if len(snap.Nodes) != 2 {
		t.Fatalf("registry has %d nodes, want 2", len(snap.Nodes))
	}

	n1 := nodeState(t, reg, 1).Node
	if n1.Name != "mac-studio" {
		t.Errorf("Name = %q, want mac-studio", n1.Name)
	}
	if n1.BaseURL == nil || n1.BaseURL.String() != "http://100.101.2.3:11434" {
		t.Errorf("BaseURL = %v, want http://100.101.2.3:11434", n1.BaseURL)
	}
	if n1.AuthHeader != "Bearer raw-secret" {
		t.Errorf("AuthHeader = %q, want raw secret", n1.AuthHeader)
	}
	if n1.Timeout != 600*time.Second {
		t.Errorf("Timeout = %v, want 600s", n1.Timeout)
	}

	n3 := nodeState(t, reg, 3).Node
	if n3.BaseURL == nil || n3.BaseURL.String() != "https://api.example.com/openai" {
		t.Errorf("BaseURL = %v, want https://api.example.com/openai", n3.BaseURL)
	}
	if n3.AuthHeader != "" {
		t.Errorf("AuthHeader = %q, want empty", n3.AuthHeader)
	}
	if n3.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (default)", n3.Timeout)
	}

	for _, ns := range snap.Nodes {
		if ns.Node.ID == 2 {
			t.Error("disabled node 2 must not be loaded into the registry")
		}
	}
}

// A reload picks up node changes (BE-7 admin edits) without a restart, and a
// node whose BaseURL changed loses its runtime state (poller + registry).
func TestLoad_ReloadPicksUpChangesAndResetsOnBaseURLChange(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, src := newTestPoller(t, []store.Node{enabledNode(1, "n1", f.srv.URL, "ollama")}, Options{})

	pollOnce(t, p)
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Fatalf("setup: Health = %q, want healthy", ns.Health)
	}

	// Add a node and change node 1's BaseURL: 1 resets to unknown, 2 appears.
	src.set([]store.Node{
		enabledNode(1, "n1", "http://moved:11434", "ollama"),
		enabledNode(2, "n2", f.srv.URL, "ollama"),
	}, nil)
	if _, err := p.load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	snap := reg.Snapshot()
	if len(snap.Nodes) != 2 {
		t.Fatalf("registry has %d nodes, want 2 after reload", len(snap.Nodes))
	}
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthUnknown {
		t.Errorf("node 1 Health = %q, want unknown after BaseURL change", ns.Health)
	}

	// The poller's own hysteresis state must reset too: one failure on the
	// moved node keeps it unknown (not an immediate unhealthy carried over
	// from stale counters), and its model list is no longer trusted.
	if ns := nodeState(t, reg, 1); ns.Models != nil {
		t.Errorf("node 1 Models = %v, want nil after BaseURL change", ns.Models)
	}
}

// A transient reload failure must not stop health probing: the cycle falls
// back to the last successfully loaded node set, otherwise routing would
// trust stale registry state until the DB recovers.
func TestPollCycle_ReloadFailureKeepsProbingLastNodes(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"models":[{"name":"m"}]}`))
	}))
	defer srv.Close()

	p, reg, src := newTestPoller(t, []store.Node{enabledNode(1, "n1", srv.URL, "ollama")}, Options{})

	// Load succeeds once (no probe yet), then the source starts failing.
	if _, err := p.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	src.set(nil, context.DeadlineExceeded)

	p.pollCycle(context.Background())

	if got := hits.Load(); got != 1 {
		t.Errorf("backend probed %d times, want 1 (cycle must fall back to cached node set)", got)
	}
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy despite reload failure", ns.Health)
	}
}

// A node whose probe configuration changed without a BaseURL change (new
// static_models list, backend_type flip, static_models removed) is a
// different probing target: its carried-over health/models must reset so a
// removed model can never stay routable on stale state.
func TestLoad_ResetsStateWhenProbeConfigChanges(t *testing.T) {
	discovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"discovered"}]}`))
	}))
	defer discovery.Close()

	staticNode := func(models []string) store.Node {
		n := enabledNode(1, "n1", discovery.URL, "ollama")
		n.StaticModels = models
		return n
	}

	t.Run("static_models replaced", func(t *testing.T) {
		p, reg, src := newTestPoller(t, []store.Node{staticNode([]string{"old-model"})}, Options{})
		pollOnce(t, p)
		if _, err := reg.Resolve("old-model"); err != nil {
			t.Fatalf("setup: old-model not routable: %v", err)
		}

		src.set([]store.Node{staticNode([]string{"new-model"})}, nil)
		if _, err := p.load(); err != nil {
			t.Fatalf("load: %v", err)
		}

		// Immediately after the reload — before any probe — the stale
		// static model must no longer be routable.
		if _, err := reg.Resolve("old-model"); err == nil {
			t.Error("old-model still routable after static_models changed")
		}
		if ns := nodeState(t, reg, 1); ns.Health != registry.HealthUnknown {
			t.Errorf("Health = %q, want unknown after config change", ns.Health)
		}

		// The node is due immediately and the next probe publishes the new list.
		pollOnce(t, p)
		if _, err := reg.Resolve("new-model"); err != nil {
			t.Errorf("new-model not routable after reprobe: %v", err)
		}
	})

	t.Run("backend_type changed", func(t *testing.T) {
		p, reg, src := newTestPoller(t, []store.Node{enabledNode(1, "n1", discovery.URL, "ollama")}, Options{})
		pollOnce(t, p)

		changed := enabledNode(1, "n1", discovery.URL, "openai_compat")
		src.set([]store.Node{changed}, nil)
		if _, err := p.load(); err != nil {
			t.Fatalf("load: %v", err)
		}

		ns := nodeState(t, reg, 1)
		if ns.Health != registry.HealthUnknown {
			t.Errorf("Health = %q, want unknown after backend_type change", ns.Health)
		}
		if ns.Models != nil {
			t.Errorf("Models = %v, want nil after backend_type change", ns.Models)
		}
	})

	t.Run("static_models removed", func(t *testing.T) {
		p, reg, src := newTestPoller(t, []store.Node{staticNode([]string{"pinned"})}, Options{})
		pollOnce(t, p)
		if _, err := reg.Resolve("pinned"); err != nil {
			t.Fatalf("setup: pinned not routable: %v", err)
		}

		src.set([]store.Node{enabledNode(1, "n1", discovery.URL, "ollama")}, nil)
		if _, err := p.load(); err != nil {
			t.Fatalf("load: %v", err)
		}

		if _, err := reg.Resolve("pinned"); err == nil {
			t.Error("pinned still routable after static_models removed")
		}

		pollOnce(t, p)
		if _, err := reg.Resolve("discovered"); err != nil {
			t.Errorf("discovered model not routable after switch to discovery: %v", err)
		}
	})
}

func TestLoad_SourceErrorLeavesRegistryUntouched(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, src := newTestPoller(t, []store.Node{enabledNode(1, "n1", f.srv.URL, "ollama")}, Options{})
	pollOnce(t, p)

	src.set(nil, context.DeadlineExceeded)
	if _, err := p.load(); err == nil {
		t.Fatal("load = nil error, want source error")
	}

	// Registry still has the node with its last-known state.
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy preserved across a failed reload", ns.Health)
	}
}

// TestLoad_DBIntegration exercises the loader against real store rows in
// Postgres (skipped without DATABASE_URL, matching the store test suite).
func TestLoad_DBIntegration(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping DB integration test")
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

	fullID, err := s.CreateNode(store.Node{
		Name:           "db-full",
		BaseURL:        "http://100.64.0.7:11434/",
		BackendType:    "ollama",
		AuthHeader:     strPtr("Bearer db-secret"),
		StaticModels:   []string{"pinned:latest"},
		HealthPath:     strPtr("/healthz"),
		TimeoutSeconds: intPtr(120),
	})
	if err != nil {
		t.Fatalf("CreateNode full: %v", err)
	}
	minID, err := s.CreateNode(store.Node{
		Name:        "db-min",
		BaseURL:     "https://API.example.com/openai",
		BackendType: "openai_compat",
	})
	if err != nil {
		t.Fatalf("CreateNode minimal: %v", err)
	}
	offID, err := s.CreateNode(store.Node{
		Name:    "db-disabled",
		BaseURL: "http://gone:11434",
	})
	if err != nil {
		t.Fatalf("CreateNode disabled: %v", err)
	}
	if err := s.DisableNode(offID); err != nil {
		t.Fatalf("DisableNode: %v", err)
	}

	reg := registry.New()
	p := New(s, reg, nil, Options{})
	specs, err := p.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}

	snap := reg.Snapshot()
	if len(snap.Nodes) != 2 {
		t.Fatalf("registry has %d nodes, want 2", len(snap.Nodes))
	}

	full := nodeState(t, reg, fullID).Node
	// Canonicalization happened at write: trailing slash trimmed.
	if full.BaseURL.String() != "http://100.64.0.7:11434" {
		t.Errorf("full BaseURL = %q, want canonical http://100.64.0.7:11434", full.BaseURL)
	}
	if full.AuthHeader != "Bearer db-secret" {
		t.Errorf("full AuthHeader = %q, want the RAW secret (loader must use ListNodesWithSecrets)", full.AuthHeader)
	}
	if full.Timeout != 120*time.Second {
		t.Errorf("full Timeout = %v, want 120s", full.Timeout)
	}

	min := nodeState(t, reg, minID).Node
	// Canonicalization lowercases the host.
	if min.BaseURL.String() != "https://api.example.com/openai" {
		t.Errorf("min BaseURL = %q, want https://api.example.com/openai", min.BaseURL)
	}
	if min.AuthHeader != "" {
		t.Errorf("min AuthHeader = %q, want empty", min.AuthHeader)
	}
}

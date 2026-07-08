package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/registry"
)

type mockPinger struct {
	err error
}

func (m *mockPinger) Ping(ctx context.Context) error {
	return m.err
}

// stubNodes is a NodeSnapshotter returning a fixed registry snapshot.
type stubNodes struct {
	snap registry.RegistrySnapshot
}

func (s stubNodes) Snapshot() registry.RegistrySnapshot { return s.snap }

// nodesWithHealth builds a snapshot with one node per given health state.
func nodesWithHealth(healths ...registry.Health) stubNodes {
	snap := registry.RegistrySnapshot{Models: map[string][]registry.Node{}}
	for i, h := range healths {
		snap.Nodes = append(snap.Nodes, registry.NodeState{
			Node:   registry.Node{ID: int64(i + 1)},
			Health: h,
		})
	}
	return stubNodes{snap: snap}
}

func TestLiveHandler_AlwaysOK(t *testing.T) {
	c := NewChecker(nil, nil, nil, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/live", nil)

	c.LiveHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status 'ok', got %v", body["status"])
	}
}

func TestReadyHandler_AllHealthy(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		nodesWithHealth(registry.HealthHealthy),
		func() int { return 5 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("expected status 'ready', got %v", body["status"])
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatal("expected 'checks' object")
	}
	dbCheck := checks["database"].(map[string]any)
	if dbCheck["status"] != "ok" {
		t.Errorf("expected database 'ok', got %v", dbCheck["status"])
	}
	nodesCheck := checks["nodes"].(map[string]any)
	if nodesCheck["status"] != "ok" {
		t.Errorf("expected nodes 'ok', got %v", nodesCheck["status"])
	}
	usageCheck := checks["usage_writer"].(map[string]any)
	if usageCheck["status"] != "ok" {
		t.Errorf("expected usage_writer 'ok', got %v", usageCheck["status"])
	}
}

func TestReadyHandler_DBDown(t *testing.T) {
	c := NewChecker(
		&mockPinger{err: errors.New("connection refused")},
		nodesWithHealth(registry.HealthHealthy),
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" {
		t.Errorf("expected 'not_ready', got %v", body["status"])
	}
	checks := body["checks"].(map[string]any)
	dbCheck := checks["database"].(map[string]any)
	if dbCheck["status"] != "error" {
		t.Errorf("expected database 'error', got %v", dbCheck["status"])
	}
}

// Zero enabled nodes is READY: a fresh install must be able to serve the
// admin API to register its first node.
func TestReadyHandler_ZeroNodes_Ready(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		nodesWithHealth(), // empty snapshot
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for zero configured nodes, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Errorf("expected 'ready', got %v", body["status"])
	}
	checks := body["checks"].(map[string]any)
	nodesCheck := checks["nodes"].(map[string]any)
	if nodesCheck["status"] != "ok" {
		t.Errorf("expected nodes 'ok' with zero nodes, got %v", nodesCheck["status"])
	}
	if nodesCheck["detail"] != "no nodes configured" {
		t.Errorf("expected 'no nodes configured' detail, got %v", nodesCheck["detail"])
	}
}

func TestReadyHandler_AllNodesDown_NotReady(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		nodesWithHealth(registry.HealthUnhealthy, registry.HealthUnhealthy),
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when all nodes are down, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "not_ready" {
		t.Errorf("expected 'not_ready', got %v", body["status"])
	}
	checks := body["checks"].(map[string]any)
	nodesCheck := checks["nodes"].(map[string]any)
	if nodesCheck["status"] != "error" {
		t.Errorf("expected nodes 'error', got %v", nodesCheck["status"])
	}
}

func TestReadyHandler_OneHealthyNode_Ready(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		nodesWithHealth(registry.HealthUnhealthy, registry.HealthHealthy, registry.HealthUnknown),
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 with one healthy node, got %d", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["status"] != "ready" {
		t.Errorf("expected 'ready', got %v", body["status"])
	}
}

// Unprobed (unknown) nodes are treated identically to unhealthy ones: not
// counted toward readiness.
func TestReadyHandler_UnknownOnly_NotReady(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		nodesWithHealth(registry.HealthUnknown),
		func() int { return 0 },
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the only node is unprobed, got %d", rec.Code)
	}
}

func TestReadyHandler_UsageChannelFull(t *testing.T) {
	c := NewChecker(
		&mockPinger{},
		nodesWithHealth(registry.HealthHealthy),
		func() int { return 1000 }, // full
		1000,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/healthz/ready", nil)

	c.ReadyHandler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}

	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	checks := body["checks"].(map[string]any)
	usageCheck := checks["usage_writer"].(map[string]any)
	if usageCheck["status"] != "error" {
		t.Errorf("expected usage_writer 'error', got %v", usageCheck["status"])
	}
}

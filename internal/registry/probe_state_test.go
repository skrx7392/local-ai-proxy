package registry

import (
	"net/url"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func findNode(t *testing.T, snap RegistrySnapshot, id int64) NodeState {
	t.Helper()
	for _, ns := range snap.Nodes {
		if ns.Node.ID == id {
			return ns
		}
	}
	t.Fatalf("node %d not in snapshot", id)
	return NodeState{}
}

func TestSetNodeProbe_PublishesProbeMetadata(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	checked := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	r.SetNodeProbe(1, ProbeResult{
		Health:        HealthUnhealthy,
		Models:        nil,
		LastError:     "connection refused",
		LastCheckedAt: checked,
	})

	ns := findNode(t, r.Snapshot(), 1)
	if ns.Health != HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy", ns.Health)
	}
	if ns.LastError != "connection refused" {
		t.Errorf("LastError = %q, want %q", ns.LastError, "connection refused")
	}
	if !ns.LastCheckedAt.Equal(checked) {
		t.Errorf("LastCheckedAt = %v, want %v", ns.LastCheckedAt, checked)
	}
	if ns.Models != nil {
		t.Errorf("Models = %v, want nil", ns.Models)
	}
}

func TestSetNodeProbe_SuccessClearsLastError(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	r.SetNodeProbe(1, ProbeResult{Health: HealthUnhealthy, LastError: "boom", LastCheckedAt: time.Now()})
	checked := time.Now().Add(time.Minute)
	r.SetNodeProbe(1, ProbeResult{Health: HealthHealthy, Models: []string{"m1"}, LastCheckedAt: checked})

	ns := findNode(t, r.Snapshot(), 1)
	if ns.LastError != "" {
		t.Errorf("LastError = %q, want empty after success", ns.LastError)
	}
	if ns.Health != HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if len(ns.Models) != 1 || ns.Models[0] != "m1" {
		t.Errorf("Models = %v, want [m1]", ns.Models)
	}
}

func TestSetNodeProbe_UnknownIDIsNoOp(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	r.SetNodeProbe(99, ProbeResult{Health: HealthHealthy, Models: []string{"m"}, LastCheckedAt: time.Now()})

	snap := r.Snapshot()
	if len(snap.Nodes) != 1 {
		t.Fatalf("len(Nodes) = %d, want 1", len(snap.Nodes))
	}
	if len(snap.Models) != 0 {
		t.Errorf("Models map = %v, want empty", snap.Models)
	}
}

func TestSetNodeProbe_PreservesEmptyVsNilModels(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	r.SetNodeProbe(1, ProbeResult{Health: HealthHealthy, Models: []string{}, LastCheckedAt: time.Now()})

	ns := findNode(t, r.Snapshot(), 1)
	if ns.Models == nil {
		t.Error("Models is nil, want non-nil empty (probed with zero models)")
	}
	if len(ns.Models) != 0 {
		t.Errorf("Models = %v, want empty", ns.Models)
	}
}

func TestSetNodes_CarriesOverProbeMetadata(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	checked := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	r.SetNodeProbe(1, ProbeResult{Health: HealthUnhealthy, LastError: "timeout", LastCheckedAt: checked})

	// Same ID, same BaseURL: runtime state including probe metadata carries over.
	r.SetNodes([]Node{{ID: 1, Name: "a-renamed", BaseURL: mustParse(t, "http://a:11434")}})
	ns := findNode(t, r.Snapshot(), 1)
	if ns.LastError != "timeout" || !ns.LastCheckedAt.Equal(checked) {
		t.Errorf("probe metadata not carried over: LastError=%q LastCheckedAt=%v", ns.LastError, ns.LastCheckedAt)
	}
	if ns.Health != HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy carried over", ns.Health)
	}

	// Changed BaseURL: everything resets, including probe metadata.
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://other:11434")}})
	ns = findNode(t, r.Snapshot(), 1)
	if ns.Health != HealthUnknown {
		t.Errorf("Health = %q, want unknown after BaseURL change", ns.Health)
	}
	if ns.LastError != "" || !ns.LastCheckedAt.IsZero() {
		t.Errorf("probe metadata not reset: LastError=%q LastCheckedAt=%v", ns.LastError, ns.LastCheckedAt)
	}
}

func TestSetNodeState_LeavesProbeMetadataUntouched(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	checked := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	r.SetNodeProbe(1, ProbeResult{Health: HealthUnhealthy, LastError: "boom", LastCheckedAt: checked})

	r.SetNodeState(1, HealthHealthy, []string{"m"})

	ns := findNode(t, r.Snapshot(), 1)
	if ns.Health != HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if ns.LastError != "boom" || !ns.LastCheckedAt.Equal(checked) {
		t.Errorf("SetNodeState must not touch probe metadata: LastError=%q LastCheckedAt=%v", ns.LastError, ns.LastCheckedAt)
	}
}

func TestSetNodeProbe_RoutableAfterHealthyProbe(t *testing.T) {
	r := New()
	r.SetNodes([]Node{{ID: 1, Name: "a", BaseURL: mustParse(t, "http://a:11434")}})

	r.SetNodeProbe(1, ProbeResult{Health: HealthHealthy, Models: []string{"llama3:8b"}, LastCheckedAt: time.Now()})

	n, err := r.Resolve("llama3:8b")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n.ID != 1 {
		t.Errorf("Resolve returned node %d, want 1", n.ID)
	}
}

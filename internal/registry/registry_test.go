package registry

import (
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"
)

func testNode(t *testing.T, id int64, name, rawURL string) Node {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return Node{ID: id, Name: name, BaseURL: u}
}

// twoHealthyNodes builds a registry with nodes 1 and 2 both healthy and
// serving "shared-model"; node 1 additionally serves "solo-model".
func twoHealthyNodes(t *testing.T) *Registry {
	t.Helper()
	r := New()
	r.SetNodes([]Node{
		testNode(t, 1, "alpha", "http://alpha:11434"),
		testNode(t, 2, "beta", "http://beta:11434"),
	})
	r.SetNodeState(1, HealthHealthy, []string{"shared-model", "solo-model"})
	r.SetNodeState(2, HealthHealthy, []string{"shared-model"})
	return r
}

func TestResolve_EmptyRegistry(t *testing.T) {
	r := New()
	_, err := r.Resolve("any-model")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("want ErrModelUnavailable, got %v", err)
	}
}

func TestResolve_HealthyNodeHit(t *testing.T) {
	r := New()
	n := testNode(t, 7, "gpu-1", "http://gpu-1:11434")
	n.AuthHeader = "Bearer secret"
	n.Timeout = 900 * time.Second
	r.SetNodes([]Node{n})
	r.SetNodeState(7, HealthHealthy, []string{"llama3.2:3b"})

	got, err := r.Resolve("llama3.2:3b")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != 7 || got.Name != "gpu-1" {
		t.Errorf("got node %d %q, want 7 %q", got.ID, got.Name, "gpu-1")
	}
	if got.BaseURL == nil || got.BaseURL.String() != "http://gpu-1:11434" {
		t.Errorf("got BaseURL %v, want http://gpu-1:11434", got.BaseURL)
	}
	if got.AuthHeader != "Bearer secret" {
		t.Errorf("got AuthHeader %q, want %q", got.AuthHeader, "Bearer secret")
	}
	if got.Timeout != 900*time.Second {
		t.Errorf("got Timeout %v, want %v", got.Timeout, 900*time.Second)
	}
}

func TestResolve_UnknownModel(t *testing.T) {
	r := twoHealthyNodes(t)
	_, err := r.Resolve("no-such-model")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("want ErrModelUnavailable, got %v", err)
	}
}

func TestResolve_NonRoutableStates(t *testing.T) {
	tests := []struct {
		name   string
		health Health
		models []string
	}{
		{"unhealthy node with known models", HealthUnhealthy, []string{"m"}},
		{"unknown-health node with known models", HealthUnknown, []string{"m"}},
		{"healthy node with unknown model list", HealthHealthy, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			r.SetNodes([]Node{testNode(t, 1, "n1", "http://n1:11434")})
			r.SetNodeState(1, tt.health, tt.models)
			_, err := r.Resolve("m")
			if !errors.Is(err, ErrModelUnavailable) {
				t.Fatalf("want ErrModelUnavailable, got %v", err)
			}
		})
	}
}

func TestResolve_NeverProbedNodeNotRoutable(t *testing.T) {
	// A node registered via SetNodes but never probed must not receive
	// traffic (no optimistic routing).
	r := New()
	r.SetNodes([]Node{testNode(t, 1, "n1", "http://n1:11434")})
	_, err := r.Resolve("m")
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("want ErrModelUnavailable, got %v", err)
	}
}

func TestResolve_RoundRobin(t *testing.T) {
	r := twoHealthyNodes(t)

	// Candidates are ordered by node ID, and the counter starts at zero,
	// so the sequence over six calls is deterministic: 1,2,1,2,1,2.
	want := []int64{1, 2, 1, 2, 1, 2}
	var got []int64
	for i := 0; i < 6; i++ {
		n, err := r.Resolve("shared-model")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		got = append(got, n.ID)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin sequence = %v, want %v", got, want)
		}
	}
}

func TestResolve_RoundRobinSkipsUnhealthy(t *testing.T) {
	r := twoHealthyNodes(t)
	r.SetNodeState(1, HealthUnhealthy, []string{"shared-model", "solo-model"})

	for i := 0; i < 4; i++ {
		n, err := r.Resolve("shared-model")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if n.ID != 2 {
			t.Fatalf("call %d resolved node %d, want only healthy node 2", i, n.ID)
		}
	}
}

func TestResolve_CountersSurviveRepublish(t *testing.T) {
	r := twoHealthyNodes(t)

	// Advance the counter to 3: sequence 1, 2, 1.
	for i, want := range []int64{1, 2, 1} {
		n, err := r.Resolve("shared-model")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if n.ID != want {
			t.Fatalf("call %d resolved %d, want %d", i, n.ID, want)
		}
	}

	// Republish the same state (as the poller does every cycle). If the
	// counter were rebuilt with the snapshot, the next pick would restart
	// at node 1 instead of continuing at node 2.
	r.SetNodeState(1, HealthHealthy, []string{"shared-model", "solo-model"})
	r.SetNodeState(2, HealthHealthy, []string{"shared-model"})

	n, err := r.Resolve("shared-model")
	if err != nil {
		t.Fatalf("Resolve after republish: %v", err)
	}
	if n.ID != 2 {
		t.Fatalf("resolved node %d after republish, want 2 (balancing continuity)", n.ID)
	}
}

func TestCounterPrunedWhenModelDisappears(t *testing.T) {
	r := twoHealthyNodes(t)

	// Create counter state for shared-model (two candidates -> counter used).
	if _, err := r.Resolve("shared-model"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := r.Resolve("shared-model"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	r.mu.Lock()
	_, exists := r.counters["shared-model"]
	r.mu.Unlock()
	if !exists {
		t.Fatal("expected a counter for shared-model after resolving it")
	}

	// Both nodes stop serving shared-model; the republish must prune its counter.
	r.SetNodeState(1, HealthHealthy, []string{"solo-model"})
	r.SetNodeState(2, HealthHealthy, []string{"other-model"})

	r.mu.Lock()
	_, exists = r.counters["shared-model"]
	r.mu.Unlock()
	if exists {
		t.Fatal("counter for disappeared model was not pruned on republish")
	}
	if _, err := r.Resolve("shared-model"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatal("shared-model should no longer resolve")
	}
}

func TestResolve_NoCounterGrowthForUnknownModels(t *testing.T) {
	r := twoHealthyNodes(t)

	// A flood of arbitrary client-supplied model names must not allocate
	// counter state.
	for i := 0; i < 1000; i++ {
		model := "bogus-" + string(rune('a'+i%26)) + "-model"
		if _, err := r.Resolve(model); !errors.Is(err, ErrModelUnavailable) {
			t.Fatalf("want ErrModelUnavailable for %q, got %v", model, err)
		}
	}

	r.mu.Lock()
	size := len(r.counters)
	for model := range r.counters {
		if model != "shared-model" && model != "solo-model" {
			t.Errorf("counter allocated for non-routable model %q", model)
		}
	}
	r.mu.Unlock()
	if size > 2 {
		t.Fatalf("counter map grew to %d entries from unknown-model floods", size)
	}
}

func TestSetNodes_RemovedNodeNotRoutable(t *testing.T) {
	r := twoHealthyNodes(t)

	// Drop node 1; solo-model loses its only server.
	r.SetNodes([]Node{testNode(t, 2, "beta", "http://beta:11434")})

	if _, err := r.Resolve("solo-model"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("want ErrModelUnavailable for solo-model, got %v", err)
	}
	n, err := r.Resolve("shared-model")
	if err != nil {
		t.Fatalf("shared-model should still resolve via node 2: %v", err)
	}
	if n.ID != 2 {
		t.Fatalf("resolved node %d, want 2", n.ID)
	}
}

func TestSetNodes_PreservesRuntimeStateByID(t *testing.T) {
	r := twoHealthyNodes(t)

	// Re-declare the same nodes (e.g. config reload with a renamed node).
	renamed := testNode(t, 1, "alpha-renamed", "http://alpha:11434")
	r.SetNodes([]Node{renamed, testNode(t, 2, "beta", "http://beta:11434")})

	n, err := r.Resolve("solo-model")
	if err != nil {
		t.Fatalf("health/models should survive SetNodes for unchanged BaseURL: %v", err)
	}
	if n.Name != "alpha-renamed" {
		t.Errorf("got Name %q, want config update applied", n.Name)
	}
}

func TestSetNodes_BaseURLChangeResetsHealth(t *testing.T) {
	r := twoHealthyNodes(t)

	// Node 1 moves to a new URL: it is effectively an unprobed backend, so
	// its stale healthy state must not be trusted.
	moved := testNode(t, 1, "alpha", "http://alpha-new:11434")
	r.SetNodes([]Node{moved, testNode(t, 2, "beta", "http://beta:11434")})

	if _, err := r.Resolve("solo-model"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("want ErrModelUnavailable for node with changed BaseURL, got %v", err)
	}
	snap := r.Snapshot()
	for _, ns := range snap.Nodes {
		if ns.Node.ID == 1 && ns.Health != HealthUnknown {
			t.Errorf("node 1 health = %q after BaseURL change, want %q", ns.Health, HealthUnknown)
		}
	}
}

func TestSetNodeState_KnownEmptyModelListStaysNonNil(t *testing.T) {
	// nil means "not yet discovered"; a successfully probed node serving
	// zero models is a known-empty list and must stay distinguishable.
	r := New()
	r.SetNodes([]Node{testNode(t, 1, "idle", "http://idle:11434")})
	r.SetNodeState(1, HealthHealthy, []string{})

	snap := r.Snapshot()
	if snap.Nodes[0].Models == nil {
		t.Fatal("known-empty model list collapsed to nil (looks undiscovered)")
	}
	if len(snap.Nodes[0].Models) != 0 {
		t.Fatalf("got models %v, want empty", snap.Nodes[0].Models)
	}
}

func TestSetNodeState_UnknownNodeIgnored(t *testing.T) {
	r := twoHealthyNodes(t)
	r.SetNodeState(99, HealthHealthy, []string{"ghost-model"})

	if _, err := r.Resolve("ghost-model"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatal("state for an unregistered node ID must be ignored")
	}
	if len(r.Snapshot().Nodes) != 2 {
		t.Fatalf("snapshot has %d nodes, want 2", len(r.Snapshot().Nodes))
	}
}

func TestSnapshot_Contents(t *testing.T) {
	r := twoHealthyNodes(t)
	r.SetNodes([]Node{
		testNode(t, 1, "alpha", "http://alpha:11434"),
		testNode(t, 2, "beta", "http://beta:11434"),
		testNode(t, 3, "gamma", "http://gamma:11434"),
	})
	r.SetNodeState(3, HealthUnhealthy, nil)

	snap := r.Snapshot()
	if len(snap.Nodes) != 3 {
		t.Fatalf("snapshot has %d nodes, want 3", len(snap.Nodes))
	}
	// Nodes are ordered by ID.
	wantHealth := map[int64]Health{1: HealthHealthy, 2: HealthHealthy, 3: HealthUnhealthy}
	for i, ns := range snap.Nodes {
		if ns.Node.ID != int64(i+1) {
			t.Errorf("snapshot.Nodes[%d].Node.ID = %d, want %d", i, ns.Node.ID, i+1)
		}
		if ns.Health != wantHealth[ns.Node.ID] {
			t.Errorf("node %d health = %q, want %q", ns.Node.ID, ns.Health, wantHealth[ns.Node.ID])
		}
	}

	// Routing map: only healthy nodes with known model lists.
	if got := len(snap.Models["shared-model"]); got != 2 {
		t.Errorf("shared-model has %d candidates, want 2", got)
	}
	if got := len(snap.Models["solo-model"]); got != 1 {
		t.Errorf("solo-model has %d candidates, want 1", got)
	}
	if _, ok := snap.Models["missing"]; ok {
		t.Error("unexpected entry for unserved model")
	}

	// Node 1's model list is visible for display.
	if len(snap.Nodes[0].Models) != 2 {
		t.Errorf("node 1 snapshot models = %v, want 2 entries", snap.Nodes[0].Models)
	}
}

func TestSnapshot_CallerMutationIsIsolated(t *testing.T) {
	r := twoHealthyNodes(t)

	snap := r.Snapshot()
	snap.Nodes[0].Health = HealthUnhealthy
	snap.Nodes[0].Models[0] = "clobbered"
	snap.Nodes[0].Node.BaseURL.Host = "evil:1"
	snap.Models["shared-model"][0] = Node{ID: 999}
	delete(snap.Models, "solo-model")

	fresh := r.Snapshot()
	if fresh.Nodes[0].Health != HealthHealthy {
		t.Error("mutating a returned snapshot changed registry health state")
	}
	if fresh.Nodes[0].Models[0] == "clobbered" {
		t.Error("mutating a returned snapshot changed registry model state")
	}
	if fresh.Nodes[0].Node.BaseURL.Host == "evil:1" {
		t.Error("mutating a returned snapshot changed a registry URL")
	}
	if fresh.Models["shared-model"][0].ID == 999 {
		t.Error("mutating a returned routing map changed registry state")
	}
	n, err := r.Resolve("solo-model")
	if err != nil || n.ID != 1 {
		t.Errorf("solo-model no longer resolves after caller map mutation: %v", err)
	}
}

func TestResolve_ReturnedURLIsCopy(t *testing.T) {
	r := twoHealthyNodes(t)

	n, err := r.Resolve("solo-model")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Today's proxy handler mutates upstreamURL.Path; a returned node must
	// not share URL storage with the registry.
	n.BaseURL.Path = "/v1/chat/completions"

	again, err := r.Resolve("solo-model")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if again.BaseURL.Path != "" {
		t.Errorf("registry URL was mutated through a resolved node: path %q", again.BaseURL.Path)
	}
}

func TestConcurrentResolveAndPublish(t *testing.T) {
	r := New()
	nodes := []Node{
		testNode(t, 1, "alpha", "http://alpha:11434"),
		testNode(t, 2, "beta", "http://beta:11434"),
		testNode(t, 3, "gamma", "http://gamma:11434"),
	}
	r.SetNodes(nodes)
	r.SetNodeState(1, HealthHealthy, []string{"m1", "m2"})
	r.SetNodeState(2, HealthHealthy, []string{"m1"})
	r.SetNodeState(3, HealthHealthy, []string{"m2"})

	stop := make(chan struct{})
	var publishers sync.WaitGroup
	publishers.Add(1)
	go func() {
		defer publishers.Done()
		healths := []Health{HealthHealthy, HealthUnhealthy, HealthUnknown}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r.SetNodeState(int64(i%3+1), healths[i%len(healths)], []string{"m1", "m2"})
			if i%7 == 0 {
				r.SetNodes(nodes)
			}
		}
	}()

	var readers sync.WaitGroup
	for g := 0; g < 8; g++ {
		readers.Add(1)
		go func(g int) {
			defer readers.Done()
			models := []string{"m1", "m2", "no-such-model"}
			for i := 0; i < 2000; i++ {
				model := models[(g+i)%len(models)]
				n, err := r.Resolve(model)
				if err == nil {
					if n.ID < 1 || n.ID > 3 || n.BaseURL == nil {
						t.Errorf("Resolve(%q) returned torn node: %+v", model, n)
					}
				} else if !errors.Is(err, ErrModelUnavailable) {
					t.Errorf("Resolve(%q) unexpected error: %v", model, err)
				}
				if i%50 == 0 {
					snap := r.Snapshot()
					if len(snap.Nodes) != 3 {
						t.Errorf("snapshot has %d nodes, want 3", len(snap.Nodes))
					}
				}
			}
		}(g)
	}

	readers.Wait()
	close(stop)
	publishers.Wait()
}

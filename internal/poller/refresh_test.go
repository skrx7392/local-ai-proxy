package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// RefreshNode is BE-7's synchronous single-node probe: admin handlers call it
// after CreateNode/UpdateNode/DisableNode so the node's live state (and its
// removal from routing) is published before the HTTP response returns.

func TestRefreshNode_ProbesAndPublishesSynchronously(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "n1", f.srv.URL, "ollama")}, Options{})

	if err := p.RefreshNode(context.Background(), 1); err != nil {
		t.Fatalf("RefreshNode: %v", err)
	}

	// The outcome must be visible immediately — no waiting on a poll cycle.
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if len(ns.Models) != 1 || ns.Models[0] != "llama3:8b" {
		t.Errorf("Models = %v, want [llama3:8b]", ns.Models)
	}
	if ns.LastCheckedAt.IsZero() {
		t.Error("LastCheckedAt is zero, want set")
	}
	if _, err := reg.Resolve("llama3:8b"); err != nil {
		t.Errorf("model not routable after RefreshNode: %v", err)
	}
}

func TestRefreshNode_PicksUpNewDBNode(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, src := newTestPoller(t, nil, Options{})
	pollOnce(t, p)

	// Node appears in the DB (admin POST); RefreshNode must reload before
	// probing rather than relying on the poller's periodic re-read.
	src.set([]store.Node{enabledNode(7, "new", f.srv.URL, "ollama")}, nil)
	if err := p.RefreshNode(context.Background(), 7); err != nil {
		t.Fatalf("RefreshNode: %v", err)
	}
	if ns := nodeState(t, reg, 7); ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy for freshly created node", ns.Health)
	}
}

func TestRefreshNode_SingleFailureIsDecisive(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "n1", f.srv.URL, "ollama")}, Options{})
	pollOnce(t, p)
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Fatalf("precondition: Health = %q, want healthy", ns.Health)
	}

	// A forced refresh is a decisive probe (startup-sweep semantics): one
	// failure marks the node unhealthy, no hysteresis.
	f.fail.Store(true)
	if err := p.RefreshNode(context.Background(), 1); err != nil {
		t.Fatalf("RefreshNode: %v", err)
	}
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy after a single forced-probe failure", ns.Health)
	}
	if ns.LastError == "" {
		t.Error("LastError empty, want probe error recorded")
	}
}

func TestRefreshNode_DisabledNodeLeavesRouting(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, src := newTestPoller(t, []store.Node{enabledNode(1, "n1", f.srv.URL, "ollama")}, Options{})
	pollOnce(t, p)
	if _, err := reg.Resolve("llama3:8b"); err != nil {
		t.Fatalf("precondition: model not routable: %v", err)
	}

	// Admin DELETE sets enabled=false, then calls RefreshNode: the reload must
	// drop the node from the registry synchronously.
	n := enabledNode(1, "n1", f.srv.URL, "ollama")
	n.Enabled = false
	src.set([]store.Node{n}, nil)
	if err := p.RefreshNode(context.Background(), 1); err != nil {
		t.Fatalf("RefreshNode on disabled node: %v", err)
	}
	snap := reg.Snapshot()
	if len(snap.Nodes) != 0 {
		t.Errorf("registry still has %d nodes, want 0 after disable+refresh", len(snap.Nodes))
	}
	if _, err := reg.Resolve("llama3:8b"); !errors.Is(err, registry.ErrModelUnavailable) {
		t.Errorf("Resolve after disable = %v, want ErrModelUnavailable", err)
	}
}

func TestRefreshNode_LoadErrorReturned(t *testing.T) {
	f := newFlakyBackend(t)
	p, _, src := newTestPoller(t, []store.Node{enabledNode(1, "n1", f.srv.URL, "ollama")}, Options{})
	src.set(nil, context.DeadlineExceeded)

	if err := p.RefreshNode(context.Background(), 1); err == nil {
		t.Fatal("RefreshNode = nil error, want DB load error surfaced")
	}
}

// RefreshNode must be safe to call concurrently with the running poll loop
// (admin requests land while Run owns the schedule). Run with -race.
func TestRefreshNode_ConcurrentWithRun(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, _ := newTestPoller(t, []store.Node{
		enabledNode(1, "n1", f.srv.URL, "ollama"),
		enabledNode(2, "n2", f.srv.URL, "ollama"),
	}, Options{Interval: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	var runDone sync.WaitGroup
	runDone.Add(1)
	go func() {
		defer runDone.Done()
		p.Run(ctx)
	}()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := p.RefreshNode(context.Background(), id); err != nil {
					t.Errorf("RefreshNode: %v", err)
					return
				}
			}
		}(int64(g%2 + 1))
	}
	wg.Wait()
	cancel()
	runDone.Wait()

	for _, id := range []int64{1, 2} {
		if ns := nodeState(t, reg, id); ns.Health != registry.HealthHealthy {
			t.Errorf("node %d Health = %q, want healthy", id, ns.Health)
		}
	}
}

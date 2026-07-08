package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func TestIntervalFor_JitterBounds(t *testing.T) {
	p, _, _ := newTestPoller(t, nil, Options{}) // defaults: 15s ±20%

	lo := time.Duration(float64(defaultInterval) * (1 - defaultJitterFrac))
	hi := time.Duration(float64(defaultInterval) * (1 + defaultJitterFrac))

	seen := make(map[time.Duration]bool)
	for id := int64(1); id <= 1000; id++ {
		iv := p.intervalFor(id)
		if iv < lo || iv > hi {
			t.Fatalf("intervalFor(%d) = %v, want within [%v, %v]", id, iv, lo, hi)
		}
		seen[iv] = true
	}

	// Jitter must actually spread nodes out, not collapse to one value.
	if len(seen) < 10 {
		t.Errorf("only %d distinct intervals across 1000 node IDs, want a real spread", len(seen))
	}
}

func TestIntervalFor_DeterministicPerNode(t *testing.T) {
	p, _, _ := newTestPoller(t, nil, Options{})
	for id := int64(1); id <= 50; id++ {
		if a, b := p.intervalFor(id), p.intervalFor(id); a != b {
			t.Fatalf("intervalFor(%d) not deterministic: %v vs %v", id, a, b)
		}
	}
}

func TestRun_ReturnsOnContextCancel(t *testing.T) {
	p, _, _ := newTestPoller(t, nil, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// Run's first cycle probes never-before-seen nodes immediately (no initial
// interval wait), so a poller started without a prior sweep still converges.
func TestRun_FirstCycleProbesImmediately(t *testing.T) {
	probed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case probed <- struct{}{}:
		default:
		}
		w.Write([]byte(`{"models":[{"name":"m"}]}`))
	}))
	defer srv.Close()

	// A huge interval proves the probe came from the immediate first cycle.
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "n1", srv.URL, "ollama")},
		Options{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("node was not probed by the first Run cycle")
	}

	// Wait for the probe result to land in the registry before cancelling —
	// cancelling ctx would abort the in-flight probe.
	waitFor(t, 5*time.Second, func() bool {
		for _, ns := range reg.Snapshot().Nodes {
			if ns.Node.ID == 1 && ns.Health == registry.HealthHealthy {
				return true
			}
		}
		return false
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met before deadline")
		case <-tick.C:
		}
	}
}

// A node already probed by the startup sweep is NOT due again on Run's first
// cycle — its next probe is a full jittered interval after the sweep.
func TestRun_DoesNotReprobeFreshlySweptNode(t *testing.T) {
	hits := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	p, _, _ := newTestPoller(t, []store.Node{enabledNode(1, "n1", srv.URL, "ollama")},
		Options{Interval: time.Hour})

	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	<-hits // the sweep's probe

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// Give Run a moment to complete its first cycle, then assert it did not
	// probe the freshly swept node (its interval is an hour).
	select {
	case <-hits:
		t.Error("Run re-probed a node the sweep had just probed")
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	<-done
}

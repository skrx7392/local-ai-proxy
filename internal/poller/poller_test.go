package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fakeSource is an in-memory NodeSource.
type fakeSource struct {
	mu    sync.Mutex
	nodes []store.Node
	err   error
}

func (f *fakeSource) ListNodesWithSecrets() ([]store.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]store.Node, len(f.nodes))
	copy(out, f.nodes)
	return out, nil
}

func (f *fakeSource) set(nodes []store.Node, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = nodes
	f.err = err
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// enabledNode returns a minimal enabled store.Node for tests.
func enabledNode(id int64, name, baseURL, backendType string) store.Node {
	return store.Node{ID: id, Name: name, BaseURL: baseURL, BackendType: backendType, Enabled: true}
}

// newTestPoller builds a poller over a fake source with a fresh registry.
func newTestPoller(t *testing.T, nodes []store.Node, opts Options) (*Poller, *registry.Registry, *fakeSource) {
	t.Helper()
	src := &fakeSource{nodes: nodes}
	reg := registry.New()
	p := New(src, reg, nil, opts)
	return p, reg, src
}

func nodeState(t *testing.T, reg *registry.Registry, id int64) registry.NodeState {
	t.Helper()
	for _, ns := range reg.Snapshot().Nodes {
		if ns.Node.ID == id {
			return ns
		}
	}
	t.Fatalf("node %d not in registry snapshot", id)
	return registry.NodeState{}
}

// ---------------------------------------------------------------------------
// Discovery parsing
// ---------------------------------------------------------------------------

func TestSweepOnce_OllamaDiscovery(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3:8b"},{"name":"qwen3:32b"}]}`))
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "mac", srv.URL, "ollama")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotPath.Load(); got != "/api/tags" {
		t.Errorf("probe path = %v, want /api/tags", got)
	}
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if len(ns.Models) != 2 || ns.Models[0] != "llama3:8b" || ns.Models[1] != "qwen3:32b" {
		t.Errorf("Models = %v, want [llama3:8b qwen3:32b]", ns.Models)
	}
	if ns.LastError != "" {
		t.Errorf("LastError = %q, want empty", ns.LastError)
	}
	if ns.LastCheckedAt.IsZero() {
		t.Error("LastCheckedAt is zero, want set")
	}
}

func TestSweepOnce_OpenAICompatDiscovery(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-oss-20b"},{"id":"qwen3-coder"}]}`))
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "cloud", srv.URL, "openai_compat")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotPath.Load(); got != "/v1/models" {
		t.Errorf("probe path = %v, want /v1/models", got)
	}
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if len(ns.Models) != 2 || ns.Models[0] != "gpt-oss-20b" || ns.Models[1] != "qwen3-coder" {
		t.Errorf("Models = %v, want [gpt-oss-20b qwen3-coder]", ns.Models)
	}
}

// Probe URLs must be built by path-joining onto base_url: a base_url carrying
// a path prefix keeps that prefix.
func TestProbe_PathJoinsBaseURLPrefix(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "prefixed", srv.URL+"/openai", "openai_compat")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotPath.Load(); got != "/openai/v1/models" {
		t.Errorf("probe path = %v, want /openai/v1/models", got)
	}
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
}

// A discovery probe that succeeds with zero models publishes a non-nil empty
// list (probed, nothing served) — distinct from nil (never discovered).
func TestSweepOnce_EmptyDiscoveryIsNonNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "empty", srv.URL, "ollama")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if ns.Models == nil {
		t.Error("Models is nil, want non-nil empty after successful probe")
	}
	if len(ns.Models) != 0 {
		t.Errorf("Models = %v, want empty", ns.Models)
	}
}

// ---------------------------------------------------------------------------
// static_models + health_path
// ---------------------------------------------------------------------------

func TestProbe_StaticModelsWithHealthPath(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("OK not json at all")) // body must be ignored
	}))
	defer srv.Close()

	n := enabledNode(1, "static", srv.URL, "openai_compat")
	n.StaticModels = []string{"custom-a", "custom-b"}
	n.HealthPath = strPtr("/healthz")

	p, reg, _ := newTestPoller(t, []store.Node{n}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotPath.Load(); got != "/healthz" {
		t.Errorf("probe path = %v, want /healthz", got)
	}
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if len(ns.Models) != 2 || ns.Models[0] != "custom-a" || ns.Models[1] != "custom-b" {
		t.Errorf("Models = %v, want static list", ns.Models)
	}
}

// Without health_path, a static_models node probes its type's discovery
// endpoint but only the status code matters — a non-JSON body is fine and the
// static list stays authoritative.
func TestProbe_StaticModelsWithoutHealthPathUsesDiscoveryEndpoint(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Write([]byte("<html>definitely not json</html>"))
	}))
	defer srv.Close()

	n := enabledNode(1, "static-ollama", srv.URL, "ollama")
	n.StaticModels = []string{"pinned:latest"}

	p, reg, _ := newTestPoller(t, []store.Node{n}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotPath.Load(); got != "/api/tags" {
		t.Errorf("probe path = %v, want /api/tags", got)
	}
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy", ns.Health)
	}
	if len(ns.Models) != 1 || ns.Models[0] != "pinned:latest" {
		t.Errorf("Models = %v, want [pinned:latest]", ns.Models)
	}
}

// ---------------------------------------------------------------------------
// Probe client hardening
// ---------------------------------------------------------------------------

func TestProbe_AuthHeaderSentWhenConfigured(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	n := enabledNode(1, "authed", srv.URL, "ollama")
	n.AuthHeader = strPtr("Bearer super-secret")

	p, _, _ := newTestPoller(t, []store.Node{n}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotAuth.Load(); got != "Bearer super-secret" {
		t.Errorf("Authorization = %v, want %q", got, "Bearer super-secret")
	}
}

func TestProbe_NoAuthHeaderWhenAbsent(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	p, _, _ := newTestPoller(t, []store.Node{enabledNode(1, "open", srv.URL, "ollama")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := gotAuth.Load(); got != "" {
		t.Errorf("Authorization = %v, want empty", got)
	}
}

// Redirects must never be followed: with auth_header attached, following one
// could exfiltrate backend credentials. The redirect target must never be
// contacted and the probe must fail.
func TestProbe_RedirectRefused(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Write([]byte(`{"models":[{"name":"evil"}]}`))
	}))
	defer target.Close()

	redirecter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/tags", http.StatusFound)
	}))
	defer redirecter.Close()

	n := enabledNode(1, "redirecting", redirecter.URL, "ollama")
	n.AuthHeader = strPtr("Bearer secret-that-must-not-leak")

	p, reg, _ := newTestPoller(t, []store.Node{n}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if hits := targetHits.Load(); hits != 0 {
		t.Errorf("redirect target was contacted %d times, want 0", hits)
	}
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy (redirect refused)", ns.Health)
	}
	if ns.LastError == "" {
		t.Error("LastError is empty, want redirect refusal error")
	}
	if strings.Contains(ns.LastError, "secret-that-must-not-leak") {
		t.Error("LastError leaks the auth header")
	}
}

// A discovery response larger than the body cap cannot be trusted (it would
// be truncated), so the probe fails.
func TestProbe_DiscoveryBodyOverCapFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[`))
		filler := `{"name":"` + strings.Repeat("x", 1024) + `"},`
		for i := 0; i < 1100; i++ { // ~1.1MB
			w.Write([]byte(filler))
		}
		w.Write([]byte(`{"name":"last"}]}`))
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "huge", srv.URL, "ollama")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy (body over cap)", ns.Health)
	}
	if ns.LastError == "" {
		t.Error("LastError is empty, want body-cap error")
	}
	if ns.Models != nil {
		t.Errorf("Models = %v, want nil (never successfully discovered)", ns.Models)
	}
}

// A health-only probe (static_models) ignores the body entirely, so an
// oversized body does not fail it — the cap just bounds how much is read.
func TestProbe_HealthProbeIgnoresOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 2048; i++ {
			w.Write([]byte(strings.Repeat("y", 1024))) // 2MB of ignored body
		}
	}))
	defer srv.Close()

	n := enabledNode(1, "chatty", srv.URL, "ollama")
	n.StaticModels = []string{"m"}
	n.HealthPath = strPtr("/healthz")

	p, reg, _ := newTestPoller(t, []store.Node{n}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy (body ignored)", ns.Health)
	}
}

func TestProbe_Non2xxIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "sad", srv.URL, "ollama")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy", ns.Health)
	}
	if !strings.Contains(ns.LastError, "503") {
		t.Errorf("LastError = %q, want it to mention status 503", ns.LastError)
	}
}

// ---------------------------------------------------------------------------
// Hysteresis state machine (running poller, not sweep)
// ---------------------------------------------------------------------------

// flakyBackend serves ollama discovery, failing with 500 while fail is true.
type flakyBackend struct {
	fail atomic.Bool
	srv  *httptest.Server
}

func newFlakyBackend(t *testing.T) *flakyBackend {
	t.Helper()
	f := &flakyBackend{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"models":[{"name":"llama3:8b"}]}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// pollOnce drives one running-poller probe cycle over the poller's loaded specs.
func pollOnce(t *testing.T, p *Poller) {
	t.Helper()
	specs, err := p.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p.probeSpecs(context.Background(), specs, false)
}

func TestHysteresis_UnknownToUnhealthyNeedsTwoFailures(t *testing.T) {
	f := newFlakyBackend(t)
	f.fail.Store(true)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "flaky", f.srv.URL, "ollama")}, Options{})

	pollOnce(t, p)
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthUnknown {
		t.Errorf("after 1 failure: Health = %q, want unknown (hysteresis)", ns.Health)
	}
	if ns.LastError == "" {
		t.Error("after 1 failure: LastError empty, want set")
	}

	pollOnce(t, p)
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthUnhealthy {
		t.Errorf("after 2 failures: Health = %q, want unhealthy", ns.Health)
	}
}

func TestHysteresis_UnknownToHealthyOnFirstSuccess(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "flaky", f.srv.URL, "ollama")}, Options{})

	pollOnce(t, p)
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy on first success", ns.Health)
	}
	if len(ns.Models) != 1 || ns.Models[0] != "llama3:8b" {
		t.Errorf("Models = %v, want [llama3:8b]", ns.Models)
	}
}

func TestHysteresis_HealthyToUnhealthyNeedsTwoConsecutiveFailures(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "flaky", f.srv.URL, "ollama")}, Options{})

	pollOnce(t, p) // healthy
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Fatalf("setup: Health = %q, want healthy", ns.Health)
	}

	f.fail.Store(true)
	pollOnce(t, p) // failure 1 of 2: still healthy
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("after 1 failure: Health = %q, want still healthy", ns.Health)
	}
	if ns.LastError == "" {
		t.Error("after 1 failure: LastError empty, want set even while still healthy")
	}
	// Last-known models are retained through the failure.
	if len(ns.Models) != 1 || ns.Models[0] != "llama3:8b" {
		t.Errorf("after 1 failure: Models = %v, want last-known [llama3:8b]", ns.Models)
	}

	pollOnce(t, p) // failure 2 of 2: unhealthy
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthUnhealthy {
		t.Errorf("after 2 failures: Health = %q, want unhealthy", ns.Health)
	}
}

// A success between two failures resets the failure counter: the failures are
// no longer consecutive.
func TestHysteresis_SuccessResetsFailureCount(t *testing.T) {
	f := newFlakyBackend(t)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "flaky", f.srv.URL, "ollama")}, Options{})

	pollOnce(t, p) // healthy
	f.fail.Store(true)
	pollOnce(t, p) // failure 1: still healthy
	f.fail.Store(false)
	pollOnce(t, p) // success: counter resets
	f.fail.Store(true)
	pollOnce(t, p) // failure 1 again: must still be healthy

	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy (failures were not consecutive)", ns.Health)
	}
}

func TestHysteresis_UnhealthyToHealthyOnFirstSuccess(t *testing.T) {
	f := newFlakyBackend(t)
	f.fail.Store(true)
	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "flaky", f.srv.URL, "ollama")}, Options{})

	pollOnce(t, p)
	pollOnce(t, p) // unhealthy
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthUnhealthy {
		t.Fatalf("setup: Health = %q, want unhealthy", ns.Health)
	}

	f.fail.Store(false)
	pollOnce(t, p)
	ns := nodeState(t, reg, 1)
	if ns.Health != registry.HealthHealthy {
		t.Errorf("Health = %q, want healthy on first success", ns.Health)
	}
	if ns.LastError != "" {
		t.Errorf("LastError = %q, want cleared on success", ns.LastError)
	}
}

// ---------------------------------------------------------------------------
// Startup sweep
// ---------------------------------------------------------------------------

// The sweep marks a node unhealthy on a SINGLE failure (per the design doc);
// the two-failure hysteresis only applies to the running poller.
func TestSweepOnce_SingleFailureMarksUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	p, reg, _ := newTestPoller(t, []store.Node{enabledNode(1, "down", srv.URL, "ollama")}, Options{})
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthUnhealthy {
		t.Errorf("Health = %q, want unhealthy after single sweep failure", ns.Health)
	}
}

// The sweep probes in parallel and is bounded by the sweep budget even when a
// node hangs forever: the responsive node comes up healthy, the hung one is
// published unhealthy, and the sweep returns well before the per-probe 5s
// default.
func TestSweepOnce_BudgetBoundedWithHungNode(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"m1"}]}`))
	}))
	defer healthy.Close()

	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never respond; unblocks when the client gives up
	}))
	defer hung.Close()

	p, reg, _ := newTestPoller(t, []store.Node{
		enabledNode(1, "fast", healthy.URL, "ollama"),
		enabledNode(2, "hung", hung.URL, "ollama"),
	}, Options{SweepBudget: 500 * time.Millisecond}) // per-probe stays at the 5s default

	start := time.Now()
	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("SweepOnce took %v, want bounded by the ~500ms sweep budget", elapsed)
	}
	if ns := nodeState(t, reg, 1); ns.Health != registry.HealthHealthy {
		t.Errorf("fast node Health = %q, want healthy", ns.Health)
	}
	ns := nodeState(t, reg, 2)
	if ns.Health != registry.HealthUnhealthy {
		t.Errorf("hung node Health = %q, want unhealthy", ns.Health)
	}
	if ns.LastError == "" {
		t.Error("hung node LastError empty, want deadline error")
	}
}

func TestSweepOnce_SourceErrorReturned(t *testing.T) {
	p, _, src := newTestPoller(t, nil, Options{})
	src.set(nil, context.DeadlineExceeded)

	if err := p.SweepOnce(context.Background()); err == nil {
		t.Fatal("SweepOnce = nil error, want the source error")
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

func TestMetrics_NodeUpAndOllamaUpOr(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":[{"name":"m"}]}`))
	}))
	defer up.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer down.Close()

	src := &fakeSource{nodes: []store.Node{
		enabledNode(1, "alive", up.URL, "ollama"),
		enabledNode(2, "dead", down.URL, "ollama"),
	}}
	reg := registry.New()
	m := metrics.New(func() int { return 0 })
	p := New(src, reg, m, Options{})

	if err := p.SweepOnce(context.Background()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	if got := testutil.ToFloat64(m.NodeUp.WithLabelValues("alive")); got != 1 {
		t.Errorf("aiproxy_node_up{node=alive} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.NodeUp.WithLabelValues("dead")); got != 0 {
		t.Errorf("aiproxy_node_up{node=dead} = %v, want 0", got)
	}
	// OR of node states: one healthy node keeps the legacy gauge at 1.
	if got := testutil.ToFloat64(m.OllamaUp); got != 1 {
		t.Errorf("aiproxy_ollama_up = %v, want 1 (OR of node states)", got)
	}

	// Drop the healthy node; only the dead one remains -> OR falls to 0 and
	// the removed node's gauge series is deleted.
	src.set([]store.Node{enabledNode(2, "dead", down.URL, "ollama")}, nil)
	specs, err := p.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p.probeSpecs(context.Background(), specs, false)

	if got := testutil.ToFloat64(m.OllamaUp); got != 0 {
		t.Errorf("aiproxy_ollama_up = %v, want 0 after healthy node removed", got)
	}
	if n := testutil.CollectAndCount(m.NodeUp); n != 1 {
		t.Errorf("aiproxy_node_up series count = %d, want 1 (removed node deleted)", n)
	}
}

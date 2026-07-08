// Package poller loads backend nodes from the database into the routing
// registry and keeps their health and model lists current by probing them.
// It implements the "Discovery and health checking" section of
// docs/design/distributed-nodes.md:
//
//   - SweepOnce is the startup sweep: it probes all enabled nodes in
//     parallel under a bounded budget (per-probe 5s, overall ~6s) and marks
//     non-responders unhealthy on a SINGLE failure, so restarts are
//     deterministic before the HTTP listener opens.
//   - Run is the ongoing poll loop: each node is probed every ~15s with a
//     deterministic per-node ±20% jitter, and the node set is re-read from
//     the database on every cycle so admin-API changes (BE-7) are picked up
//     without a restart. Run returns when its context is cancelled.
//   - The running loop applies hysteresis: healthy/unknown → unhealthy only
//     after 2 consecutive failures; any success is immediately healthy.
//
// Probe = discovery: for `ollama` nodes GET {base_url}/api/tags supplies both
// liveness and the model list (tag names); for `openai_compat` nodes GET
// {base_url}/v1/models (data[].id). Nodes with static_models are only
// health-checked (health_path if set, else the type's discovery endpoint);
// 2xx = alive, the body is ignored, and the static list stays authoritative.
//
// Probe client hardening (review-mandated): redirects are NEVER followed
// (CheckRedirect returns an error — with auth_header attached, following a
// redirect could exfiltrate backend credentials), response bodies are read
// through a 1MB cap, and every probe carries the node's auth_header as the
// Authorization header when set.
//
// Probe outcomes are published to the registry after every probe via
// registry.SetNodeProbe, including LastError and LastCheckedAt (NodeState
// was extended additively for this; BE-7's admin endpoints can read both
// straight from Registry.Snapshot).
//
// Wiring notes for BE-6 (main.go):
//
//	p := poller.New(db, reg, m, poller.Options{})
//	if err := p.SweepOnce(ctx); err != nil { /* fail fast: DB unreadable */ }
//	go p.Run(ctx) // alongside credits.StartSweeper; returns on ctx cancel
//
// Metrics: the poller sets aiproxy_node_up{node} on every probe and keeps
// the legacy aiproxy_ollama_up gauge equal to the OR of all node states
// after every probe and reload. While the poller runs it owns that gauge —
// do not also wire health.Checker.SetOllamaGauge.
package poller

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

const (
	defaultInterval     = 15 * time.Second
	defaultJitterFrac   = 0.20
	defaultProbeTimeout = 5 * time.Second
	defaultSweepBudget  = 6 * time.Second
	defaultMaxBodyBytes = 1 << 20 // 1MB

	// failureThreshold is the number of CONSECUTIVE probe failures needed to
	// take a node from healthy (or unknown) to unhealthy in the running
	// poller. The startup sweep bypasses it by design.
	failureThreshold = 2

	// minWait keeps the Run loop from busy-spinning if a computed wake time
	// is already in the past.
	minWait = 100 * time.Millisecond

	backendOllama = "ollama"
)

// errRedirectRefused is returned from the probe client's CheckRedirect:
// following a redirect with auth_header attached could exfiltrate backend
// credentials to an attacker-influenced Location.
var errRedirectRefused = errors.New("upstream redirect refused (probes never follow redirects)")

// NodeSource is the subset of *store.Store the poller needs. The loader must
// see raw auth_header values (they are sent on probes), hence WithSecrets.
type NodeSource interface {
	ListNodesWithSecrets() ([]store.Node, error)
}

// Options tunes the poller. Zero values take the documented defaults.
type Options struct {
	Interval     time.Duration // base poll interval; default 15s
	JitterFrac   float64       // per-node jitter fraction; default 0.20 (±20%)
	ProbeTimeout time.Duration // per-probe budget; default 5s
	SweepBudget  time.Duration // overall SweepOnce budget; default 6s
	MaxBodyBytes int64         // probe response body cap; default 1MB
}

// nodeSpec is the poller's probing view of one enabled node, built from a
// store row on each load.
type nodeSpec struct {
	id           int64
	name         string
	baseURL      *url.URL
	backendType  string
	authHeader   string   // "" = none
	staticModels []string // non-nil disables discovery; the list is authoritative
	healthPath   string   // "" = none; only meaningful with staticModels
}

// discoveryProbe reports whether the probe response body is parsed for
// models (true) or ignored (false, static_models nodes).
func (s nodeSpec) discoveryProbe() bool { return s.staticModels == nil }

// probeURL path-joins the probe endpoint onto base_url. Joining (never
// overwriting url.Path) preserves base_url path prefixes like /openai.
func (s nodeSpec) probeURL() string {
	switch {
	case !s.discoveryProbe() && s.healthPath != "":
		return s.baseURL.JoinPath(s.healthPath).String()
	case s.backendType == backendOllama:
		return s.baseURL.JoinPath("api/tags").String()
	default: // openai_compat
		return s.baseURL.JoinPath("v1/models").String()
	}
}

// identity returns the node's probe identity: every field that changes what
// a probe means — the URL it hits, the credentials it carries, and whether
// the model list is discovered or statically pinned (and to what). Runtime
// state carried across a reload is only valid while the identity is
// unchanged; the nil-vs-empty staticModels distinction is encoded so
// switching between discovery and an empty static list also resets.
func (s nodeSpec) identity() string {
	id := s.baseURL.String() + "\x1f" + s.backendType + "\x1f" + s.authHeader + "\x1f" + s.healthPath + "\x1f"
	if s.staticModels == nil {
		return id + "discovery"
	}
	return id + "static\x1f" + strings.Join(s.staticModels, "\x1f")
}

// nodeRuntime is the poller-owned per-node state machine, guarded by
// Poller.mu. The poller (not the registry) owns hysteresis: the registry only
// ever sees the already-smoothed health value.
type nodeRuntime struct {
	name      string          // last published metric label, for series cleanup
	identity  string          // probe identity; a change invalidates runtime state
	health    registry.Health // smoothed health as last published
	failures  int             // consecutive probe failures
	models    []string        // last known model list; nil = never discovered
	lastProbe time.Time       // for scheduling; zero = probe immediately
}

// Poller loads nodes from a NodeSource into a registry and probes them.
// SweepOnce and Run are safe to use from one goroutine each; internal state
// is locked because probes within a cycle run in parallel.
type Poller struct {
	src    NodeSource
	reg    *registry.Registry
	m      *metrics.Metrics // nil-safe (metrics disabled)
	opts   Options
	client *http.Client
	now    func() time.Time // injectable clock for tests

	mu        sync.Mutex
	state     map[int64]*nodeRuntime
	lastSpecs []nodeSpec // last successfully loaded node set (reload fallback)
}

// New builds a poller. reg must be non-nil; m may be nil (metrics disabled).
// Zero Options fields take the defaults documented on Options.
func New(src NodeSource, reg *registry.Registry, m *metrics.Metrics, opts Options) *Poller {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.JitterFrac <= 0 {
		opts.JitterFrac = defaultJitterFrac
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = defaultProbeTimeout
	}
	if opts.SweepBudget <= 0 {
		opts.SweepBudget = defaultSweepBudget
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = defaultMaxBodyBytes
	}
	return &Poller{
		src:   src,
		reg:   reg,
		m:     m,
		opts:  opts,
		now:   time.Now,
		state: make(map[int64]*nodeRuntime),
		client: &http.Client{
			Timeout: opts.ProbeTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errRedirectRefused
			},
		},
	}
}

// SweepOnce is the synchronous startup sweep: it loads enabled nodes from
// the database and probes them all in parallel, bounded by SweepBudget
// overall and ProbeTimeout per node. Nodes that respond are healthy with a
// populated model list; nodes that don't are unhealthy after this SINGLE
// failure (per the design doc — hysteresis only applies to the running
// loop) and receive no traffic until the poller reaches them. The returned
// error is only for a failed node load (DB unreachable); probe failures are
// recorded in the registry, not returned.
func (p *Poller) SweepOnce(ctx context.Context) error {
	specs, err := p.load()
	if err != nil {
		return err
	}
	sweepCtx, cancel := context.WithTimeout(ctx, p.opts.SweepBudget)
	defer cancel()
	p.probeSpecs(sweepCtx, specs, true)
	return nil
}

// Run is the ongoing poll loop. Each cycle re-reads the node set from the
// database (a cheap single-gateway query, so BE-7 admin changes apply
// without a restart), probes every node that is due, and sleeps until the
// next node is due — capped at one base interval so newly added nodes are
// probed within ~Interval. The first cycle runs immediately; nodes already
// probed by SweepOnce are not due again until a full jittered interval after
// the sweep. Run returns when ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	timer := time.NewTimer(0) // immediate first cycle
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		wait := p.pollCycle(ctx)
		if ctx.Err() != nil {
			return
		}
		timer.Reset(wait)
	}
}

// pollCycle runs one poll cycle — reload, probe everything due, compute the
// next wake — and returns how long to wait before the next cycle. When the
// reload fails it falls back to the last successfully loaded node set: a
// transient DB error must not stop health refreshing, or routing would trust
// stale registry state until the DB recovers.
func (p *Poller) pollCycle(ctx context.Context) time.Duration {
	specs, err := p.load()
	if err != nil {
		specs = p.cachedSpecs()
		slog.Error("node poller: reloading nodes failed; probing last-known node set",
			"error", err, "nodes", len(specs))
	}
	now := p.now()
	due := make([]nodeSpec, 0, len(specs))
	for _, s := range specs {
		if !p.nextDue(s.id).After(now) {
			due = append(due, s)
		}
	}
	p.probeSpecs(ctx, due, false)

	// Next wake: the earliest node due, capped at one base interval so node
	// additions/changes in the DB are picked up within ~Interval even when
	// nothing is due sooner.
	next := p.now().Add(p.opts.Interval)
	for _, s := range specs {
		if d := p.nextDue(s.id); d.Before(next) {
			next = d
		}
	}
	wait := next.Sub(p.now())
	if wait < minWait {
		wait = minWait
	}
	return wait
}

// cachedSpecs returns the node set from the last successful load.
func (p *Poller) cachedSpecs() []nodeSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSpecs
}

// load reads all nodes from the source, maps the enabled ones into the
// registry (registry.SetNodes carries runtime state over by ID and resets
// nodes whose BaseURL changed), and reconciles the poller's own runtime
// state to the new node set. It returns the probing specs for the cycle.
func (p *Poller) load() ([]nodeSpec, error) {
	rows, err := p.src.ListNodesWithSecrets()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	specs := make([]nodeSpec, 0, len(rows))
	regNodes := make([]registry.Node, 0, len(rows))
	for _, n := range rows {
		if !n.Enabled {
			continue
		}
		u, err := url.Parse(n.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			// base_url is canonicalized at write time, so this indicates DB
			// corruption; skip the node rather than probing garbage.
			slog.Error("node poller: skipping node with unparseable base_url", "node", n.Name, "error", err)
			continue
		}
		spec := nodeSpec{
			id:           n.ID,
			name:         n.Name,
			baseURL:      u,
			backendType:  n.BackendType,
			staticModels: n.StaticModels,
		}
		if n.AuthHeader != nil {
			spec.authHeader = *n.AuthHeader
		}
		if n.HealthPath != nil {
			spec.healthPath = *n.HealthPath
		}
		var timeout time.Duration
		if n.TimeoutSeconds != nil {
			timeout = time.Duration(*n.TimeoutSeconds) * time.Second
		}
		specs = append(specs, spec)
		regNodes = append(regNodes, registry.Node{
			ID:         n.ID,
			Name:       n.Name,
			BaseURL:    u,
			AuthHeader: spec.authHeader,
			Timeout:    timeout,
		})
	}

	p.reg.SetNodes(regNodes)
	p.reconcile(specs)
	return specs, nil
}

// reconcile aligns the poller's runtime state with a freshly loaded node
// set: new nodes start unknown and immediately due, removed nodes drop their
// state and metric series, renamed nodes migrate their metric series, and a
// node whose probe identity changed (BaseURL, backend_type, auth_header,
// health_path, or static_models) is a different probing target — its
// hysteresis state, model list, and schedule reset, and the reset is
// published to the registry immediately so stale carried-over models (e.g. a
// removed static model) can never stay routable while waiting for the next
// probe. The reset node is due at once, so the same cycle reprobes it.
func (p *Poller) reconcile(specs []nodeSpec) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current := make(map[int64]bool, len(specs))
	for _, s := range specs {
		current[s.id] = true
		st, ok := p.state[s.id]
		if !ok {
			p.state[s.id] = &nodeRuntime{
				name:     s.name,
				identity: s.identity(),
				health:   registry.HealthUnknown,
			}
			continue
		}
		if id := s.identity(); st.identity != id {
			st.identity = id
			st.health = registry.HealthUnknown
			st.failures = 0
			st.models = nil
			st.lastProbe = time.Time{}
			// registry.SetNodes only resets on BaseURL changes; for the
			// other identity fields it carried the old state over, so
			// publish the reset explicitly.
			p.reg.SetNodeProbe(s.id, registry.ProbeResult{Health: registry.HealthUnknown})
		}
		if st.name != s.name {
			p.m.DeleteNodeUp(st.name)
			st.name = s.name
		}
	}
	for id, st := range p.state {
		if !current[id] {
			p.m.DeleteNodeUp(st.name)
			delete(p.state, id)
		}
	}
	p.lastSpecs = specs
	p.m.SetOllamaUp(p.anyHealthyLocked())
}

// probeSpecs probes the given nodes in parallel and applies each outcome.
// sweep selects startup-sweep semantics (single failure = unhealthy).
func (p *Poller) probeSpecs(ctx context.Context, specs []nodeSpec, sweep bool) {
	var wg sync.WaitGroup
	for _, s := range specs {
		wg.Add(1)
		go func(s nodeSpec) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, p.opts.ProbeTimeout)
			defer cancel()
			models, err := p.probe(probeCtx, s)
			p.apply(s, models, err, sweep)
		}(s)
	}
	wg.Wait()
}

// probe performs one HTTP probe. On success it returns the node's model
// list: parsed from the discovery response, or a copy of the static list for
// health-only probes. All response reading goes through the body cap.
func (p *Poller) probe(ctx context.Context, s nodeSpec) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.probeURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("build probe request: %w", err)
	}
	if s.authHeader != "" {
		req.Header.Set("Authorization", s.authHeader)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		drainCapped(resp.Body, p.opts.MaxBodyBytes)
		return nil, fmt.Errorf("probe returned status %d", resp.StatusCode)
	}

	if !s.discoveryProbe() {
		// Health-only probe: 2xx = alive, body ignored (the cap bounds how
		// much of it is ever read). The static list is authoritative.
		drainCapped(resp.Body, p.opts.MaxBodyBytes)
		models := make([]string, len(s.staticModels))
		copy(models, s.staticModels)
		return models, nil
	}

	body, err := readCapped(resp.Body, p.opts.MaxBodyBytes)
	if err != nil {
		return nil, err
	}
	return parseModels(s.backendType, body)
}

// apply runs the per-node state machine on a probe outcome, logs health
// transitions, and publishes the result to the registry and metrics.
//
// Transitions: any success → healthy with the failure counter reset;
// a failure increments the counter and demotes to unhealthy once it reaches
// failureThreshold — or immediately when sweep is set (startup semantics:
// nodes that miss the sweep get no traffic until the poller reaches them).
// On failure the last-known model list keeps being published: routing
// ignores it while unhealthy, and it documents what the node served.
func (p *Poller) apply(s nodeSpec, models []string, probeErr error, sweep bool) {
	now := p.now()

	p.mu.Lock()
	defer p.mu.Unlock()

	st, ok := p.state[s.id]
	if !ok {
		// The node vanished in a concurrent reload; drop the result (the
		// registry would ignore it anyway).
		return
	}

	prev := st.health
	var lastErr string
	if probeErr == nil {
		st.failures = 0
		st.health = registry.HealthHealthy
		st.models = models
	} else {
		lastErr = probeErr.Error()
		st.failures++
		if sweep || st.failures >= failureThreshold {
			st.health = registry.HealthUnhealthy
		}
	}
	st.lastProbe = now

	if st.health != prev {
		if lastErr != "" {
			slog.Info("node health transition",
				"node", s.name, "from", prev, "to", st.health, "error", lastErr)
		} else {
			slog.Info("node health transition", "node", s.name, "from", prev, "to", st.health)
		}
	} else if probeErr != nil {
		slog.Debug("node probe failed",
			"node", s.name, "health", st.health, "consecutive_failures", st.failures, "error", lastErr)
	}

	p.reg.SetNodeProbe(s.id, registry.ProbeResult{
		Health:        st.health,
		Models:        st.models,
		LastError:     lastErr,
		LastCheckedAt: now,
	})
	p.m.SetNodeUp(s.name, st.health == registry.HealthHealthy)
	p.m.SetOllamaUp(p.anyHealthyLocked())
}

// anyHealthyLocked reports whether any known node is healthy (the OR fed
// into the legacy aiproxy_ollama_up gauge). Callers must hold p.mu.
func (p *Poller) anyHealthyLocked() bool {
	for _, st := range p.state {
		if st.health == registry.HealthHealthy {
			return true
		}
	}
	return false
}

// nextDue returns when the node should next be probed: a full jittered
// interval after its last probe, or immediately (zero time) if it has never
// been probed by this poller.
func (p *Poller) nextDue(id int64) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[id]
	if !ok || st.lastProbe.IsZero() {
		return time.Time{}
	}
	return st.lastProbe.Add(p.intervalFor(id))
}

// intervalFor returns the node's poll interval: the base interval with a
// deterministic per-node jitter in [-JitterFrac, +JitterFrac], derived from
// the node ID. Deriving jitter from the ID (rather than a live RNG) spreads
// probes across nodes, keeps each node's cadence steady, and makes
// scheduling fully deterministic in tests.
func (p *Poller) intervalFor(nodeID int64) time.Duration {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(nodeID))
	h := fnv.New64a()
	h.Write(b[:])
	// Map the hash onto [-1, 1] exactly: n/1023.5 - 1 for n in [0, 2047].
	frac := float64(h.Sum64()%2048)/1023.5 - 1.0
	return time.Duration(float64(p.opts.Interval) * (1 + p.opts.JitterFrac*frac))
}

// readCapped reads r fully, failing if the body exceeds max bytes — a
// truncated discovery response cannot be trusted, so the probe must fail
// rather than publish a partial model list.
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read probe response: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("probe response body exceeds %d bytes", limit)
	}
	return b, nil
}

// drainCapped discards up to max bytes of r so the connection can be reused,
// without ever reading an unbounded body.
func drainCapped(r io.Reader, limit int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, limit))
}

// parseModels extracts the model list from a discovery response body.
// Ollama /api/tags: {"models":[{"name":...}]}; openai_compat /v1/models:
// {"data":[{"id":...}]}. The result is always non-nil: a successful probe
// with zero models is a known-empty list, distinct from never-discovered.
func parseModels(backendType string, body []byte) ([]string, error) {
	if backendType == backendOllama {
		var tags struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &tags); err != nil {
			return nil, fmt.Errorf("parse ollama /api/tags response: %w", err)
		}
		models := make([]string, 0, len(tags.Models))
		for _, m := range tags.Models {
			if m.Name != "" {
				models = append(models, m.Name)
			}
		}
		return models, nil
	}

	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse /v1/models response: %w", err)
	}
	models := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

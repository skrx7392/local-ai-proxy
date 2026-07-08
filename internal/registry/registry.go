// Package registry is the in-memory routing core for distributed backend
// nodes. It maps model names to healthy nodes and round-robins across
// candidates. It has no HTTP or database dependencies: loaders (config file,
// DB) declare nodes via SetNodes and the health poller reports probe results
// via SetNodeState; the proxy consumes Resolve and Snapshot.
//
// Concurrency model (see docs/design/distributed-nodes.md, "Registry
// concurrency"): routing state is published as an immutable snapshot swapped
// via atomic.Pointer (copy-on-write). Writers rebuild a complete snapshot
// under the registry mutex and publish it atomically; Resolve reads exactly
// one snapshot per call and can never observe a torn mix of old and new
// state. Round-robin counters live outside the snapshot in a mutex-guarded
// map so republishes do not reset balancing; the map is pruned to routable
// models on each republish and is never grown by lookups of unknown models.
package registry

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrModelUnavailable is returned by Resolve when no healthy node with a
// known model list serves the requested model. Callers should map it to
// 503 model_unavailable.
var ErrModelUnavailable = errors.New("no healthy node serves this model")

// Health is a node's probed health state. Unknown and unhealthy are both
// non-routable; they are distinct for display (never probed vs failing).
type Health string

const (
	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthUnhealthy Health = "unhealthy"
)

// Node is the routing view of a backend node: everything the proxy needs to
// forward a request.
type Node struct {
	ID         int64
	Name       string
	BaseURL    *url.URL
	AuthHeader string        // Authorization value sent upstream; "" = none
	Timeout    time.Duration // per-node request timeout; 0 = default
}

// clone returns a deep copy of n so registry state can never be mutated
// through values handed to callers (url.URL is a mutable struct).
func (n Node) clone() Node {
	if n.BaseURL != nil {
		u := *n.BaseURL
		if n.BaseURL.User != nil {
			user := *n.BaseURL.User
			u.User = &user
		}
		n.BaseURL = &u
	}
	return n
}

// NodeState is a node plus its runtime state, as exposed by Snapshot.
type NodeState struct {
	Node   Node
	Health Health
	Models []string // last reported model list; nil = not yet discovered
}

// RegistrySnapshot is a self-consistent view of the registry: every node the
// registry knows about (ascending by ID) and the model→nodes routing map.
// Models contains only routable candidates: healthy nodes with a known model
// list, ascending by ID. The returned value is a private copy; callers may
// retain and mutate it freely.
type RegistrySnapshot struct {
	Nodes  []NodeState
	Models map[string][]Node
}

// snapshot is the immutable published routing state. Once stored it is never
// mutated; writers build a complete replacement and swap the pointer.
type snapshot struct {
	nodes  []NodeState       // ascending by Node.ID
	models map[string][]Node // model -> routable candidates, ascending by Node.ID
}

// entry is the writer-side authoritative state for one node, guarded by
// Registry.mu.
type entry struct {
	node   Node
	health Health
	models []string
}

// Registry routes models to healthy backend nodes.
//
// Resolve and Snapshot are safe for unbounded concurrent use; SetNodes and
// SetNodeState may be called concurrently with them and with each other.
type Registry struct {
	snap atomic.Pointer[snapshot]

	mu       sync.Mutex
	entries  map[int64]*entry
	counters map[string]uint64 // round-robin position per routable model
}

// New returns an empty registry: every Resolve fails with
// ErrModelUnavailable until nodes are declared and reported healthy.
func New() *Registry {
	r := &Registry{
		entries:  make(map[int64]*entry),
		counters: make(map[string]uint64),
	}
	r.snap.Store(&snapshot{models: make(map[string][]Node)})
	return r
}

// SetNodes replaces the full set of nodes the registry knows about (initial
// load, config reload, admin add/update/disable). Runtime state carries over
// for a node whose ID persists with an unchanged BaseURL; a node whose
// BaseURL changed is effectively an unprobed backend, so its health resets
// to unknown and its model list is cleared (no optimistic routing). If the
// same ID appears more than once the last declaration wins. A new snapshot
// is published before SetNodes returns.
func (r *Registry) SetNodes(nodes []Node) {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := make(map[int64]*entry, len(nodes))
	for _, n := range nodes {
		e := &entry{node: n.clone(), health: HealthUnknown}
		if prev, ok := r.entries[n.ID]; ok && sameURL(prev.node.BaseURL, n.BaseURL) {
			e.health = prev.health
			e.models = prev.models
		}
		next[n.ID] = e
	}
	r.entries = next
	r.publishLocked()
}

// SetNodeState records a probe result for one node and publishes a new
// snapshot. models is the node's discovered (or static) model list; nil
// means the list is unknown, which keeps the node non-routable even when
// healthy. State reported for a node ID the registry does not know is
// ignored (the poller may race a node removal).
func (r *Registry) SetNodeState(nodeID int64, health Health, models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[nodeID]
	if !ok {
		return
	}
	e.health = health
	if models == nil {
		e.models = nil
	} else {
		e.models = append([]string(nil), models...)
	}
	r.publishLocked()
}

// Resolve returns a healthy node serving model, round-robining across
// candidates on successive calls. It returns an error wrapping
// ErrModelUnavailable when no healthy node with a known model list serves
// the model. The returned Node is a private copy; mutating it (including
// BaseURL) does not affect the registry.
func (r *Registry) Resolve(model string) (Node, error) {
	candidates := r.snap.Load().models[model]
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("model %q: %w", model, ErrModelUnavailable)
	}

	var idx int
	if len(candidates) > 1 {
		// The counter is only touched for models that are routable in the
		// snapshot just read, so arbitrary client-supplied model names can
		// never allocate counter state.
		r.mu.Lock()
		c := r.counters[model]
		r.counters[model] = c + 1
		r.mu.Unlock()
		idx = int(c % uint64(len(candidates)))
	}
	return candidates[idx].clone(), nil
}

// Snapshot returns the current node states and model→nodes routing map, for
// /v1/models, admin endpoints, and readiness. The result is a deep copy the
// caller owns.
func (r *Registry) Snapshot() RegistrySnapshot {
	s := r.snap.Load()

	out := RegistrySnapshot{
		Nodes:  make([]NodeState, len(s.nodes)),
		Models: make(map[string][]Node, len(s.models)),
	}
	for i, ns := range s.nodes {
		out.Nodes[i] = NodeState{
			Node:   ns.Node.clone(),
			Health: ns.Health,
			Models: append([]string(nil), ns.Models...),
		}
	}
	for model, candidates := range s.models {
		nodes := make([]Node, len(candidates))
		for i, n := range candidates {
			nodes[i] = n.clone()
		}
		out.Models[model] = nodes
	}
	return out
}

// publishLocked rebuilds the immutable snapshot from r.entries, prunes
// round-robin counters for models that are no longer routable, and swaps the
// snapshot pointer. Callers must hold r.mu.
func (r *Registry) publishLocked() {
	nodes := make([]NodeState, 0, len(r.entries))
	models := make(map[string][]Node)
	for _, e := range r.entries {
		nodes = append(nodes, NodeState{Node: e.node, Health: e.health, Models: e.models})
		if e.health != HealthHealthy || e.models == nil {
			continue
		}
		for _, m := range e.models {
			models[m] = append(models[m], e.node)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node.ID < nodes[j].Node.ID })
	for _, candidates := range models {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	}

	// Prune counters for models that disappeared from the routable map; the
	// counter map stays bounded by the discovered catalog.
	for model := range r.counters {
		if _, ok := models[model]; !ok {
			delete(r.counters, model)
		}
	}

	r.snap.Store(&snapshot{nodes: nodes, models: models})
}

func sameURL(a, b *url.URL) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.String() == b.String()
}

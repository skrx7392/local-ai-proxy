package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/krishna/local-ai-proxy/internal/registry"
)

// Pinger is satisfied by *store.Store or any type with a Ping method.
type Pinger interface {
	Ping(ctx context.Context) error
}

// NodeSnapshotter exposes the node registry's current state. Satisfied by
// *registry.Registry. Readiness reads the snapshot only — it never probes
// backends synchronously; the health poller owns probing.
type NodeSnapshotter interface {
	Snapshot() registry.RegistrySnapshot
}

// Checker holds dependencies for health check endpoints.
type Checker struct {
	db         Pinger
	nodes      NodeSnapshotter
	usageChLen func() int
	usageChCap int
}

// NewChecker creates a health Checker. Any parameter can be nil/zero to skip
// that check.
func NewChecker(db Pinger, nodes NodeSnapshotter, usageChLen func() int, usageChCap int) *Checker {
	return &Checker{
		db:         db,
		nodes:      nodes,
		usageChLen: usageChLen,
		usageChCap: usageChCap,
	}
}

// LiveHandler returns 200 OK unconditionally. Used for k8s liveness probes.
func (c *Checker) LiveHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// CheckResult is one component's health snapshot. Exported so other packages
// (e.g. internal/admin) can render it under their own response shape.
type CheckResult struct {
	Status    string `json:"status"`
	LatencyMs *int64 `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Depth     *int   `json:"queue_depth,omitempty"`
	Capacity  *int   `json:"queue_capacity,omitempty"`
	Total     *int   `json:"total,omitempty"`
	Healthy   *int   `json:"healthy,omitempty"`
}

// RunChecks executes every configured probe and returns whether all passed
// plus per-component results, keyed by the spec's component name (db, nodes,
// usage_writer).
//
// The nodes rule (docs/design/distributed-nodes.md, "Readiness"):
// ready = zero enabled nodes OR at least one healthy node. Zero enabled
// nodes is deliberately OK — a fresh install must serve the admin API to
// register its first node — flagged with a "no nodes configured" detail.
// Unprobed (unknown) nodes count the same as unhealthy ones. Per-node
// breakdown is an admin concern (BE-7), not readiness's.
func (c *Checker) RunChecks(ctx context.Context) (allOK bool, checks map[string]CheckResult) {
	checks = map[string]CheckResult{}
	allOK = true

	if c.db != nil {
		start := time.Now()
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.db.Ping(pingCtx)
		cancel()
		ms := time.Since(start).Milliseconds()

		if err != nil {
			allOK = false
			checks["db"] = CheckResult{Status: "error", LatencyMs: &ms, Error: err.Error()}
		} else {
			checks["db"] = CheckResult{Status: "ok", LatencyMs: &ms}
		}
	}

	if c.nodes != nil {
		snap := c.nodes.Snapshot()
		total := len(snap.Nodes)
		healthy := 0
		for _, ns := range snap.Nodes {
			if ns.Health == registry.HealthHealthy {
				healthy++
			}
		}
		result := CheckResult{Status: "ok", Total: &total, Healthy: &healthy}
		switch {
		case total == 0:
			result.Detail = "no nodes configured"
		case healthy == 0:
			allOK = false
			result.Status = "error"
			result.Error = "no healthy nodes"
		}
		checks["nodes"] = result
	}

	if c.usageChLen != nil {
		depth := c.usageChLen()
		capacity := c.usageChCap
		result := CheckResult{Status: "ok", Depth: &depth, Capacity: &capacity}
		if depth >= capacity {
			allOK = false
			result.Status = "error"
			result.Error = "usage channel full"
		}
		checks["usage_writer"] = result
	}

	return allOK, checks
}

// ReadyHandler checks DB, node registry, and usage writer health. Used for
// k8s readiness probes. Renames the db key to "database" for compatibility
// with existing probe consumers.
func (c *Checker) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	allOK, checks := c.RunChecks(r.Context())

	legacy := make(map[string]CheckResult, len(checks))
	for k, v := range checks {
		if k == "db" {
			legacy["database"] = v
			continue
		}
		legacy[k] = v
	}

	status := "ready"
	httpStatus := http.StatusOK
	if !allOK {
		status = "not_ready"
		httpStatus = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"checks": legacy,
	})
}

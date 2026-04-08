package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Pinger is satisfied by *store.Store or any type with a Ping method.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker holds dependencies for health check endpoints.
type Checker struct {
	db         Pinger
	ollamaURL  string
	usageChLen func() int
	usageChCap int
	ollamaUp   prometheus.Gauge // nil-safe
}

// NewChecker creates a health Checker. Any parameter can be nil/zero to skip that check.
func NewChecker(db Pinger, ollamaURL string, usageChLen func() int, usageChCap int) *Checker {
	return &Checker{
		db:         db,
		ollamaURL:  ollamaURL,
		usageChLen: usageChLen,
		usageChCap: usageChCap,
	}
}

// SetOllamaGauge sets the prometheus gauge that will be updated on readiness checks.
func (c *Checker) SetOllamaGauge(g prometheus.Gauge) {
	c.ollamaUp = g
}

// LiveHandler returns 200 OK unconditionally. Used for k8s liveness probes.
func (c *Checker) LiveHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

type checkResult struct {
	Status    string `json:"status"`
	LatencyMs *int64 `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Depth     *int   `json:"queue_depth,omitempty"`
	Capacity  *int   `json:"queue_capacity,omitempty"`
}

// ReadyHandler checks DB, Ollama, and usage writer health. Used for k8s readiness probes.
func (c *Checker) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	checks := map[string]checkResult{}
	allOK := true

	// DB check
	if c.db != nil {
		start := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := c.db.Ping(ctx)
		cancel()
		ms := time.Since(start).Milliseconds()

		if err != nil {
			allOK = false
			checks["database"] = checkResult{Status: "error", LatencyMs: &ms, Error: err.Error()}
		} else {
			checks["database"] = checkResult{Status: "ok", LatencyMs: &ms}
		}
	}

	// Ollama check
	if c.ollamaURL != "" {
		start := time.Now()
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Head(c.ollamaURL)
		ms := time.Since(start).Milliseconds()

		if err != nil {
			allOK = false
			checks["ollama"] = checkResult{Status: "error", LatencyMs: &ms, Error: err.Error()}
			if c.ollamaUp != nil {
				c.ollamaUp.Set(0)
			}
		} else {
			resp.Body.Close()
			checks["ollama"] = checkResult{Status: "ok", LatencyMs: &ms}
			if c.ollamaUp != nil {
				c.ollamaUp.Set(1)
			}
		}
	}

	// Usage writer check
	if c.usageChLen != nil {
		depth := c.usageChLen()
		cap := c.usageChCap
		result := checkResult{Status: "ok", Depth: &depth, Capacity: &cap}
		if depth >= cap {
			allOK = false
			result.Status = "error"
			result.Error = "usage channel full"
		}
		checks["usage_writer"] = result
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
		"checks": checks,
	})
}

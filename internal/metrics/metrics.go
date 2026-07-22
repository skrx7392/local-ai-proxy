package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus metrics for the ai-proxy.
type Metrics struct {
	// HTTP metrics (recorded by InstrumentHandler)
	RequestDuration  *prometheus.HistogramVec // labels: method, status
	RequestsTotal    *prometheus.CounterVec   // labels: method, status
	RequestBodySize  *prometheus.HistogramVec // labels: method
	ResponseBodySize *prometheus.HistogramVec // labels: method, status

	// Token metrics (recorded by proxy handler)
	TokensTotal *prometheus.CounterVec // labels: model, type (prompt|completion), node

	// Credit/rate limit
	CreditGateRejects prometheus.Counter
	RateLimitRejects  prometheus.Counter
	// Account-level limiter rejects (docs/design/per-account-rate-limiting.md).
	// Partitions rejects with RateLimitRejects by gate: key-level rejects stay
	// in the legacy counter, account-level ones land here.
	AccountRateLimitRejects *prometheus.CounterVec // labels: kind (rate|concurrency), class (enduser|service)
	UsageDrops              prometheus.Counter

	// Infrastructure
	OllamaUp prometheus.Gauge
	NodeUp   *prometheus.GaugeVec // labels: node — per-node probe health (1=up, 0=down)
	// StreamsInflight counts non-GET /api/v1/ requests currently holding a
	// concurrency slot — "is GPU occupancy saturated right now".
	StreamsInflight prometheus.Gauge

	// Sweeper
	SweeperRuns  *prometheus.CounterVec // labels: operation (stale_holds|settled_cleanup), outcome (success|error)
	SweeperSwept *prometheus.CounterVec // labels: operation — increments by rows affected on success

	// Registrations
	Registrations *prometheus.CounterVec // labels: source (user_signup|service_registration|admin_bootstrap)

	// Admin auth failures (401/403 only; 429 rate-limit rejects tracked separately)
	AdminAuthFailures *prometheus.CounterVec // labels: reason

	registry *prometheus.Registry
}

// bodySizeBuckets spans empty bodies to multi-MB streaming payloads.
var bodySizeBuckets = []float64{
	0, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216,
}

// New creates and registers all metrics. usageChLen returns the current
// length of the usage channel for the depth gauge.
func New(usageChLen func() int) *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aiproxy_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "status"}),

		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_requests_total",
			Help: "Total HTTP requests.",
		}, []string{"method", "status"}),

		RequestBodySize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aiproxy_http_request_body_bytes",
			Help:    "HTTP request body size in bytes. Only observed when Content-Length is known (>= 0).",
			Buckets: bodySizeBuckets,
		}, []string{"method"}),

		ResponseBodySize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "aiproxy_http_response_body_bytes",
			Help:    "HTTP response body size in bytes, accumulated from Write calls.",
			Buckets: bodySizeBuckets,
		}, []string{"method", "status"}),

		TokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_tokens_total",
			Help: "Total tokens processed, by model, type, and serving node.",
		}, []string{"model", "type", "node"}),

		CreditGateRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aiproxy_credit_gate_rejects_total",
			Help: "Requests rejected by credit gate.",
		}),

		RateLimitRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aiproxy_ratelimit_rejects_total",
			Help: "Requests rejected by the key-level rate limiter. Account-level rejects are counted in aiproxy_account_ratelimit_rejects_total.",
		}),

		AccountRateLimitRejects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_account_ratelimit_rejects_total",
			Help: "Requests rejected by the account-level limiter, by kind (rate|concurrency) and account class (enduser|service).",
		}, []string{"kind", "class"}),

		UsageDrops: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aiproxy_usage_drops_total",
			Help: "Usage entries dropped because the async usage channel was full.",
		}),

		OllamaUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aiproxy_ollama_up",
			Help: "Whether Ollama is reachable (1=up, 0=down). Updated on readiness check.",
		}),

		NodeUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "aiproxy_node_up",
			Help: "Whether a backend node is healthy per its last probe (1=up, 0=down), by node name.",
		}, []string{"node"}),

		StreamsInflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aiproxy_streams_inflight",
			Help: "Non-GET proxy requests currently holding a per-account concurrency slot.",
		}),

		SweeperRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_credit_sweeper_runs_total",
			Help: "Credit-hold sweeper tick invocations by operation and outcome.",
		}, []string{"operation", "outcome"}),

		SweeperSwept: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_credit_sweeper_swept_total",
			Help: "Rows affected by the credit-hold sweeper (released or deleted) on successful runs.",
		}, []string{"operation"}),

		Registrations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_registrations_total",
			Help: "Successful registrations by source.",
		}, []string{"source"}),

		AdminAuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_admin_auth_failures_total",
			Help: "Admin auth middleware 401/403 rejections by reason. Rate-limit (429) rejections are counted in aiproxy_ratelimit_rejects_total.",
		}, []string{"reason"}),

		registry: reg,
	}

	usageDepth := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "aiproxy_usage_channel_depth",
		Help: "Current depth of the async usage logging channel.",
	}, func() float64 {
		return float64(usageChLen())
	})

	reg.MustRegister(
		m.RequestDuration,
		m.RequestsTotal,
		m.RequestBodySize,
		m.ResponseBodySize,
		m.TokensTotal,
		m.CreditGateRejects,
		m.RateLimitRejects,
		m.AccountRateLimitRejects,
		m.UsageDrops,
		m.OllamaUp,
		m.NodeUp,
		m.StreamsInflight,
		m.SweeperRuns,
		m.SweeperSwept,
		m.Registrations,
		m.AdminAuthFailures,
		usageDepth,
	)

	return m
}

// RegisterPoolCollector attaches a pgxpool Stat collector to the metrics
// registry. Called after New once the DB pool is available. Safe to call
// with a nil provider — the collector will emit nothing on scrape.
func (m *Metrics) RegisterPoolCollector(provider PoolStatProvider) {
	if m == nil {
		return
	}
	m.registry.MustRegister(newPoolCollector(provider))
}

// Handler returns an http.Handler that serves the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RecordTokens increments token counters for the node that served the
// request (cardinality is bounded by the handful of configured nodes).
// Nil-safe.
func (m *Metrics) RecordTokens(model, node string, prompt, completion int) {
	if m == nil {
		return
	}
	if prompt > 0 {
		m.TokensTotal.WithLabelValues(model, "prompt", node).Add(float64(prompt))
	}
	if completion > 0 {
		m.TokensTotal.WithLabelValues(model, "completion", node).Add(float64(completion))
	}
}

// RecordCreditGateReject increments the credit gate reject counter. Nil-safe.
func (m *Metrics) RecordCreditGateReject() {
	if m == nil {
		return
	}
	m.CreditGateRejects.Inc()
}

// RecordStreamStart marks a concurrency slot acquired. Nil-safe.
func (m *Metrics) RecordStreamStart() {
	if m == nil {
		return
	}
	m.StreamsInflight.Inc()
}

// RecordStreamEnd marks a concurrency slot released. Nil-safe.
func (m *Metrics) RecordStreamEnd() {
	if m == nil {
		return
	}
	m.StreamsInflight.Dec()
}

// RecordAccountRateLimitReject increments the account-level limiter reject
// counter. Nil-safe. kind is "rate" or "concurrency"; class is "enduser" or
// "service" — bounded vocabularies, never an account ID (cardinality rule).
func (m *Metrics) RecordAccountRateLimitReject(kind, class string) {
	if m == nil {
		return
	}
	m.AccountRateLimitRejects.WithLabelValues(kind, class).Inc()
}

// RecordRateLimitReject increments the rate limit reject counter. Nil-safe.
func (m *Metrics) RecordRateLimitReject() {
	if m == nil {
		return
	}
	m.RateLimitRejects.Inc()
}

// RecordUsageDrop increments the usage-channel drop counter. Nil-safe.
func (m *Metrics) RecordUsageDrop() {
	if m == nil {
		return
	}
	m.UsageDrops.Inc()
}

// SetNodeUp sets aiproxy_node_up{node} to 1 (up) or 0 (down). Called by the
// node health poller on every probe. Nil-safe.
func (m *Metrics) SetNodeUp(node string, up bool) {
	if m == nil {
		return
	}
	v := 0.0
	if up {
		v = 1.0
	}
	m.NodeUp.WithLabelValues(node).Set(v)
}

// DeleteNodeUp removes the aiproxy_node_up series for a node that no longer
// exists (removed or renamed), so stale series don't linger on /metrics.
// Deleting a series that was never set is a no-op. Nil-safe.
func (m *Metrics) DeleteNodeUp(node string) {
	if m == nil {
		return
	}
	m.NodeUp.DeleteLabelValues(node)
}

// SetOllamaUp sets the legacy aiproxy_ollama_up gauge. Kept for one release
// as the OR of all node states: the node health poller calls this with
// "any node healthy" after every probe cycle (see internal/poller). While a
// poller is running it owns this gauge — BE-6 must not also wire
// health.Checker.SetOllamaGauge, or the two writers will fight. Nil-safe.
func (m *Metrics) SetOllamaUp(up bool) {
	if m == nil {
		return
	}
	if up {
		m.OllamaUp.Set(1)
		return
	}
	m.OllamaUp.Set(0)
}

// RecordSweeperRun records a sweeper tick. On success, rowsAffected is added
// to the swept counter. Nil-safe.
func (m *Metrics) RecordSweeperRun(operation string, rowsAffected int64, err error) {
	if m == nil {
		return
	}
	if err != nil {
		m.SweeperRuns.WithLabelValues(operation, "error").Inc()
		return
	}
	m.SweeperRuns.WithLabelValues(operation, "success").Inc()
	if rowsAffected > 0 {
		m.SweeperSwept.WithLabelValues(operation).Add(float64(rowsAffected))
	}
}

// RecordRegistration increments the registration counter for a bounded source
// vocabulary: user_signup, service_registration, admin_bootstrap. Nil-safe.
func (m *Metrics) RecordRegistration(source string) {
	if m == nil {
		return
	}
	m.Registrations.WithLabelValues(source).Inc()
}

// RecordAdminAuthFailure increments the admin-auth-failures counter with a
// bounded reason. Rate-limit (429) failures are intentionally excluded —
// they're already counted by the rate-limit rejects counter. Nil-safe.
func (m *Metrics) RecordAdminAuthFailure(reason string) {
	if m == nil {
		return
	}
	m.AdminAuthFailures.WithLabelValues(reason).Inc()
}

// InstrumentHandler wraps an http.Handler to record request duration, count,
// and request/response body size. The wrapper preserves http.Flusher for SSE.
func (m *Metrics) InstrumentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		status := strconv.Itoa(sw.status)
		duration := time.Since(start).Seconds()
		m.RequestDuration.WithLabelValues(r.Method, status).Observe(duration)
		m.RequestsTotal.WithLabelValues(r.Method, status).Inc()

		// Request body size: only observe when Content-Length is known.
		// Chunked / unknown length → ContentLength is -1; skip to avoid
		// polluting the histogram with a negative bucket.
		if r.ContentLength >= 0 {
			m.RequestBodySize.WithLabelValues(r.Method).Observe(float64(r.ContentLength))
		}

		m.ResponseBodySize.WithLabelValues(r.Method, status).Observe(float64(sw.bytes))
	})
}

// statusWriter captures the response status code and bytes written while
// delegating all writes to the inner ResponseWriter. Preserves Flusher.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.wroteHeader = true
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.bytes += int64(n)
	return n, err
}

// Flush delegates to the inner ResponseWriter if it implements http.Flusher.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap allows http.ResponseController to reach the inner writer.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

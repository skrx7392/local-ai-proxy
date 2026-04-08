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
	RequestDuration *prometheus.HistogramVec // labels: method, status
	RequestsTotal   *prometheus.CounterVec   // labels: method, status

	// Token metrics (recorded by proxy handler)
	TokensTotal *prometheus.CounterVec // labels: model, type (prompt|completion)

	// Credit/rate limit
	CreditGateRejects prometheus.Counter
	RateLimitRejects  prometheus.Counter

	// Infrastructure
	OllamaUp prometheus.Gauge

	registry *prometheus.Registry
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

		TokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aiproxy_tokens_total",
			Help: "Total tokens processed.",
		}, []string{"model", "type"}),

		CreditGateRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aiproxy_credit_gate_rejects_total",
			Help: "Requests rejected by credit gate.",
		}),

		RateLimitRejects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aiproxy_ratelimit_rejects_total",
			Help: "Requests rejected by rate limiter.",
		}),

		OllamaUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "aiproxy_ollama_up",
			Help: "Whether Ollama is reachable (1=up, 0=down). Updated on readiness check.",
		}),

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
		m.TokensTotal,
		m.CreditGateRejects,
		m.RateLimitRejects,
		m.OllamaUp,
		usageDepth,
	)

	return m
}

// Handler returns an http.Handler that serves the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// RecordTokens increments token counters. Nil-safe.
func (m *Metrics) RecordTokens(model string, prompt, completion int) {
	if m == nil {
		return
	}
	if prompt > 0 {
		m.TokensTotal.WithLabelValues(model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.TokensTotal.WithLabelValues(model, "completion").Add(float64(completion))
	}
}

// RecordCreditGateReject increments the credit gate reject counter. Nil-safe.
func (m *Metrics) RecordCreditGateReject() {
	if m == nil {
		return
	}
	m.CreditGateRejects.Inc()
}

// RecordRateLimitReject increments the rate limit reject counter. Nil-safe.
func (m *Metrics) RecordRateLimitReject() {
	if m == nil {
		return
	}
	m.RateLimitRejects.Inc()
}

// InstrumentHandler wraps an http.Handler to record request duration and count.
// The wrapper preserves http.Flusher for SSE streaming compatibility.
func (m *Metrics) InstrumentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		status := strconv.Itoa(sw.status)
		duration := time.Since(start).Seconds()
		m.RequestDuration.WithLabelValues(r.Method, status).Observe(duration)
		m.RequestsTotal.WithLabelValues(r.Method, status).Inc()
	})
}

// statusWriter captures the response status code while delegating all
// writes to the inner ResponseWriter. It preserves Flusher.
type statusWriter struct {
	http.ResponseWriter
	status      int
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
	return sw.ResponseWriter.Write(b)
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

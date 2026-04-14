package metrics

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNew_CreatesAllMetrics(t *testing.T) {
	m := New(func() int { return 42 })
	if m == nil {
		t.Fatal("expected non-nil Metrics")
	}
	checks := []struct {
		name string
		got  any
	}{
		{"RequestDuration", m.RequestDuration},
		{"RequestsTotal", m.RequestsTotal},
		{"RequestBodySize", m.RequestBodySize},
		{"ResponseBodySize", m.ResponseBodySize},
		{"TokensTotal", m.TokensTotal},
		{"CreditGateRejects", m.CreditGateRejects},
		{"RateLimitRejects", m.RateLimitRejects},
		{"UsageDrops", m.UsageDrops},
		{"OllamaUp", m.OllamaUp},
		{"SweeperRuns", m.SweeperRuns},
		{"SweeperSwept", m.SweeperSwept},
		{"Registrations", m.Registrations},
		{"AdminAuthFailures", m.AdminAuthFailures},
	}
	for _, c := range checks {
		if c.got == nil {
			t.Errorf("%s is nil", c.name)
		}
	}
}

func TestHandler_ExposesNewMetricNames(t *testing.T) {
	m := New(func() int { return 5 })
	m.RequestsTotal.WithLabelValues("POST", "200").Inc()
	m.RecordUsageDrop()
	m.RecordRegistration("user_signup")
	m.RecordAdminAuthFailure("invalid_session")
	m.RecordSweeperRun("stale_holds", 3, nil)
	m.RequestBodySize.WithLabelValues("POST").Observe(128)
	m.ResponseBodySize.WithLabelValues("POST", "200").Observe(256)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	want := []string{
		"aiproxy_requests_total",
		"aiproxy_usage_channel_depth",
		"aiproxy_usage_drops_total",
		"aiproxy_http_request_body_bytes",
		"aiproxy_http_response_body_bytes",
		"aiproxy_credit_sweeper_runs_total",
		"aiproxy_credit_sweeper_swept_total",
		"aiproxy_registrations_total",
		"aiproxy_admin_auth_failures_total",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("expected %s in /metrics output", w)
		}
	}
}

func TestRecordTokens_NilSafe(t *testing.T) {
	var m *Metrics
	m.RecordTokens("llama3.1:8b", 100, 200)
	m.RecordCreditGateReject()
	m.RecordRateLimitReject()
	m.RecordUsageDrop()
	m.RecordSweeperRun("stale_holds", 1, nil)
	m.RecordRegistration("user_signup")
	m.RecordAdminAuthFailure("invalid_session")
	m.RegisterPoolCollector(nil)
}

func TestRecordTokens_IncrementsCounters(t *testing.T) {
	m := New(func() int { return 0 })
	m.RecordTokens("llama3.1:8b", 100, 200)

	prompt := testutil.ToFloat64(m.TokensTotal.WithLabelValues("llama3.1:8b", "prompt"))
	if prompt != 100 {
		t.Errorf("expected prompt tokens 100, got %v", prompt)
	}
	completion := testutil.ToFloat64(m.TokensTotal.WithLabelValues("llama3.1:8b", "completion"))
	if completion != 200 {
		t.Errorf("expected completion tokens 200, got %v", completion)
	}
}

func TestInstrumentHandler_RecordsMetrics(t *testing.T) {
	m := New(func() int { return 0 })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := m.InstrumentHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	count := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("GET", "200"))
	if count != 1 {
		t.Errorf("expected RequestsTotal count 1, got %v", count)
	}
}

func TestInstrumentHandler_PreservesFlusher(t *testing.T) {
	m := New(func() int { return 0 })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("expected Flusher interface to be preserved")
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := m.InstrumentHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
}

func TestInstrumentHandler_RecordsResponseBytes(t *testing.T) {
	m := New(func() int { return 0 })

	payload := strings.Repeat("x", 1234)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
	})

	handler := m.InstrumentHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Histogram _sum should equal the payload size.
	sum := getHistogramSum(t, m, "aiproxy_http_response_body_bytes")
	if sum != float64(len(payload)) {
		t.Errorf("expected response body sum %d, got %v", len(payload), sum)
	}
}

func TestInstrumentHandler_RequestBody_KnownLength(t *testing.T) {
	m := New(func() int { return 0 })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	handler := m.InstrumentHandler(inner)
	body := strings.NewReader("hello world")
	req := httptest.NewRequest(http.MethodPost, "/x", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	sum := getHistogramSum(t, m, "aiproxy_http_request_body_bytes")
	if sum != 11 {
		t.Errorf("expected request body sum 11, got %v", sum)
	}
}

func TestInstrumentHandler_RequestBody_SkipsUnknownLength(t *testing.T) {
	m := New(func() int { return 0 })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	handler := m.InstrumentHandler(inner)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("abc"))
	req.ContentLength = -1 // chunked / unknown
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Histogram should have zero observations — no negative bucket pollution.
	count := getHistogramCount(t, m, "aiproxy_http_request_body_bytes")
	if count != 0 {
		t.Errorf("expected 0 observations when ContentLength is -1, got %d", count)
	}
}

func TestUsageChannelDepth_ReflectsFunction(t *testing.T) {
	depth := 42
	m := New(func() int { return depth })

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "aiproxy_usage_channel_depth 42") {
		t.Errorf("expected usage_channel_depth 42 in output:\n%s", string(body))
	}
}

func TestRecordSweeperRun_Outcomes(t *testing.T) {
	m := New(func() int { return 0 })

	m.RecordSweeperRun("stale_holds", 7, nil)
	m.RecordSweeperRun("stale_holds", 0, nil)
	m.RecordSweeperRun("stale_holds", 0, errBoom{})
	m.RecordSweeperRun("settled_cleanup", 3, nil)

	if got := testutil.ToFloat64(m.SweeperRuns.WithLabelValues("stale_holds", "success")); got != 2 {
		t.Errorf("stale_holds success runs = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.SweeperRuns.WithLabelValues("stale_holds", "error")); got != 1 {
		t.Errorf("stale_holds error runs = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.SweeperSwept.WithLabelValues("stale_holds")); got != 7 {
		t.Errorf("stale_holds swept = %v, want 7", got)
	}
	if got := testutil.ToFloat64(m.SweeperSwept.WithLabelValues("settled_cleanup")); got != 3 {
		t.Errorf("settled_cleanup swept = %v, want 3", got)
	}
}

func TestPoolCollector_EmitsAllMetrics(t *testing.T) {
	m := New(func() int { return 0 })
	m.RegisterPoolCollector(fakeStatProvider{
		PoolStat{
			Total: 5, Acquired: 2, Idle: 3, Max: 10, Constructing: 1,
			AcquireCount: 42, AcquireDuration: 500 * time.Millisecond,
			NewConns: 6, LifetimeDestroys: 1, IdleDestroys: 2, EmptyAcquires: 3,
		},
	})

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(raw)

	wantLines := []string{
		"aiproxy_db_pool_total_connections 5",
		"aiproxy_db_pool_acquired_connections 2",
		"aiproxy_db_pool_idle_connections 3",
		"aiproxy_db_pool_max_connections 10",
		"aiproxy_db_pool_constructing_connections 1",
		"aiproxy_db_pool_acquire_count_total 42",
		"aiproxy_db_pool_acquire_duration_seconds_total 0.5",
		"aiproxy_db_pool_new_connections_total 6",
		"aiproxy_db_pool_lifetime_destroys_total 1",
		"aiproxy_db_pool_idle_destroys_total 2",
		"aiproxy_db_pool_empty_acquires_total 3",
	}
	for _, line := range wantLines {
		if !strings.Contains(text, line) {
			t.Errorf("expected %q in /metrics output", line)
		}
	}
}

func TestPoolCollector_NilProvider_NoPanic(t *testing.T) {
	m := New(func() int { return 0 })
	m.RegisterPoolCollector(nil)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// --- helpers -----------------------------------------------------------------

type fakeStatProvider struct{ s PoolStat }

func (f fakeStatProvider) Stat() PoolStat { return f.s }

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func getHistogramSum(t *testing.T, m *Metrics, name string) float64 {
	t.Helper()
	for _, line := range metricsLines(t, m) {
		if strings.HasPrefix(line, name+"_sum") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				var v float64
				fmt.Sscan(parts[len(parts)-1], &v)
				return v
			}
		}
	}
	return 0
}

func getHistogramCount(t *testing.T, m *Metrics, name string) int64 {
	t.Helper()
	for _, line := range metricsLines(t, m) {
		if strings.HasPrefix(line, name+"_count") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				var v int64
				fmt.Sscan(parts[len(parts)-1], &v)
				return v
			}
		}
	}
	return 0
}

func metricsLines(t *testing.T, m *Metrics) []string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.Split(string(body), "\n")
}

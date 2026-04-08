package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNew_CreatesAllMetrics(t *testing.T) {
	m := New(func() int { return 42 })
	if m == nil {
		t.Fatal("expected non-nil Metrics")
	}
	if m.RequestDuration == nil {
		t.Error("RequestDuration is nil")
	}
	if m.RequestsTotal == nil {
		t.Error("RequestsTotal is nil")
	}
	if m.TokensTotal == nil {
		t.Error("TokensTotal is nil")
	}
	if m.CreditGateRejects == nil {
		t.Error("CreditGateRejects is nil")
	}
	if m.RateLimitRejects == nil {
		t.Error("RateLimitRejects is nil")
	}
	if m.OllamaUp == nil {
		t.Error("OllamaUp is nil")
	}
}

func TestHandler_ReturnsPrometheusFormat(t *testing.T) {
	m := New(func() int { return 5 })
	m.RequestsTotal.WithLabelValues("POST", "200").Inc()

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "aiproxy_requests_total") {
		t.Error("expected aiproxy_requests_total in /metrics output")
	}
	if !strings.Contains(text, "aiproxy_usage_channel_depth") {
		t.Error("expected aiproxy_usage_channel_depth in /metrics output")
	}
}

func TestRecordTokens_NilSafe(t *testing.T) {
	var m *Metrics
	m.RecordTokens("llama3.1:8b", 100, 200) // must not panic
}

func TestRecordCreditGateReject_NilSafe(t *testing.T) {
	var m *Metrics
	m.RecordCreditGateReject() // must not panic
}

func TestRecordRateLimitReject_NilSafe(t *testing.T) {
	var m *Metrics
	m.RecordRateLimitReject() // must not panic
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
	rec := httptest.NewRecorder() // httptest.ResponseRecorder implements Flusher

	handler.ServeHTTP(rec, req)
}

func TestUsageChannelDepth_ReflectsFunction(t *testing.T) {
	depth := 42
	m := New(func() int { return depth })

	// Read the gauge value from metrics output
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "aiproxy_usage_channel_depth 42") {
		t.Errorf("expected usage_channel_depth 42 in output:\n%s", string(body))
	}
}

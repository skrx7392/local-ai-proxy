package health

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestChecker_SetOllamaGauge(t *testing.T) {
	c := NewChecker(nil, "", nil, 0)
	if c.ollamaUp != nil {
		t.Fatal("expected initial ollamaUp nil")
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_ollama_up"})
	c.SetOllamaGauge(g)
	if c.ollamaUp != g {
		t.Error("SetOllamaGauge did not attach gauge")
	}
}

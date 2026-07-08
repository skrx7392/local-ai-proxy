package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSetNodeUp(t *testing.T) {
	m := New(func() int { return 0 })

	m.SetNodeUp("mac-studio", true)
	m.SetNodeUp("gaming-pc", false)

	if got := testutil.ToFloat64(m.NodeUp.WithLabelValues("mac-studio")); got != 1 {
		t.Errorf("aiproxy_node_up{node=mac-studio} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.NodeUp.WithLabelValues("gaming-pc")); got != 0 {
		t.Errorf("aiproxy_node_up{node=gaming-pc} = %v, want 0", got)
	}

	// Flipping works.
	m.SetNodeUp("mac-studio", false)
	if got := testutil.ToFloat64(m.NodeUp.WithLabelValues("mac-studio")); got != 0 {
		t.Errorf("aiproxy_node_up{node=mac-studio} = %v, want 0 after flip", got)
	}
}

func TestDeleteNodeUp_RemovesSeries(t *testing.T) {
	m := New(func() int { return 0 })

	m.SetNodeUp("old-node", true)
	if n := testutil.CollectAndCount(m.NodeUp); n != 1 {
		t.Fatalf("series count = %d, want 1", n)
	}

	m.DeleteNodeUp("old-node")
	if n := testutil.CollectAndCount(m.NodeUp); n != 0 {
		t.Errorf("series count = %d after delete, want 0", n)
	}

	// Deleting a nonexistent series is a no-op.
	m.DeleteNodeUp("never-existed")
}

func TestSetOllamaUp(t *testing.T) {
	m := New(func() int { return 0 })

	m.SetOllamaUp(true)
	if got := testutil.ToFloat64(m.OllamaUp); got != 1 {
		t.Errorf("aiproxy_ollama_up = %v, want 1", got)
	}
	m.SetOllamaUp(false)
	if got := testutil.ToFloat64(m.OllamaUp); got != 0 {
		t.Errorf("aiproxy_ollama_up = %v, want 0", got)
	}
}

func TestNodeUpMethods_NilSafe(t *testing.T) {
	var m *Metrics
	m.SetNodeUp("x", true) // must not panic
	m.DeleteNodeUp("x")
	m.SetOllamaUp(true)
}

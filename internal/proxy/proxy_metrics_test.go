package proxy

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// TestLogUsage_ChannelFull_IncrementsUsageDrops exercises the default branch
// of the select statement in logUsage — this is the exact call site BE 6 adds
// the aiproxy_usage_drops_total counter to. Done as a direct unit test (not
// via the HTTP stack) so it runs without Ollama or a DB.
func TestLogUsage_ChannelFull_IncrementsUsageDrops(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 1)
	// Fill the channel so the next send hits the default branch.
	usageCh <- store.UsageEntry{}

	m := metrics.New(func() int { return len(usageCh) })
	h := &handler{usageCh: usageCh, metrics: m}

	key := &store.APIKey{ID: 1}
	h.logUsage(key, usageData{Model: "llama3.1:8b"}, 0, "ok", 0)

	if got := testutil.ToFloat64(m.UsageDrops); got != 1 {
		t.Errorf("UsageDrops = %v, want 1 after drop", got)
	}
}

// TestLogUsage_ChannelHasRoom_DoesNotIncrement confirms the counter only fires
// when the channel is actually full — not on every successful send.
func TestLogUsage_ChannelHasRoom_DoesNotIncrement(t *testing.T) {
	usageCh := make(chan store.UsageEntry, 4)
	m := metrics.New(func() int { return len(usageCh) })
	h := &handler{usageCh: usageCh, metrics: m}

	key := &store.APIKey{ID: 1}
	h.logUsage(key, usageData{Model: "llama3.1:8b"}, 0, "ok", 0)

	if got := testutil.ToFloat64(m.UsageDrops); got != 0 {
		t.Errorf("UsageDrops = %v, want 0 when channel has room", got)
	}
	if len(usageCh) != 1 {
		t.Errorf("usageCh len = %d, want 1", len(usageCh))
	}
}

package credits

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/krishna/local-ai-proxy/internal/metrics"
)

// TestStartSweeper_RecordsRunsAndSwept exercises the wiring from StartSweeper
// into *Metrics end-to-end: after one stale-hold sweep tick, success runs and
// swept rows must both increment.
func TestStartSweeper_RecordsRunsAndSwept(t *testing.T) {
	db := setupTestStore(t)

	accID, _, _ := db.RegisterUser("sweeper-metrics@example.com", "hash", "SweepM")
	_ = db.AddCredits(accID, 1000, "grant")

	holdID, err := db.ReserveCredits(accID, 25)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}
	// Backdate the hold past the stale threshold.
	_, _ = db.Pool().Exec(context.Background(),
		`UPDATE credit_holds SET created_at = NOW() - INTERVAL '20 minutes' WHERE id = $1`, holdID)

	m := metrics.New(func() int { return 0 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartSweeper(ctx, db, m,
		50*time.Millisecond, 10*time.Minute, // stale sweep every 50ms, threshold 10m
		1*time.Hour, 30*24*time.Hour, // cleanup won't trigger in this window
	)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(m.SweeperSwept.WithLabelValues(opStaleHolds)) > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if got := testutil.ToFloat64(m.SweeperRuns.WithLabelValues(opStaleHolds, "success")); got < 1 {
		t.Errorf("stale_holds success runs = %v, want >= 1", got)
	}
	if got := testutil.ToFloat64(m.SweeperSwept.WithLabelValues(opStaleHolds)); got < 1 {
		t.Errorf("stale_holds swept = %v, want >= 1", got)
	}
	if got := testutil.ToFloat64(m.SweeperRuns.WithLabelValues(opStaleHolds, "error")); got != 0 {
		t.Errorf("stale_holds error runs = %v, want 0 on healthy path", got)
	}
}

// TestStartSweeper_RecordsCleanupRuns confirms the settled_cleanup branch is
// independently wired — not a copy-paste that always records stale_holds.
func TestStartSweeper_RecordsCleanupRuns(t *testing.T) {
	db := setupTestStore(t)

	accID, _, _ := db.RegisterUser("sweeper-cleanup-metrics@example.com", "hash", "SweepC")
	_ = db.AddCredits(accID, 1000, "grant")
	holdID, _ := db.ReserveCredits(accID, 10)
	_, _ = db.SettleHold(holdID, 5)
	_, _ = db.Pool().Exec(context.Background(),
		`UPDATE credit_holds SET settled_at = NOW() - INTERVAL '31 days' WHERE id = $1`, holdID)

	m := metrics.New(func() int { return 0 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartSweeper(ctx, db, m,
		1*time.Hour, 10*time.Minute, // stale sweep dormant
		50*time.Millisecond, 30*24*time.Hour, // cleanup every 50ms
	)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if testutil.ToFloat64(m.SweeperSwept.WithLabelValues(opSettledCleanup)) > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if got := testutil.ToFloat64(m.SweeperRuns.WithLabelValues(opSettledCleanup, "success")); got < 1 {
		t.Errorf("settled_cleanup success runs = %v, want >= 1", got)
	}
	if got := testutil.ToFloat64(m.SweeperSwept.WithLabelValues(opSettledCleanup)); got < 1 {
		t.Errorf("settled_cleanup swept = %v, want >= 1", got)
	}
}

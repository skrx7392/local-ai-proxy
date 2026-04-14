package credits

import (
	"context"
	"log/slog"
	"time"

	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// Sweeper operation labels. Kept in sync with the bounded label set on
// aiproxy_credit_sweeper_runs_total / aiproxy_credit_sweeper_swept_total.
const (
	opStaleHolds      = "stale_holds"
	opSettledCleanup  = "settled_cleanup"
)

// StartSweeper launches two background goroutines:
// 1. Stale hold sweeper: releases pending holds older than staleThreshold (every sweepInterval)
// 2. Hold cleanup: deletes settled/released holds older than cleanupAge (every cleanupInterval)
// Both goroutines respect context cancellation for graceful shutdown.
//
// m may be nil (metrics disabled).
func StartSweeper(ctx context.Context, db *store.Store, m *metrics.Metrics,
	sweepInterval, staleThreshold time.Duration,
	cleanupInterval, cleanupAge time.Duration) {

	// Stale hold sweeper
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				released, err := db.SweepStaleHolds(staleThreshold)
				m.RecordSweeperRun(opStaleHolds, int64(released), err)
				if err != nil {
					slog.Error("sweep stale holds error", "error", err)
				} else if released > 0 {
					slog.Info("released stale credit holds", "count", released)
				}
			}
		}
	}()

	// Settled/released hold cleanup
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				deleted, err := db.CleanupSettledHolds(cleanupAge)
				m.RecordSweeperRun(opSettledCleanup, int64(deleted), err)
				if err != nil {
					slog.Error("cleanup settled holds error", "error", err)
				} else if deleted > 0 {
					slog.Info("cleaned up old credit holds", "count", deleted)
				}
			}
		}
	}()
}

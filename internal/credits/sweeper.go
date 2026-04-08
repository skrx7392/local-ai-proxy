package credits

import (
	"context"
	"log"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// StartSweeper launches two background goroutines:
// 1. Stale hold sweeper: releases pending holds older than staleThreshold (every sweepInterval)
// 2. Hold cleanup: deletes settled/released holds older than cleanupAge (every cleanupInterval)
// Both goroutines respect context cancellation for graceful shutdown.
func StartSweeper(ctx context.Context, db *store.Store,
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
				if err != nil {
					log.Printf("sweep stale holds error: %v", err)
				} else if released > 0 {
					log.Printf("released %d stale credit holds", released)
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
				if err != nil {
					log.Printf("cleanup settled holds error: %v", err)
				} else if deleted > 0 {
					log.Printf("cleaned up %d old credit holds", deleted)
				}
			}
		}
	}()
}

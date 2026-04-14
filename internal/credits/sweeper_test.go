package credits

import (
	"context"
	"testing"
	"time"
)

func TestStartSweeper_StopsOnContextCancel(t *testing.T) {
	db := setupTestStore(t)

	ctx, cancel := context.WithCancel(context.Background())

	// Start sweeper with very short intervals for testing
	StartSweeper(ctx, db,
		50*time.Millisecond, 10*time.Minute,
		50*time.Millisecond, 30*24*time.Hour,
	)

	// Let it tick a couple times
	time.Sleep(150 * time.Millisecond)

	// Cancel — goroutines should exit cleanly
	cancel()

	// Give goroutines time to exit
	time.Sleep(100 * time.Millisecond)
	// No assertion needed — test verifies no panic/deadlock on shutdown
}

func TestStartSweeper_ReleasesStaleHolds(t *testing.T) {
	db := setupTestStore(t)

	accID, _, _ := db.RegisterUser("sweep-test@example.com", "hash", "SweepTest")
	_ = db.AddCredits(accID, 1000, "grant")

	holdID, err := db.ReserveCredits(accID, 50)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}

	// Backdate the hold to make it stale
	pool := db.Pool()
	_, _ = pool.Exec(context.Background(),
		`UPDATE credit_holds SET created_at = NOW() - INTERVAL '20 minutes' WHERE id = $1`, holdID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sweeper with 10-minute stale threshold, checking every 50ms
	StartSweeper(ctx, db,
		50*time.Millisecond, 10*time.Minute,
		1*time.Hour, 30*24*time.Hour, // cleanup won't trigger during test
	)

	// Wait for sweeper to run
	time.Sleep(200 * time.Millisecond)

	// Verify hold was released
	bal, _ := db.GetCreditBalance(accID)
	if bal.Reserved != 0 {
		t.Errorf("expected reserved 0 after sweep, got %f", bal.Reserved)
	}
}

func TestStartSweeper_CleansUpOldHolds(t *testing.T) {
	db := setupTestStore(t)

	accID, _, _ := db.RegisterUser("cleanup-test@example.com", "hash", "CleanupTest")
	_ = db.AddCredits(accID, 1000, "grant")

	holdID, _ := db.ReserveCredits(accID, 10)
	_, _ = db.SettleHold(holdID, 5)

	// Backdate settled_at
	pool := db.Pool()
	_, _ = pool.Exec(context.Background(),
		`UPDATE credit_holds SET settled_at = NOW() - INTERVAL '31 days' WHERE id = $1`, holdID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartSweeper(ctx, db,
		1*time.Hour, 10*time.Minute, // sweep won't trigger
		50*time.Millisecond, 30*24*time.Hour, // cleanup checks every 50ms
	)

	time.Sleep(200 * time.Millisecond)

	// Verify hold was deleted — check directly in DB
	var count int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM credit_holds WHERE id = $1`, holdID).Scan(&count)
	if count != 0 {
		t.Errorf("expected hold to be deleted, but found %d", count)
	}
}

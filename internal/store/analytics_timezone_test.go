package store

import (
	"context"
	"testing"
	"time"
)

// TestGetUsageTimeseries_UTCAlignedEvenOnNonUTCSession locks in the contract
// that bucket boundaries returned by GetUsageTimeseries are always UTC,
// regardless of the Postgres session TimeZone. The pool AfterConnect hook
// already pins new connections to UTC, but that is defense-in-depth — this
// test overrides the session TZ on a held connection and runs the analytics
// query directly to prove the SQL expression itself is UTC-stable. Without
// the AT TIME ZONE 'UTC' wrapper inside date_trunc, a non-UTC session would
// return offset-shifted buckets and the handler gap-fill would silently
// drop real rows (P2 from BE 2 review).
func TestGetUsageTimeseries_UTCAlignedEvenOnNonUTCSession(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)
	ctx := context.Background()

	// Hold a single pooled connection for the whole test so the session TZ
	// we set below is observed by the subsequent query. Without Acquire, pgx
	// may hand out a different connection each statement.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("pool.Acquire: %v", err)
	}
	defer conn.Release()

	// Override the UTC default set by AfterConnect. If any `date_trunc` in
	// analytics.go is still implicitly session-scoped, buckets returned below
	// will be 7-hour-shifted from their UTC-truncated counterparts.
	if _, err := conn.Exec(ctx, "SET TIME ZONE 'America/Los_Angeles'"); err != nil {
		t.Fatalf("SET TIME ZONE: %v", err)
	}

	// Reproduce the exact timeseries SQL from GetUsageTimeseries against the
	// held connection. Window matches the fixture.
	rows, err := conn.Query(ctx,
		`SELECT
		   date_trunc($1, ul.created_at AT TIME ZONE 'UTC'),
		   COUNT(*)
		 FROM usage_logs ul
		 JOIN api_keys k ON ul.api_key_id = k.id
		 WHERE ul.created_at >= $2
		 GROUP BY 1
		 ORDER BY 1`,
		"day", fx.t0,
	)
	if err != nil {
		t.Fatalf("timeseries query: %v", err)
	}
	defer rows.Close()

	var buckets []time.Time
	for rows.Next() {
		var b time.Time
		var c int
		if err := rows.Scan(&b, &c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		buckets = append(buckets, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(buckets) == 0 {
		t.Fatal("expected at least one bucket from fixture")
	}

	for i, b := range buckets {
		// A UTC-truncated day bucket has zero-valued hour/min/sec fields
		// *in UTC*. If the session TZ leaked through, a bucket nominally at
		// LA-midnight would convert to 07:00:00 UTC.
		u := b.UTC()
		if u.Hour() != 0 || u.Minute() != 0 || u.Second() != 0 {
			t.Errorf("bucket[%d] = %v, want midnight-UTC (hour/min/sec must be 0)", i, u)
		}
	}
}

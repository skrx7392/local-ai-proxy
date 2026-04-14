package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// UsageFilter constrains the set of usage_logs rows aggregated by the
// analytics methods. All fields are optional; zero-valued fields skip that
// filter. Filters compose with AND semantics.
type UsageFilter struct {
	Since     *time.Time
	Until     *time.Time
	AccountID *int64
	APIKeyID  *int64
	UserID    *int64
	Model     *string
}

// UsageSummary is the aggregated response for GetUsageSummary.
type UsageSummary struct {
	Requests         int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Credits          float64
	AvgDurationMs    float64
	Errors           int
}

// ModelUsageRow is one row of GetUsageByModel.
type ModelUsageRow struct {
	Model         string
	Requests      int
	TotalTokens   int
	Credits       float64
	AvgDurationMs float64
}

// OwnerUsageRow is one row of GetUsageByUser. Either the user fields or the
// account fields will be populated depending on whether the key was
// user-owned, service-owned, or an unattributed admin key.
type OwnerUsageRow struct {
	UserID      *int64
	Email       *string
	Name        *string
	AccountID   *int64
	AccountName *string
	AccountType *string
	Requests    int
	TotalTokens int
	Credits     float64
	KeyCount    int
}

// TimeseriesBucket is one bucket of GetUsageTimeseries.
type TimeseriesBucket struct {
	Bucket           time.Time
	Requests         int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Credits          float64
	Errors           int
}

// buildUsageFilterClause appends WHERE conditions derived from f to the given
// string builder, starting at argIdx. It returns the updated args slice and
// the next argument index.
func buildUsageFilterClause(sb *strings.Builder, args []any, f UsageFilter, argIdx int) ([]any, int) {
	if f.Since != nil {
		fmt.Fprintf(sb, " AND ul.created_at >= $%d", argIdx)
		args = append(args, *f.Since)
		argIdx++
	}
	if f.Until != nil {
		fmt.Fprintf(sb, " AND ul.created_at < $%d", argIdx)
		args = append(args, *f.Until)
		argIdx++
	}
	if f.AccountID != nil {
		fmt.Fprintf(sb, " AND k.account_id = $%d", argIdx)
		args = append(args, *f.AccountID)
		argIdx++
	}
	if f.APIKeyID != nil {
		fmt.Fprintf(sb, " AND ul.api_key_id = $%d", argIdx)
		args = append(args, *f.APIKeyID)
		argIdx++
	}
	if f.UserID != nil {
		fmt.Fprintf(sb, " AND k.user_id = $%d", argIdx)
		args = append(args, *f.UserID)
		argIdx++
	}
	if f.Model != nil {
		fmt.Fprintf(sb, " AND ul.model = $%d", argIdx)
		args = append(args, *f.Model)
		argIdx++
	}
	return args, argIdx
}

// GetUsageSummary returns a single aggregated summary row for the window.
func (s *Store) GetUsageSummary(f UsageFilter) (UsageSummary, error) {
	var sb strings.Builder
	sb.WriteString(
		`SELECT
		   COUNT(*),
		   COALESCE(SUM(ul.prompt_tokens), 0),
		   COALESCE(SUM(ul.completion_tokens), 0),
		   COALESCE(SUM(ul.total_tokens), 0),
		   COALESCE(SUM(ul.credits_charged), 0),
		   COALESCE(AVG(ul.duration_ms), 0),
		   COALESCE(SUM(CASE WHEN ul.status='error' THEN 1 ELSE 0 END), 0)
		 FROM usage_logs ul
		 JOIN api_keys k ON ul.api_key_id = k.id
		 WHERE 1=1`)
	args, _ := buildUsageFilterClause(&sb, nil, f, 1)

	var summary UsageSummary
	err := s.pool.QueryRow(context.Background(), sb.String(), args...).Scan(
		&summary.Requests,
		&summary.PromptTokens,
		&summary.CompletionTokens,
		&summary.TotalTokens,
		&summary.Credits,
		&summary.AvgDurationMs,
		&summary.Errors,
	)
	if err != nil {
		return UsageSummary{}, fmt.Errorf("usage summary: %w", err)
	}
	return summary, nil
}

// GetUsageByModel returns per-model aggregates, ordered by total_tokens desc.
func (s *Store) GetUsageByModel(f UsageFilter) ([]ModelUsageRow, error) {
	var sb strings.Builder
	sb.WriteString(
		`SELECT
		   ul.model,
		   COUNT(*),
		   COALESCE(SUM(ul.total_tokens), 0),
		   COALESCE(SUM(ul.credits_charged), 0),
		   COALESCE(AVG(ul.duration_ms), 0)
		 FROM usage_logs ul
		 JOIN api_keys k ON ul.api_key_id = k.id
		 WHERE 1=1`)
	args, _ := buildUsageFilterClause(&sb, nil, f, 1)
	sb.WriteString(` GROUP BY ul.model ORDER BY COALESCE(SUM(ul.total_tokens), 0) DESC`)

	rows, err := s.pool.Query(context.Background(), sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage by model: %w", err)
	}
	defer rows.Close()

	out := make([]ModelUsageRow, 0)
	for rows.Next() {
		var r ModelUsageRow
		if err := rows.Scan(&r.Model, &r.Requests, &r.TotalTokens, &r.Credits, &r.AvgDurationMs); err != nil {
			return nil, fmt.Errorf("scan model row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetUsageByUser returns per-owner aggregates, ordered by total_tokens desc.
// "Owner" is the human user if the key has a user_id, otherwise the service
// account if the key has only an account_id, otherwise neither (legacy
// admin-created keys).
func (s *Store) GetUsageByUser(f UsageFilter) ([]OwnerUsageRow, error) {
	var sb strings.Builder
	sb.WriteString(
		`SELECT
		   k.user_id,
		   usr.email,
		   usr.name,
		   k.account_id,
		   a.name,
		   a.type,
		   COUNT(*),
		   COALESCE(SUM(ul.total_tokens), 0),
		   COALESCE(SUM(ul.credits_charged), 0),
		   COUNT(DISTINCT k.id)
		 FROM usage_logs ul
		 JOIN api_keys k ON ul.api_key_id = k.id
		 LEFT JOIN users usr ON usr.id = k.user_id
		 LEFT JOIN accounts a ON a.id = k.account_id
		 WHERE 1=1`)
	args, _ := buildUsageFilterClause(&sb, nil, f, 1)
	sb.WriteString(
		` GROUP BY k.user_id, usr.email, usr.name, k.account_id, a.name, a.type
		  ORDER BY COALESCE(SUM(ul.total_tokens), 0) DESC`)

	rows, err := s.pool.Query(context.Background(), sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage by user: %w", err)
	}
	defer rows.Close()

	out := make([]OwnerUsageRow, 0)
	for rows.Next() {
		var r OwnerUsageRow
		if err := rows.Scan(&r.UserID, &r.Email, &r.Name, &r.AccountID, &r.AccountName, &r.AccountType,
			&r.Requests, &r.TotalTokens, &r.Credits, &r.KeyCount); err != nil {
			return nil, fmt.Errorf("scan owner row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetUsageTimeseries returns time-bucketed aggregates. interval must be
// "hour" or "day". Gap-filling is the caller's responsibility.
func (s *Store) GetUsageTimeseries(f UsageFilter, interval string) ([]TimeseriesBucket, error) {
	// Validate interval against a small allowlist — date_trunc accepts many
	// values but handler contract is hour/day.
	switch interval {
	case "hour", "day":
	default:
		return nil, fmt.Errorf("invalid interval %q: must be 'hour' or 'day'", interval)
	}

	var sb strings.Builder
	// date_trunc on a TIMESTAMPTZ operates in the session TimeZone, so without
	// AT TIME ZONE 'UTC' a non-UTC session (e.g. America/Los_Angeles) returns
	// bucket boundaries that are UTC-misaligned. The handler gap-fill keys on
	// UTC times, so misaligned buckets would silently drop real rows. Keeping
	// the truncation explicitly UTC removes that coupling; the pool also pins
	// session TZ to UTC as defense in depth.
	sb.WriteString(
		`SELECT
		   date_trunc($1, ul.created_at AT TIME ZONE 'UTC'),
		   COUNT(*),
		   COALESCE(SUM(ul.prompt_tokens), 0),
		   COALESCE(SUM(ul.completion_tokens), 0),
		   COALESCE(SUM(ul.total_tokens), 0),
		   COALESCE(SUM(ul.credits_charged), 0),
		   COALESCE(SUM(CASE WHEN ul.status='error' THEN 1 ELSE 0 END), 0)
		 FROM usage_logs ul
		 JOIN api_keys k ON ul.api_key_id = k.id
		 WHERE 1=1`)

	args := []any{interval}
	args, _ = buildUsageFilterClause(&sb, args, f, 2)
	sb.WriteString(` GROUP BY 1 ORDER BY 1`)

	rows, err := s.pool.Query(context.Background(), sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage timeseries: %w", err)
	}
	defer rows.Close()

	out := make([]TimeseriesBucket, 0)
	for rows.Next() {
		var b TimeseriesBucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.PromptTokens, &b.CompletionTokens,
			&b.TotalTokens, &b.Credits, &b.Errors); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RegistrationEvent is one row of the admin /registrations feed. Enriched
// with user email/name and account name/type via LEFT JOIN so the admin UI
// can render owner info without extra round trips.
type RegistrationEvent struct {
	ID                  int64
	Kind                string
	Source              string
	UserID              *int64
	UserEmail           *string
	UserName            *string
	AccountID           *int64
	AccountName         *string
	AccountType         *string
	RegistrationTokenID *int64
	Metadata            []byte // raw JSONB, may be nil
	CreatedAt           time.Time
}

// ListRegistrationEvents returns a page of registration_events rows ordered
// newest-first, enriched with the related user + account fields. Returns
// (rows, total, err) where total is the pre-slice count for pagination.
func (s *Store) ListRegistrationEvents(limit, offset int) ([]RegistrationEvent, int, error) {
	ctx := context.Background()

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM registration_events`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count registration_events: %w", err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT e.id, e.kind, e.source,
		        e.user_id, u.email, u.name,
		        e.account_id, a.name, a.type,
		        e.registration_token_id, e.metadata, e.created_at
		 FROM registration_events e
		 LEFT JOIN users u    ON u.id = e.user_id
		 LEFT JOIN accounts a ON a.id = e.account_id
		 ORDER BY e.created_at DESC, e.id DESC
		 LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list registration_events: %w", err)
	}
	defer rows.Close()

	out := make([]RegistrationEvent, 0)
	for rows.Next() {
		var ev RegistrationEvent
		if err := rows.Scan(
			&ev.ID, &ev.Kind, &ev.Source,
			&ev.UserID, &ev.UserEmail, &ev.UserName,
			&ev.AccountID, &ev.AccountName, &ev.AccountType,
			&ev.RegistrationTokenID, &ev.Metadata, &ev.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scan registration_event: %w", err)
		}
		out = append(out, ev)
	}
	return out, total, rows.Err()
}

// BackfillRegistrationEvents inserts a registration_events row with
// source='backfill' for every existing users/accounts row that is not yet
// tracked. Idempotent: uses WHERE NOT EXISTS guards.
func (s *Store) BackfillRegistrationEvents() error {
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, user_id, source, created_at)
		 SELECT 'user', u.account_id, u.id, 'backfill', u.created_at
		 FROM users u
		 WHERE NOT EXISTS (
		   SELECT 1 FROM registration_events e
		   WHERE e.user_id = u.id AND e.kind = 'user'
		 )`,
	); err != nil {
		return fmt.Errorf("backfill user events: %w", err)
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, source, created_at)
		 SELECT 'service', a.id, 'backfill', a.created_at
		 FROM accounts a
		 WHERE a.type = 'service'
		   AND NOT EXISTS (
		     SELECT 1 FROM registration_events e
		     WHERE e.account_id = a.id AND e.kind = 'service'
		   )`,
	); err != nil {
		return fmt.Errorf("backfill service events: %w", err)
	}
	return nil
}

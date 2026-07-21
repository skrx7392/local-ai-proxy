package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Credit requests: auto-filed "account hit its monthly allowance" records
// (docs/design/credit-requests.md). Filed by the cap-hit recorder on the 402
// paths, listed/resolved by the admin API and the Discord bot.

var (
	// ErrCreditRequestNotFound: no credit request with that id.
	ErrCreditRequestNotFound = errors.New("credit request not found")
	// ErrCreditRequestResolved: the request was already granted or dismissed.
	ErrCreditRequestResolved = errors.New("credit request already resolved")
)

// CreditRequest is one cap-hit record.
type CreditRequest struct {
	ID           int64
	AccountID    int64
	Period       time.Time // first day of the UTC month
	Status       string    // pending | granted | dismissed
	CreatedAt    time.Time
	ResolvedAt   *time.Time
	ResolvedNote *string
}

// CreditRequestRow adds the display fields the admin listing joins in.
type CreditRequestRow struct {
	CreditRequest
	AccountName  string
	Email        *string  // oldest federated identity, nil when none
	MonthlyGrant *float64 // per-account override; nil = env default applies
	Balance      float64
}

// CreditRequestInfo is the payload data for a cap-hit notification.
type CreditRequestInfo struct {
	AccountID    int64
	Email        *string
	DisplayName  *string
	MonthlyGrant *float64 // override; nil = env default applies
	Balance      float64
	Period       time.Time
}

// FileCreditRequest files a cap-hit request for the account's current UTC
// month unless one is pending (dedupe) or dismissed (silenced for the
// month). Granted rows do not block: an account that burned through a top-up
// may legitimately need to ask again. Returns (id, true) when this call
// filed the request. Race-safe: the partial unique index on pending rows
// elects a single winner among concurrent 402s.
func (s *Store) FileCreditRequest(accountID int64, now time.Time) (int64, bool, error) {
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)

	var id int64
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO credit_requests (account_id, period)
		 SELECT $1, $2
		 WHERE NOT EXISTS (
		     SELECT 1 FROM credit_requests
		     WHERE account_id = $1 AND period = $2 AND status IN ('pending', 'dismissed')
		 )
		 ON CONFLICT DO NOTHING
		 RETURNING id`,
		accountID, month,
	).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("file credit request: %w", err)
	}
	return id, true, nil
}

// ListCreditRequests returns requests with the given status, newest first,
// joined with account display data for the admin listing.
func (s *Store) ListCreditRequests(status string) ([]CreditRequestRow, error) {
	rows, err := s.pool.Query(context.Background(),
		`SELECT cr.id, cr.account_id, cr.period, cr.status, cr.created_at,
		        cr.resolved_at, cr.resolved_note,
		        a.name, fi.email, a.monthly_grant, COALESCE(cb.balance, 0)
		 FROM credit_requests cr
		 JOIN accounts a ON a.id = cr.account_id
		 LEFT JOIN credit_balances cb ON cb.account_id = cr.account_id
		 LEFT JOIN LATERAL (
		     SELECT email FROM federated_identities
		     WHERE account_id = cr.account_id ORDER BY id LIMIT 1
		 ) fi ON TRUE
		 WHERE cr.status = $1
		 ORDER BY cr.created_at DESC, cr.id DESC`,
		status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CreditRequestRow
	for rows.Next() {
		var r CreditRequestRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Period, &r.Status, &r.CreatedAt,
			&r.ResolvedAt, &r.ResolvedNote,
			&r.AccountName, &r.Email, &r.MonthlyGrant, &r.Balance); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveCreditRequest moves a pending request to granted or dismissed.
// Resolution never moves money itself — callers grant credits through the
// audited grant path first, then mark the request.
func (s *Store) ResolveCreditRequest(id int64, status, note string) error {
	ct, err := s.pool.Exec(context.Background(),
		`UPDATE credit_requests
		 SET status = $2, resolved_at = NOW(), resolved_note = NULLIF($3, '')
		 WHERE id = $1 AND status = 'pending'`,
		id, status, note,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() > 0 {
		return nil
	}

	// Distinguish "no such request" from "already resolved".
	var current string
	err = s.pool.QueryRow(context.Background(),
		`SELECT status FROM credit_requests WHERE id = $1`, id,
	).Scan(&current)
	if err == pgx.ErrNoRows {
		return ErrCreditRequestNotFound
	}
	if err != nil {
		return err
	}
	return ErrCreditRequestResolved
}

// GetCreditRequestInfo loads the notification payload data for a request.
func (s *Store) GetCreditRequestInfo(id int64) (*CreditRequestInfo, error) {
	var info CreditRequestInfo
	err := s.pool.QueryRow(context.Background(),
		`SELECT cr.account_id, fi.email, fi.display_name,
		        a.monthly_grant, COALESCE(cb.balance, 0), cr.period
		 FROM credit_requests cr
		 JOIN accounts a ON a.id = cr.account_id
		 LEFT JOIN credit_balances cb ON cb.account_id = cr.account_id
		 LEFT JOIN LATERAL (
		     SELECT email, display_name FROM federated_identities
		     WHERE account_id = cr.account_id ORDER BY id LIMIT 1
		 ) fi ON TRUE
		 WHERE cr.id = $1`,
		id,
	).Scan(&info.AccountID, &info.Email, &info.DisplayName,
		&info.MonthlyGrant, &info.Balance, &info.Period)
	if err == pgx.ErrNoRows {
		return nil, ErrCreditRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	return &info, nil
}

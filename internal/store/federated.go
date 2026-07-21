package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// FederatedIdentity is an end-user identity forwarded by a trusted key
// (docs/design/end-user-accounts.md). (Source, ExternalID) is the authority;
// Email and DisplayName are display metadata refreshed on every sight.
type FederatedIdentity struct {
	Source      string
	ExternalID  string
	Email       string
	DisplayName string
}

// EndUserResolution reports what ResolveEndUserAccount did.
type EndUserResolution struct {
	AccountID        int64
	Provisioned      bool // account was created by this call
	AllowanceGranted bool // monthly allowance was applied by this call
}

// accountName picks the display name for an auto-provisioned account:
// email, else display name, else a source-qualified external id.
func (f FederatedIdentity) accountName() string {
	if f.Email != "" {
		return f.Email
	}
	if f.DisplayName != "" {
		return f.DisplayName
	}
	return fmt.Sprintf("%s:%s", f.Source, f.ExternalID)
}

// ResolveEndUserAccount maps a federated identity to its billing account,
// provisioning the account on first sight and applying the monthly allowance
// when due. Safe under concurrent first sight: the UNIQUE(source, external_id)
// constraint elects one winner and losers adopt its account; the allowance
// top-up is guarded so exactly one grant lands per account per month.
//
// defaultGrant is the env-level monthly allowance, overridden per account by
// accounts.monthly_grant (explicit 0 = blocked). now is injected for
// testability; callers pass time.Now().
func (s *Store) ResolveEndUserAccount(id FederatedIdentity, defaultGrant float64, now time.Time) (EndUserResolution, error) {
	var res EndUserResolution
	ctx := context.Background()

	accountID, err := s.lookupFederatedAccount(ctx, id)
	if err != nil {
		return res, err
	}
	if accountID == 0 {
		accountID, res.Provisioned, err = s.provisionFederatedAccount(ctx, id)
		if err != nil {
			return res, err
		}
	}
	res.AccountID = accountID

	res.AllowanceGranted, err = s.applyMonthlyAllowance(ctx, accountID, defaultGrant, now)
	if err != nil {
		return res, err
	}
	return res, nil
}

// lookupFederatedAccount returns the existing account for an identity
// (refreshing its metadata), or 0 when the identity is unknown.
func (s *Store) lookupFederatedAccount(ctx context.Context, id FederatedIdentity) (int64, error) {
	var accountID int64
	err := s.pool.QueryRow(ctx,
		`UPDATE federated_identities
		 SET email = $3, display_name = $4, last_seen_at = NOW()
		 WHERE source = $1 AND external_id = $2
		 RETURNING account_id`,
		id.Source, id.ExternalID, id.Email, id.DisplayName,
	).Scan(&accountID)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return accountID, nil
}

// provisionFederatedAccount creates account + zero balance + identity +
// registration event in one transaction. On an identity-insert conflict
// (concurrent first sight) the whole transaction rolls back — discarding the
// orphan account — and the winner's account is adopted.
func (s *Store) provisionFederatedAccount(ctx context.Context, id FederatedIdentity) (accountID int64, provisioned bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`INSERT INTO accounts (name, type, allowance_managed) VALUES ($1, 'end_user', TRUE) RETURNING id`,
		id.accountName(),
	).Scan(&accountID)
	if err != nil {
		return 0, false, err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO credit_balances (account_id, balance, reserved) VALUES ($1, 0, 0)`,
		accountID,
	); err != nil {
		return 0, false, err
	}

	var identityID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO federated_identities (source, external_id, account_id, email, display_name)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (source, external_id) DO NOTHING
		 RETURNING id`,
		id.Source, id.ExternalID, accountID, id.Email, id.DisplayName,
	).Scan(&identityID)
	if err == pgx.ErrNoRows {
		// Lost the race: roll back our provisional rows, adopt the winner's account.
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return 0, false, rbErr
		}
		winner, lookErr := s.lookupFederatedAccount(ctx, id)
		if lookErr != nil {
			return 0, false, lookErr
		}
		if winner == 0 {
			return 0, false, fmt.Errorf("federated identity %s:%s vanished after conflict", id.Source, id.ExternalID)
		}
		return winner, false, nil
	}
	if err != nil {
		return 0, false, err
	}

	if _, err = tx.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, source, metadata)
		 VALUES ('user', $1, 'trusted_header', $2)`,
		accountID,
		map[string]any{"source": id.Source, "external_id": id.ExternalID, "email": id.Email},
	); err != nil {
		return 0, false, err
	}

	if err = tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return accountID, true, nil
}

// applyMonthlyAllowance resets the balance to the account's grant on its
// first activity of a new UTC month. The guarded UPDATE elects exactly one
// winner under concurrency; the reset (not +=) is what makes the grant a
// monthly spend cap — unspent allowance does not roll over. In-flight holds
// (reserved) are deliberately untouched.
func (s *Store) applyMonthlyAllowance(ctx context.Context, accountID int64, defaultGrant float64, now time.Time) (bool, error) {
	var managed bool
	var override *float64
	err := s.pool.QueryRow(ctx,
		`SELECT allowance_managed, monthly_grant FROM accounts WHERE id = $1`,
		accountID,
	).Scan(&managed, &override)
	if err != nil {
		return false, err
	}
	if !managed {
		return false, nil
	}
	grant := defaultGrant
	if override != nil {
		grant = *override
	}
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx,
		`UPDATE credit_balances
		 SET balance = $2, allowance_period = $3, updated_at = NOW()
		 WHERE account_id = $1
		   AND (allowance_period IS NULL OR allowance_period < $3)`,
		accountID, grant, month,
	)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil // already granted this month (possibly by a concurrent request)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO credit_transactions (account_id, amount, balance_after, type, description)
		 VALUES ($1, $2, $2, 'monthly_allowance', $3)`,
		accountID, grant, fmt.Sprintf("monthly allowance %s", month.Format("2006-01")),
	); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// SetMonthlyGrant sets (or, with nil, clears back to the env default) an
// account's monthly allowance override. Takes effect at the next monthly
// reset, not retroactively.
func (s *Store) SetMonthlyGrant(accountID int64, grant *float64) error {
	ct, err := s.pool.Exec(context.Background(),
		`UPDATE accounts SET monthly_grant = $2 WHERE id = $1`,
		accountID, grant,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

// SetTrustUserHeaders flips identity-header trust on a key. Admin API only.
func (s *Store) SetTrustUserHeaders(keyID int64, trust bool) error {
	ct, err := s.pool.Exec(context.Background(),
		`UPDATE api_keys SET trust_user_headers = $2 WHERE id = $1`,
		keyID, trust,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

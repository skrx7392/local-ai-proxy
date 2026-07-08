package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AdminServiceAccountName is the well-known name of the service account that
// admin-minted API keys attach to when no explicit account_id is given, and
// that legacy NULL-account keys are backfilled onto at startup.
const AdminServiceAccountName = "admin-service"

// adminServiceAccountLockKey serializes concurrent EnsureAdminServiceAccount
// calls (e.g. two pods booting at once) so exactly one account is created.
// Distinct from the migrate lock key in store.go.
const adminServiceAccountLockKey int64 = 917230424

// EnsureAdminServiceAccount returns the ID of the designated admin service
// account, creating it on first call with a credit balance and the given
// initial grant. The grant applies only at creation time — subsequent calls
// never re-grant or top up. Idempotent and safe under concurrent startup.
func (s *Store) EnsureAdminServiceAccount(initialGrant float64) (int64, error) {
	ctx := context.Background()

	// Fast path: the account almost always exists already.
	id, err := s.findAdminServiceAccount(ctx)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("lookup admin service account: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize creation across processes, then re-check under the lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, adminServiceAccountLockKey); err != nil {
		return 0, fmt.Errorf("acquire admin account lock: %w", err)
	}
	err = tx.QueryRow(ctx,
		`SELECT id FROM accounts WHERE name = $1 AND type = 'service' ORDER BY id LIMIT 1`,
		AdminServiceAccountName,
	).Scan(&id)
	if err == nil {
		return id, tx.Commit(ctx)
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("re-check admin service account: %w", err)
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO accounts (name, type) VALUES ($1, 'service') RETURNING id`,
		AdminServiceAccountName,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create admin service account: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO credit_balances (account_id, balance) VALUES ($1, $2)`,
		id, initialGrant,
	); err != nil {
		return 0, fmt.Errorf("init admin service balance: %w", err)
	}

	if initialGrant > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO credit_transactions (account_id, amount, balance_after, type, description)
			 VALUES ($1, $2, $2, 'grant', 'admin service account initial grant')`,
			id, initialGrant,
		); err != nil {
			return 0, fmt.Errorf("insert admin service grant transaction: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, source)
		 VALUES ('service', $1, 'admin_service')`,
		id,
	); err != nil {
		return 0, fmt.Errorf("record admin service registration_event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit admin service account: %w", err)
	}
	return id, nil
}

func (s *Store) findAdminServiceAccount(ctx context.Context) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM accounts WHERE name = $1 AND type = 'service' ORDER BY id LIMIT 1`,
		AdminServiceAccountName,
	).Scan(&id)
	return id, err
}

// BackfillAdminKeyAccounts attaches every api_key that still has a NULL
// account_id to an account, so the credit gate can meter it: keys owned by a
// user go to that user's account; the rest (legacy admin-created keys) go to
// the admin service account. Returns the number of keys attached.
// Idempotent — safe to call on every startup. Runs after BackfillAccounts,
// which guarantees every user has an account.
func (s *Store) BackfillAdminKeyAccounts(adminAccountID int64) (int64, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// User-owned keys reattach to their owner's account.
	ctUser, err := tx.Exec(ctx,
		`UPDATE api_keys k SET account_id = u.account_id
		 FROM users u
		 WHERE k.user_id = u.id AND k.account_id IS NULL AND u.account_id IS NOT NULL`,
	)
	if err != nil {
		return 0, fmt.Errorf("attach user keys to owner accounts: %w", err)
	}

	// Everything left over (admin-created keys, user_id IS NULL) goes to the
	// admin service account.
	ctAdmin, err := tx.Exec(ctx,
		`UPDATE api_keys SET account_id = $1 WHERE account_id IS NULL`,
		adminAccountID,
	)
	if err != nil {
		return 0, fmt.Errorf("attach legacy keys to admin service account: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit backfill: %w", err)
	}
	return ctUser.RowsAffected() + ctAdmin.RowsAffected(), nil
}

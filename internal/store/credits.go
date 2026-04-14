package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetAccountCreditStatus returns account active status and credit balance in one query.
// Used by CreditGate middleware for fast pre-check.
func (s *Store) GetAccountCreditStatus(accountID int64) (isActive bool, balance, reserved float64, err error) {
	err = s.pool.QueryRow(
		context.Background(),
		`SELECT a.is_active, cb.balance, cb.reserved
		 FROM accounts a
		 JOIN credit_balances cb ON cb.account_id = a.id
		 WHERE a.id = $1`, accountID,
	).Scan(&isActive, &balance, &reserved)
	if err == pgx.ErrNoRows {
		return false, 0, 0, fmt.Errorf("account or balance not found")
	}
	return
}

// GetCreditBalance returns the credit balance for an account.
func (s *Store) GetCreditBalance(accountID int64) (*CreditBalance, error) {
	var cb CreditBalance
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT account_id, balance, reserved, updated_at
		 FROM credit_balances WHERE account_id = $1`, accountID,
	).Scan(&cb.AccountID, &cb.Balance, &cb.Reserved, &cb.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cb, nil
}

// InitCreditBalance creates a zero-balance row for an account. Idempotent.
func (s *Store) InitCreditBalance(accountID int64) error {
	_, err := s.pool.Exec(
		context.Background(),
		`INSERT INTO credit_balances (account_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		accountID,
	)
	return err
}

// BackfillCreditBalances creates credit_balances rows for accounts that lack one.
func (s *Store) BackfillCreditBalances() error {
	_, err := s.pool.Exec(
		context.Background(),
		`INSERT INTO credit_balances (account_id)
		 SELECT id FROM accounts WHERE id NOT IN (SELECT account_id FROM credit_balances)
		 ON CONFLICT DO NOTHING`,
	)
	return err
}

// AddCredits grants credits to an account. Transactional: updates balance and inserts audit trail.
func (s *Store) AddCredits(accountID int64, amount float64, description string) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var newBalance float64
	err = tx.QueryRow(ctx,
		`UPDATE credit_balances SET balance = balance + $1, updated_at = NOW()
		 WHERE account_id = $2 RETURNING balance`,
		amount, accountID,
	).Scan(&newBalance)
	if err != nil {
		return fmt.Errorf("update balance: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO credit_transactions (account_id, amount, balance_after, type, description)
		 VALUES ($1, $2, $3, 'grant', $4)`,
		accountID, amount, newBalance, description,
	)
	if err != nil {
		return fmt.Errorf("insert transaction: %w", err)
	}

	return tx.Commit(ctx)
}

// ReserveCredits atomically reserves credits for an in-flight request.
// Returns the hold ID. Returns error if insufficient balance.
func (s *Store) ReserveCredits(accountID int64, amount float64) (int64, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the balance row and check availability
	var balance, reserved float64
	err = tx.QueryRow(ctx,
		`SELECT balance, reserved FROM credit_balances
		 WHERE account_id = $1 FOR UPDATE`, accountID,
	).Scan(&balance, &reserved)
	if err != nil {
		return 0, fmt.Errorf("lock balance: %w", err)
	}

	available := balance - reserved
	if available < amount {
		return 0, fmt.Errorf("insufficient credits: available %.6f, requested %.6f", available, amount)
	}

	// Insert the hold
	var holdID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO credit_holds (account_id, amount) VALUES ($1, $2) RETURNING id`,
		accountID, amount,
	).Scan(&holdID)
	if err != nil {
		return 0, fmt.Errorf("insert hold: %w", err)
	}

	// Update reserved amount
	_, err = tx.Exec(ctx,
		`UPDATE credit_balances SET reserved = reserved + $1, updated_at = NOW()
		 WHERE account_id = $2`, amount, accountID,
	)
	if err != nil {
		return 0, fmt.Errorf("update reserved: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return holdID, nil
}

// SettleHold settles a credit hold with the actual cost and returns the cost
// that was actually charged. When the hold was already released by the
// sweeper, returns (0, nil) so callers can record "no cost charged" without
// special-casing. Normal path returns (actualAmount, nil).
func (s *Store) SettleHold(holdID int64, actualAmount float64) (float64, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Atomically mark hold as settled and get its details
	var accountID int64
	var holdAmount float64
	err = tx.QueryRow(ctx,
		`UPDATE credit_holds SET status = 'settled', settled_at = NOW()
		 WHERE id = $1 AND status = 'pending'
		 RETURNING account_id, amount`, holdID,
	).Scan(&accountID, &holdAmount)
	if err == pgx.ErrNoRows {
		// Sweeper already released this hold — nothing charged.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("update hold: %w", err)
	}

	// Update balance: deduct actual cost, release reservation
	var newBalance float64
	err = tx.QueryRow(ctx,
		`UPDATE credit_balances
		 SET balance = balance - $1, reserved = reserved - $2, updated_at = NOW()
		 WHERE account_id = $3
		 RETURNING balance`, actualAmount, holdAmount, accountID,
	).Scan(&newBalance)
	if err != nil {
		return 0, fmt.Errorf("update balance: %w", err)
	}

	// Audit trail
	_, err = tx.Exec(ctx,
		`INSERT INTO credit_transactions (account_id, amount, balance_after, type, reference_id, description)
		 VALUES ($1, $2, $3, 'usage', $4, 'request settlement')`,
		accountID, -actualAmount, newBalance, holdID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert transaction: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return actualAmount, nil
}

// ReleaseHold releases a pending hold without charging. Used when requests fail with 0 bytes.
func (s *Store) ReleaseHold(holdID int64) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var accountID int64
	var holdAmount float64
	err = tx.QueryRow(ctx,
		`UPDATE credit_holds SET status = 'released', settled_at = NOW()
		 WHERE id = $1 AND status = 'pending'
		 RETURNING account_id, amount`, holdID,
	).Scan(&accountID, &holdAmount)
	if err == pgx.ErrNoRows {
		return nil // already settled or released
	}
	if err != nil {
		return fmt.Errorf("update hold: %w", err)
	}

	_, err = tx.Exec(ctx,
		`UPDATE credit_balances SET reserved = reserved - $1, updated_at = NOW()
		 WHERE account_id = $2`, holdAmount, accountID,
	)
	if err != nil {
		return fmt.Errorf("update reserved: %w", err)
	}

	return tx.Commit(ctx)
}

// SweepStaleHolds releases pending holds older than the given duration.
// Returns the number of holds released.
func (s *Store) SweepStaleHolds(olderThan time.Duration) (int, error) {
	ctx := context.Background()
	cutoff := time.Now().Add(-olderThan)

	rows, err := s.pool.Query(ctx,
		`SELECT id FROM credit_holds WHERE status = 'pending' AND created_at < $1`, cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("query stale holds: %w", err)
	}
	defer rows.Close()

	var holdIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan hold id: %w", err)
		}
		holdIDs = append(holdIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	released := 0
	for _, id := range holdIDs {
		if err := s.ReleaseHold(id); err != nil {
			return released, fmt.Errorf("release hold %d: %w", id, err)
		}
		released++
	}
	return released, nil
}

// CleanupSettledHolds deletes settled/released holds older than the given duration.
func (s *Store) CleanupSettledHolds(olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	ct, err := s.pool.Exec(
		context.Background(),
		`DELETE FROM credit_holds WHERE status IN ('settled', 'released') AND settled_at < $1`, cutoff,
	)
	if err != nil {
		return 0, err
	}
	return int(ct.RowsAffected()), nil
}

// GetCreditTransactions returns paginated transaction history for an account.
func (s *Store) GetCreditTransactions(accountID int64, limit, offset int) ([]CreditTransaction, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, account_id, amount, balance_after, type, reference_id, description, created_at
		 FROM credit_transactions WHERE account_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		accountID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txns []CreditTransaction
	for rows.Next() {
		var t CreditTransaction
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Amount, &t.BalanceAfter,
			&t.Type, &t.ReferenceID, &t.Description, &t.CreatedAt); err != nil {
			return nil, err
		}
		txns = append(txns, t)
	}
	return txns, rows.Err()
}

// GetPricingByModel returns pricing for a specific model ID.
func (s *Store) GetPricingByModel(modelID string) (*CreditPricing, error) {
	var p CreditPricing
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT id, model_id, prompt_rate, completion_rate, typical_completion, effective_from, active
		 FROM credit_pricing WHERE model_id = $1 AND active = TRUE`, modelID,
	).Scan(&p.ID, &p.ModelID, &p.PromptRate, &p.CompletionRate, &p.TypicalCompletion, &p.EffectiveFrom, &p.Active)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListActivePricing returns all active pricing rules.
func (s *Store) ListActivePricing() ([]CreditPricing, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, model_id, prompt_rate, completion_rate, typical_completion, effective_from, active
		 FROM credit_pricing WHERE active = TRUE ORDER BY model_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pricing []CreditPricing
	for rows.Next() {
		var p CreditPricing
		if err := rows.Scan(&p.ID, &p.ModelID, &p.PromptRate, &p.CompletionRate,
			&p.TypicalCompletion, &p.EffectiveFrom, &p.Active); err != nil {
			return nil, err
		}
		pricing = append(pricing, p)
	}
	return pricing, rows.Err()
}

// UpsertPricing creates or updates a pricing rule.
func (s *Store) UpsertPricing(modelID string, promptRate, completionRate float64, typicalCompletion int) error {
	_, err := s.pool.Exec(
		context.Background(),
		`INSERT INTO credit_pricing (model_id, prompt_rate, completion_rate, typical_completion)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (model_id) DO UPDATE SET
		   prompt_rate = EXCLUDED.prompt_rate,
		   completion_rate = EXCLUDED.completion_rate,
		   typical_completion = EXCLUDED.typical_completion,
		   active = TRUE`,
		modelID, promptRate, completionRate, typicalCompletion,
	)
	return err
}

// DeletePricing deactivates a pricing rule.
func (s *Store) DeletePricing(id int64) error {
	ct, err := s.pool.Exec(
		context.Background(),
		`UPDATE credit_pricing SET active = FALSE WHERE id = $1`, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("pricing not found")
	}
	return nil
}

// GetAccountUsageStats returns historical usage stats for an account+model.
func (s *Store) GetAccountUsageStats(accountID int64, model string) (*AccountUsageStats, error) {
	var stats AccountUsageStats
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT account_id, model, avg_completion_tokens, request_count, updated_at
		 FROM account_usage_stats WHERE account_id = $1 AND model = $2`,
		accountID, model,
	).Scan(&stats.AccountID, &stats.Model, &stats.AvgCompletionTokens, &stats.RequestCount, &stats.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &stats, nil
}

// UpdateAccountUsageStats atomically updates the running average completion tokens.
func (s *Store) UpdateAccountUsageStats(accountID int64, model string, completionTokens int) error {
	_, err := s.pool.Exec(
		context.Background(),
		`INSERT INTO account_usage_stats (account_id, model, avg_completion_tokens, request_count, updated_at)
		 VALUES ($1, $2, $3, 1, NOW())
		 ON CONFLICT (account_id, model) DO UPDATE SET
		   avg_completion_tokens = (account_usage_stats.avg_completion_tokens * account_usage_stats.request_count + $3) / (account_usage_stats.request_count + 1),
		   request_count = account_usage_stats.request_count + 1,
		   updated_at = NOW()`,
		accountID, model, completionTokens,
	)
	return err
}

// CreateRegistrationToken inserts a new registration token.
func (s *Store) CreateRegistrationToken(name, tokenHash string, creditGrant float64, maxUses int, expiresAt *time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO registration_tokens (name, token_hash, credit_grant, max_uses, expires_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, tokenHash, creditGrant, maxUses, expiresAt,
	).Scan(&id)
	return id, err
}

// ListRegistrationTokens returns all registration tokens.
func (s *Store) ListRegistrationTokens() ([]RegistrationToken, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, name, token_hash, credit_grant, max_uses, uses, created_at, expires_at, revoked
		 FROM registration_tokens ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []RegistrationToken
	for rows.Next() {
		var t RegistrationToken
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenHash, &t.CreditGrant, &t.MaxUses, &t.Uses,
			&t.CreatedAt, &t.ExpiresAt, &t.Revoked); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeRegistrationToken marks a token as revoked.
func (s *Store) RevokeRegistrationToken(id int64) error {
	ct, err := s.pool.Exec(context.Background(),
		`UPDATE registration_tokens SET revoked = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("registration token not found")
	}
	return nil
}

// RegisterServiceAccount atomically: validates+consumes token, creates service account,
// inits credit balance, grants credits, creates API key, and records a
// registration_events audit row with source='registration_token'.
// Returns (accountID, keyID, creditGrant, err).
func (s *Store) RegisterServiceAccount(tokenHash, name, keyHash, keyPrefix string, rateLimit int) (int64, int64, float64, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Atomically validate and consume the token
	var tokenID int64
	var creditGrant float64
	err = tx.QueryRow(ctx,
		`UPDATE registration_tokens
		 SET uses = uses + 1
		 WHERE token_hash = $1 AND NOT revoked AND uses < max_uses
		   AND (expires_at IS NULL OR expires_at > NOW())
		 RETURNING id, credit_grant`, tokenHash,
	).Scan(&tokenID, &creditGrant)
	if err == pgx.ErrNoRows {
		return 0, 0, 0, fmt.Errorf("invalid, expired, or exhausted registration token")
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("consume token: %w", err)
	}

	// Create service account
	var accountID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO accounts (name, type) VALUES ($1, 'service') RETURNING id`, name,
	).Scan(&accountID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("create account: %w", err)
	}

	// Init credit balance
	_, err = tx.Exec(ctx,
		`INSERT INTO credit_balances (account_id) VALUES ($1)`, accountID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("init balance: %w", err)
	}

	// Grant credits from token
	if creditGrant > 0 {
		_, err = tx.Exec(ctx,
			`UPDATE credit_balances SET balance = $1, updated_at = NOW() WHERE account_id = $2`,
			creditGrant, accountID)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("grant credits: %w", err)
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO credit_transactions (account_id, amount, balance_after, type, description)
			 VALUES ($1, $2, $2, 'grant', 'registration token grant')`,
			accountID, creditGrant)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("insert grant transaction: %w", err)
		}
	}

	// Create API key
	var keyID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO api_keys (name, key_hash, key_prefix, rate_limit, account_id)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, keyHash, keyPrefix, rateLimit, accountID,
	).Scan(&keyID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("create key: %w", err)
	}

	if _, err = tx.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, registration_token_id, source)
		 VALUES ('service', $1, $2, 'registration_token')`,
		accountID, tokenID,
	); err != nil {
		return 0, 0, 0, fmt.Errorf("record registration_event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, fmt.Errorf("commit: %w", err)
	}
	return accountID, keyID, creditGrant, nil
}

// SetSessionTokenLimit sets the session token limit for a key. Use nil to remove the limit.
func (s *Store) SetSessionTokenLimit(keyID int64, limit *int) error {
	ct, err := s.pool.Exec(context.Background(),
		`UPDATE api_keys SET session_token_limit = $1 WHERE id = $2`, limit, keyID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("key not found")
	}
	return nil
}

// ListAccountsWithBalances returns all accounts with their credit balances.
func (s *Store) ListAccountsWithBalances() ([]struct {
	Account
	Balance  float64
	Reserved float64
}, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT a.id, a.name, a.type, a.is_active, a.created_at,
		        COALESCE(cb.balance, 0), COALESCE(cb.reserved, 0)
		 FROM accounts a
		 LEFT JOIN credit_balances cb ON cb.account_id = a.id
		 ORDER BY a.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type AccountWithBalance struct {
		Account
		Balance  float64
		Reserved float64
	}
	var results []struct {
		Account
		Balance  float64
		Reserved float64
	}
	for rows.Next() {
		var r struct {
			Account
			Balance  float64
			Reserved float64
		}
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.IsActive, &r.CreatedAt,
			&r.Balance, &r.Reserved); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetSessionTokenUsage returns total tokens consumed by a key within a time window,
// plus the oldest usage entry timestamp (for Retry-After calculation).
func (s *Store) GetSessionTokenUsage(keyID int64, window time.Duration) (consumed int, oldest *time.Time, err error) {
	cutoff := time.Now().Add(-window)
	err = s.pool.QueryRow(
		context.Background(),
		`SELECT COALESCE(SUM(total_tokens), 0), MIN(created_at)
		 FROM usage_logs
		 WHERE api_key_id = $1 AND created_at > $2`,
		keyID, cutoff,
	).Scan(&consumed, &oldest)
	return
}

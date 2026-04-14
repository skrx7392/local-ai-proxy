package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrEmailExists is returned by user-creation helpers when the email is
// already taken, so callers can map to 409 without inspecting SQL errors.
var ErrEmailExists = errors.New("email already exists")

// ErrLastActiveAdmin is returned when a mutation would leave the system
// without any active admin (role='admin' AND is_active=TRUE). Callers
// map this to 409 "last_admin".
var ErrLastActiveAdmin = errors.New("cannot remove the last active admin")

// ErrUserNotFound is returned by mutation helpers when the target row
// does not exist.
var ErrUserNotFound = errors.New("user not found")

// pgUniqueViolation is the Postgres SQLSTATE code for unique constraint
// violations. Used to distinguish "email already taken" from other errors.
const pgUniqueViolation = "23505"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

//go:embed schema.sql
var schemaSql string

type Store struct {
	pool *pgxpool.Pool
}

type APIKey struct {
	ID                int64
	Name              string
	KeyHash           string
	KeyPrefix         string
	RateLimit         int
	CreatedAt         time.Time
	Revoked           bool
	UserID            *int64 // nil = legacy admin-created key
	AccountID         *int64 // nil = legacy key not on credit system
	SessionTokenLimit *int   // nil = no session limit
}

type Account struct {
	ID        int64
	Name      string
	Type      string // "personal" or "service"
	IsActive  bool
	CreatedAt time.Time
}

type UsageEntry struct {
	APIKeyID         int64
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
	Status           string  // completed | partial | error
	CreditsCharged   float64 // 0 when no hold/pricing was active
}

type UsageStat struct {
	APIKeyID        int64
	KeyName         string
	Model           string
	TotalRequests   int
	TotalPrompt     int
	TotalCompletion int
	TotalTokens     int
	Status          string
}

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	Name         string
	Role         string // "user" or "admin"
	IsActive     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	AccountID    *int64
}

type CreditBalance struct {
	AccountID int64
	Balance   float64
	Reserved  float64
	UpdatedAt time.Time
}

type CreditTransaction struct {
	ID           int64
	AccountID    int64
	Amount       float64
	BalanceAfter float64
	Type         string // "grant", "usage", "refund", "adjustment"
	ReferenceID  *int64
	Description  *string
	CreatedAt    time.Time
}

type CreditHold struct {
	ID        int64
	AccountID int64
	Amount    float64
	Status    string // "pending", "settled", "released"
	CreatedAt time.Time
	SettledAt *time.Time
}

type CreditPricing struct {
	ID                int64
	ModelID           string
	PromptRate        float64
	CompletionRate    float64
	TypicalCompletion int
	EffectiveFrom     time.Time
	Active            bool
}

type AccountUsageStats struct {
	AccountID           int64
	Model               string
	AvgCompletionTokens int
	RequestCount        int
	UpdatedAt           time.Time
}

type RegistrationToken struct {
	ID          int64
	Name        string
	TokenHash   string
	CreditGrant float64
	MaxUses     int
	Uses        int
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	Revoked     bool
}

type Session struct {
	ID        int64
	UserID    int64
	TokenHash string
	ExpiresAt time.Time
	CreatedAt time.Time
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// Pin every pooled connection to UTC so date_trunc, CURRENT_DATE, NOW()
	// and other time-returning functions are session-TZ-independent. Without
	// this, analytics bucket boundaries would drift based on the database
	// role or server default.
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET TIME ZONE 'UTC'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	dataStore := &Store{pool: pool}
	if err := dataStore.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return dataStore, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

// Ping verifies the database connection is alive.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Pool returns the underlying connection pool (for test cleanup).
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *Store) migrate(ctx context.Context) error {
	// Serialize concurrent migrations against the same database. Without
	// this, parallel test binaries or pods booting simultaneously can race
	// on Postgres catalog inserts for CREATE TABLE IF NOT EXISTS and fail
	// with "duplicate key value violates unique constraint
	// pg_type_typname_nsp_index" or "deadlock detected". A session-scoped
	// advisory lock blocks until acquired; we retry briefly on rare
	// catalog races that slip through.
	const lockKey int64 = 917230423
	const maxAttempts = 5

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := func() error {
			conn, err := s.pool.Acquire(ctx)
			if err != nil {
				return fmt.Errorf("acquire conn: %w", err)
			}
			defer conn.Release()

			if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
				return fmt.Errorf("acquire migrate lock: %w", err)
			}
			defer func() {
				_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey)
			}()

			if _, err := conn.Exec(ctx, schemaSql); err != nil {
				return fmt.Errorf("exec migration: %w", err)
			}
			return nil
		}()

		if err == nil {
			return nil
		}
		if !isTransientMigrateError(err) {
			return err
		}
		lastErr = err
		// Brief backoff before retry to let the racing session commit.
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return lastErr
}

// isTransientMigrateError reports whether err is one of the catalog-race
// errors worth retrying during migrate.
func isTransientMigrateError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case pgUniqueViolation, // 23505
		"40P01", // deadlock_detected
		"42710", // duplicate_object (CREATE INDEX concurrent race)
		"55000": // object_not_in_prerequisite_state
		return true
	}
	return false
}

// CreateKey inserts a new API key and returns its ID.
func (s *Store) CreateKey(name, keyHash, keyPrefix string, rateLimit int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO api_keys (name, key_hash, key_prefix, rate_limit) VALUES ($1, $2, $3, $4) RETURNING id`,
		name, keyHash, keyPrefix, rateLimit,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetKeyByHash looks up an active (non-revoked) key by its SHA-256 hash.
func (s *Store) GetKeyByHash(hash string) (*APIKey, error) {
	var apiKey APIKey
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT id, name, key_hash, key_prefix, rate_limit, created_at, revoked, user_id, account_id, session_token_limit
		 FROM api_keys WHERE key_hash = $1 AND revoked = FALSE`, hash,
	).Scan(&apiKey.ID, &apiKey.Name, &apiKey.KeyHash, &apiKey.KeyPrefix, &apiKey.RateLimit, &apiKey.CreatedAt, &apiKey.Revoked,
		&apiKey.UserID, &apiKey.AccountID, &apiKey.SessionTokenLimit)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &apiKey, nil
}

// ListKeys returns all API keys (for admin display).
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, name, key_prefix, rate_limit, created_at, revoked, user_id, account_id, session_token_limit FROM api_keys ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var apiKey APIKey
		if err := rows.Scan(&apiKey.ID, &apiKey.Name, &apiKey.KeyPrefix, &apiKey.RateLimit, &apiKey.CreatedAt, &apiKey.Revoked,
			&apiKey.UserID, &apiKey.AccountID, &apiKey.SessionTokenLimit); err != nil {
			return nil, err
		}
		keys = append(keys, apiKey)
	}
	return keys, rows.Err()
}

// RevokeKey soft-deletes a key by setting revoked = TRUE.
func (s *Store) RevokeKey(id int64) error {
	ct, err := s.pool.Exec(context.Background(), `UPDATE api_keys SET revoked = TRUE WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("key not found")
	}
	return nil
}

// LogUsage inserts a usage log entry. Called from the async writer goroutine.
func (s *Store) LogUsage(entry UsageEntry) error {
	_, err := s.pool.Exec(
		context.Background(),
		`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		entry.APIKeyID, entry.Model, entry.PromptTokens, entry.CompletionTokens,
		entry.TotalTokens, entry.DurationMs, entry.Status, entry.CreditsCharged,
	)
	return err
}

// CreateUser inserts a new user and returns their ID.
func (s *Store) CreateUser(email, passwordHash, name string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO users (email, password_hash, name) VALUES ($1, $2, $3) RETURNING id`,
		email, passwordHash, name,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// GetUserByEmail looks up a user by email address.
func (s *Store) GetUserByEmail(email string) (*User, error) {
	var u User
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT id, email, password_hash, name, role, is_active, created_at, updated_at, account_id
		 FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.IsActive, &u.CreatedAt, &u.UpdatedAt, &u.AccountID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID looks up a user by ID.
func (s *Store) GetUserByID(id int64) (*User, error) {
	var u User
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT id, email, password_hash, name, role, is_active, created_at, updated_at, account_id
		 FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.IsActive, &u.CreatedAt, &u.UpdatedAt, &u.AccountID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// UpdateUserProfile updates a user's name and email.
func (s *Store) UpdateUserProfile(id int64, name, email string) error {
	ct, err := s.pool.Exec(
		context.Background(),
		`UPDATE users SET name = $1, email = $2, updated_at = NOW() WHERE id = $3`,
		name, email, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

// UpdateUserPassword updates a user's password hash.
func (s *Store) UpdateUserPassword(id int64, passwordHash string) error {
	ct, err := s.pool.Exec(
		context.Background(),
		`UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`,
		passwordHash, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

// ListUsers returns all users (for admin use).
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, email, password_hash, name, role, is_active, created_at, updated_at, account_id FROM users ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.IsActive, &u.CreatedAt, &u.UpdatedAt, &u.AccountID); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetUserActive activates or deactivates a user.
func (s *Store) SetUserActive(id int64, active bool) error {
	ct, err := s.pool.Exec(
		context.Background(),
		`UPDATE users SET is_active = $1, updated_at = NOW() WHERE id = $2`,
		active, id,
	)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

// DeactivateUserGuarded deactivates a user, refusing to do so if that would
// leave zero active admins. Two concurrent calls targeting different admin
// rows cannot both succeed — they serialize on a Postgres advisory lock so
// the second transaction sees the first's decrement before its COUNT.
func (s *Store) DeactivateUserGuarded(id int64) error {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Serialize all admin-affecting mutations against each other. Plain
	// users/scripts that aren't admins proceed concurrently.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('admin_mutations'))`); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}

	var currentRole string
	var currentActive bool
	err = tx.QueryRow(ctx,
		`SELECT role, is_active FROM users WHERE id = $1 FOR UPDATE`, id,
	).Scan(&currentRole, &currentActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}

	// If the target is an active admin, make sure at least one other remains.
	if currentRole == "admin" && currentActive {
		var activeAdmins int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM users WHERE role = 'admin' AND is_active = TRUE`,
		).Scan(&activeAdmins); err != nil {
			return fmt.Errorf("count active admins: %w", err)
		}
		if activeAdmins <= 1 {
			return ErrLastActiveAdmin
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET is_active = FALSE, updated_at = NOW() WHERE id = $1`, id,
	); err != nil {
		return fmt.Errorf("deactivate: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// CreateSession inserts a new session record.
func (s *Store) CreateSession(userID int64, tokenHash string, expiresAt time.Time) error {
	_, err := s.pool.Exec(
		context.Background(),
		`INSERT INTO user_sessions (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, tokenHash, expiresAt,
	)
	return err
}

// GetSessionByTokenHash looks up a session by its token hash.
func (s *Store) GetSessionByTokenHash(hash string) (*Session, error) {
	var sess Session
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT id, user_id, token_hash, expires_at, created_at
		 FROM user_sessions WHERE token_hash = $1`, hash,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.ExpiresAt, &sess.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// DeleteSession removes a session by its token hash.
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.pool.Exec(
		context.Background(),
		`DELETE FROM user_sessions WHERE token_hash = $1`, tokenHash,
	)
	return err
}

// DeleteUserSessions removes all sessions for a user.
func (s *Store) DeleteUserSessions(userID int64) error {
	_, err := s.pool.Exec(
		context.Background(),
		`DELETE FROM user_sessions WHERE user_id = $1`, userID,
	)
	return err
}

// CreateKeyForUser creates an API key owned by a user.
func (s *Store) CreateKeyForUser(userID int64, name, keyHash, keyPrefix string, rateLimit int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO api_keys (name, key_hash, key_prefix, rate_limit, user_id) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, keyHash, keyPrefix, rateLimit, userID,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListKeysByUser returns all API keys belonging to a user.
func (s *Store) ListKeysByUser(userID int64) ([]APIKey, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, name, key_hash, key_prefix, rate_limit, created_at, revoked, user_id, account_id, session_token_limit
		 FROM api_keys WHERE user_id = $1 ORDER BY id`, userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.RateLimit, &k.CreatedAt, &k.Revoked,
			&k.UserID, &k.AccountID, &k.SessionTokenLimit); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CreateAccount inserts a new account and returns its ID.
func (s *Store) CreateAccount(name, accountType string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO accounts (name, type) VALUES ($1, $2) RETURNING id`,
		name, accountType,
	).Scan(&id)
	return id, err
}

// GetAccountByID looks up an account by ID.
func (s *Store) GetAccountByID(id int64) (*Account, error) {
	var a Account
	err := s.pool.QueryRow(
		context.Background(),
		`SELECT id, name, type, is_active, created_at FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.Type, &a.IsActive, &a.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAccounts returns all accounts.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.pool.Query(
		context.Background(),
		`SELECT id, name, type, is_active, created_at FROM accounts ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.IsActive, &a.CreatedAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// SetAccountActive activates or deactivates an account.
func (s *Store) SetAccountActive(id int64, active bool) error {
	ct, err := s.pool.Exec(context.Background(), `UPDATE accounts SET is_active = $1 WHERE id = $2`, active, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("account not found")
	}
	return nil
}

// CreateKeyForAccount creates an API key for an account with both user_id and account_id.
func (s *Store) CreateKeyForAccount(userID, accountID int64, name, keyHash, keyPrefix string, rateLimit int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO api_keys (name, key_hash, key_prefix, rate_limit, user_id, account_id) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		name, keyHash, keyPrefix, rateLimit, userID, accountID,
	).Scan(&id)
	return id, err
}

// CreateKeyForAccountOnly creates an API key for an account without a user (admin/service created).
func (s *Store) CreateKeyForAccountOnly(accountID int64, name, keyHash, keyPrefix string, rateLimit int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(
		context.Background(),
		`INSERT INTO api_keys (name, key_hash, key_prefix, rate_limit, account_id) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		name, keyHash, keyPrefix, rateLimit, accountID,
	).Scan(&id)
	return id, err
}

// RegisterUser atomically creates a personal account and user in one transaction.
// Returns (accountID, userID, err). Also records a registration_events audit
// row with source='public_signup' so the admin registrations view reflects
// self-serve signups without an additional code path.
func (s *Store) RegisterUser(email, passwordHash, name string) (int64, int64, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var accountID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO accounts (name, type) VALUES ($1, 'personal') RETURNING id`, name,
	).Scan(&accountID)
	if err != nil {
		return 0, 0, fmt.Errorf("create account: %w", err)
	}

	var userID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, name, account_id) VALUES ($1, $2, $3, $4) RETURNING id`,
		email, passwordHash, name, accountID,
	).Scan(&userID)
	if err != nil {
		return 0, 0, fmt.Errorf("create user: %w", err)
	}

	// Initialize credit balance for the new account
	_, err = tx.Exec(ctx,
		`INSERT INTO credit_balances (account_id) VALUES ($1) ON CONFLICT DO NOTHING`, accountID)
	if err != nil {
		return 0, 0, fmt.Errorf("init credit balance: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, user_id, source)
		 VALUES ('user', $1, $2, 'public_signup')`,
		accountID, userID,
	); err != nil {
		return 0, 0, fmt.Errorf("record registration_event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return accountID, userID, nil
}

// CreateAdminBootstrap atomically creates an admin user together with its
// personal account, initial credit balance, and a registration_events audit
// row (source='admin_bootstrap'). Returns the new user ID.
//
// On a duplicate email, returns ErrEmailExists so callers can map to 409
// without inspecting SQLSTATE themselves.
func (s *Store) CreateAdminBootstrap(email, passwordHash, name string) (int64, error) {
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var accountID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO accounts (name, type) VALUES ($1, 'personal') RETURNING id`, name,
	).Scan(&accountID); err != nil {
		return 0, fmt.Errorf("create account: %w", err)
	}

	var userID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, name, role, is_active, account_id)
		 VALUES ($1, $2, $3, 'admin', TRUE, $4) RETURNING id`,
		email, passwordHash, name, accountID,
	).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrEmailExists
		}
		return 0, fmt.Errorf("create user: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO credit_balances (account_id) VALUES ($1) ON CONFLICT DO NOTHING`,
		accountID,
	); err != nil {
		return 0, fmt.Errorf("init credit balance: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO registration_events (kind, account_id, user_id, source)
		 VALUES ('user', $1, $2, 'admin_bootstrap')`,
		accountID, userID,
	); err != nil {
		return 0, fmt.Errorf("record registration_event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return userID, nil
}

// BackfillAccounts creates personal accounts for existing users without one,
// and updates their keys' account_id. Idempotent — safe to call on every startup.
func (s *Store) BackfillAccounts() error {
	ctx := context.Background()

	// Find users without an account_id
	rows, err := s.pool.Query(ctx,
		`SELECT id, name FROM users WHERE account_id IS NULL`)
	if err != nil {
		return fmt.Errorf("query users without account: %w", err)
	}
	defer rows.Close()

	type userInfo struct {
		id   int64
		name string
	}
	var users []userInfo
	for rows.Next() {
		var u userInfo
		if err := rows.Scan(&u.id, &u.name); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate users: %w", err)
	}

	for _, u := range users {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx for user %d: %w", u.id, err)
		}

		var accountID int64
		err = tx.QueryRow(ctx,
			`INSERT INTO accounts (name, type) VALUES ($1, 'personal') RETURNING id`, u.name,
		).Scan(&accountID)
		if err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("create account for user %d: %w", u.id, err)
		}

		_, err = tx.Exec(ctx,
			`UPDATE users SET account_id = $1 WHERE id = $2`, accountID, u.id)
		if err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("set user %d account_id: %w", u.id, err)
		}

		_, err = tx.Exec(ctx,
			`UPDATE api_keys SET account_id = $1 WHERE user_id = $2 AND account_id IS NULL`,
			accountID, u.id)
		if err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("set keys account_id for user %d: %w", u.id, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit backfill for user %d: %w", u.id, err)
		}
	}
	return nil
}

// GetUsageStats returns aggregated usage statistics.
func (s *Store) GetUsageStats(keyID *int64, since *time.Time) ([]UsageStat, error) {
	query := `SELECT u.api_key_id, k.name, u.model, COUNT(*) as total_requests,
		SUM(u.prompt_tokens), SUM(u.completion_tokens), SUM(u.total_tokens), u.status
		FROM usage_logs u JOIN api_keys k ON u.api_key_id = k.id
		WHERE 1=1`
	args := []any{}
	argIdx := 1

	if keyID != nil {
		query += fmt.Sprintf(` AND u.api_key_id = $%d`, argIdx)
		args = append(args, *keyID)
		argIdx++
	}
	if since != nil {
		query += fmt.Sprintf(` AND u.created_at >= $%d`, argIdx)
		args = append(args, *since)
		argIdx++
	}

	query += ` GROUP BY u.api_key_id, k.name, u.model, u.status ORDER BY u.api_key_id, u.model`

	rows, err := s.pool.Query(context.Background(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []UsageStat
	for rows.Next() {
		var usageStat UsageStat
		if err := rows.Scan(&usageStat.APIKeyID, &usageStat.KeyName, &usageStat.Model, &usageStat.TotalRequests,
			&usageStat.TotalPrompt, &usageStat.TotalCompletion, &usageStat.TotalTokens, &usageStat.Status); err != nil {
			return nil, err
		}
		stats = append(stats, usageStat)
	}
	return stats, rows.Err()
}

package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

type APIKey struct {
	ID        int64
	Name      string
	KeyHash   string
	KeyPrefix string
	RateLimit int
	CreatedAt time.Time
	Revoked   bool
}

type UsageEntry struct {
	APIKeyID        int64
	Model           string
	PromptTokens    int
	CompletionTokens int
	TotalTokens     int
	DurationMs      int64
	Status          string // completed | partial | error
}

type UsageStat struct {
	APIKeyID         int64
	KeyName          string
	Model            string
	TotalRequests    int
	TotalPrompt      int
	TotalCompletion  int
	TotalTokens      int
	Status           string
}

func New(dbPath string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", dbPath)

	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read db: %w", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)

	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		readDB.Close()
		return nil, fmt.Errorf("open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	s := &Store{readDB: readDB, writeDB: writeDB}
	if err := s.migrate(); err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	s.readDB.Close()
	s.writeDB.Close()
	return nil
}

func (s *Store) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS api_keys (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT    NOT NULL,
			key_hash   TEXT    NOT NULL UNIQUE,
			key_prefix TEXT    NOT NULL,
			rate_limit INTEGER NOT NULL DEFAULT 60,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			revoked    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			api_key_id        INTEGER NOT NULL REFERENCES api_keys(id),
			model             TEXT    NOT NULL,
			prompt_tokens     INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens      INTEGER NOT NULL DEFAULT 0,
			duration_ms       INTEGER NOT NULL DEFAULT 0,
			status            TEXT    NOT NULL DEFAULT 'completed',
			created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_key_id ON usage_logs(api_key_id)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_created ON usage_logs(created_at)`,
	}

	for _, m := range migrations {
		if _, err := s.writeDB.Exec(m); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	return nil
}

// CreateKey inserts a new API key and returns its ID.
func (s *Store) CreateKey(name, keyHash, keyPrefix string, rateLimit int) (int64, error) {
	res, err := s.writeDB.Exec(
		`INSERT INTO api_keys (name, key_hash, key_prefix, rate_limit) VALUES (?, ?, ?, ?)`,
		name, keyHash, keyPrefix, rateLimit,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetKeyByHash looks up an active (non-revoked) key by its SHA-256 hash.
func (s *Store) GetKeyByHash(hash string) (*APIKey, error) {
	var k APIKey
	var revoked int
	err := s.readDB.QueryRow(
		`SELECT id, name, key_hash, key_prefix, rate_limit, created_at, revoked
		 FROM api_keys WHERE key_hash = ? AND revoked = 0`, hash,
	).Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.RateLimit, &k.CreatedAt, &revoked)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Revoked = revoked != 0
	return &k, nil
}

// ListKeys returns all API keys (for admin display).
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.readDB.Query(
		`SELECT id, name, key_prefix, rate_limit, created_at, revoked FROM api_keys ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var revoked int
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.RateLimit, &k.CreatedAt, &revoked); err != nil {
			return nil, err
		}
		k.Revoked = revoked != 0
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// RevokeKey soft-deletes a key by setting revoked = 1.
func (s *Store) RevokeKey(id int64) error {
	res, err := s.writeDB.Exec(`UPDATE api_keys SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("key not found")
	}
	return nil
}

// LogUsage inserts a usage log entry. Called from the async writer goroutine.
func (s *Store) LogUsage(entry UsageEntry) error {
	_, err := s.writeDB.Exec(
		`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.APIKeyID, entry.Model, entry.PromptTokens, entry.CompletionTokens,
		entry.TotalTokens, entry.DurationMs, entry.Status,
	)
	return err
}

// GetUsageStats returns aggregated usage statistics.
func (s *Store) GetUsageStats(keyID *int64, since *time.Time) ([]UsageStat, error) {
	query := `SELECT u.api_key_id, k.name, u.model, COUNT(*) as total_requests,
		SUM(u.prompt_tokens), SUM(u.completion_tokens), SUM(u.total_tokens), u.status
		FROM usage_logs u JOIN api_keys k ON u.api_key_id = k.id
		WHERE 1=1`
	args := []any{}

	if keyID != nil {
		query += ` AND u.api_key_id = ?`
		args = append(args, *keyID)
	}
	if since != nil {
		query += ` AND u.created_at >= ?`
		args = append(args, *since)
	}

	query += ` GROUP BY u.api_key_id, u.model, u.status ORDER BY u.api_key_id, u.model`

	rows, err := s.readDB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []UsageStat
	for rows.Next() {
		var st UsageStat
		if err := rows.Scan(&st.APIKeyID, &st.KeyName, &st.Model, &st.TotalRequests,
			&st.TotalPrompt, &st.TotalCompletion, &st.TotalTokens, &st.Status); err != nil {
			return nil, err
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

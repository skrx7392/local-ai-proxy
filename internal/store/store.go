package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSql string

type Store struct {
	pool *pgxpool.Pool
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
	APIKeyID         int64
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMs       int64
	Status           string // completed | partial | error
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

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
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

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schemaSql)
	if err != nil {
		return fmt.Errorf("exec migration: %w", err)
	}
	return nil
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
		`SELECT id, name, key_hash, key_prefix, rate_limit, created_at, revoked
		 FROM api_keys WHERE key_hash = $1 AND revoked = FALSE`, hash,
	).Scan(&apiKey.ID, &apiKey.Name, &apiKey.KeyHash, &apiKey.KeyPrefix, &apiKey.RateLimit, &apiKey.CreatedAt, &apiKey.Revoked)
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
		`SELECT id, name, key_prefix, rate_limit, created_at, revoked FROM api_keys ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var apiKey APIKey
		if err := rows.Scan(&apiKey.ID, &apiKey.Name, &apiKey.KeyPrefix, &apiKey.RateLimit, &apiKey.CreatedAt, &apiKey.Revoked); err != nil {
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
		`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
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

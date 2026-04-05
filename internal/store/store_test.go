package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping store integration test")
	}

	ctx := context.Background()
	s, err := New(ctx, dbURL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() {
		// Drop tables so each test starts clean
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS usage_logs")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS api_keys")
		s.Close()
	})

	// Ensure clean state
	_, _ = s.pool.Exec(ctx, "DELETE FROM usage_logs")
	_, _ = s.pool.Exec(ctx, "DELETE FROM api_keys")

	return s
}

func TestNew(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	s, err := New(ctx, dbURL)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS usage_logs")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS api_keys")
		s.Close()
	})

	// Verify migration ran by checking that api_keys table exists
	var exists bool
	err = s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'api_keys')`).Scan(&exists)
	if err != nil {
		t.Fatalf("query table existence: %v", err)
	}
	if !exists {
		t.Fatal("expected api_keys table to exist after migration")
	}
}

func TestNewInvalidURL(t *testing.T) {
	ctx := context.Background()
	_, err := New(ctx, "postgres://invalid:invalid@localhost:59999/nonexistent?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("expected error for invalid database URL")
	}
}

func TestCreateKey(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateKey("test-key", "hash123", "sk-abc", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
}

func TestGetKeyByHash(t *testing.T) {
	s := setupTestStore(t)

	_, err := s.CreateKey("lookup-key", "hash-lookup", "sk-look", 30)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	key, err := s.GetKeyByHash("hash-lookup")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if key == nil {
		t.Fatal("expected key, got nil")
	}
	if key.Name != "lookup-key" {
		t.Errorf("expected name 'lookup-key', got %q", key.Name)
	}
	if key.RateLimit != 30 {
		t.Errorf("expected rate limit 30, got %d", key.RateLimit)
	}
	if key.Revoked {
		t.Error("expected revoked=false")
	}
}

func TestGetKeyByHash_NotFound(t *testing.T) {
	s := setupTestStore(t)

	key, err := s.GetKeyByHash("nonexistent-hash")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if key != nil {
		t.Fatalf("expected nil for nonexistent hash, got %+v", key)
	}
}

func TestGetKeyByHash_RevokedReturnsNil(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateKey("revokable", "hash-revoke", "sk-rev", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	err = s.RevokeKey(id)
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	key, err := s.GetKeyByHash("hash-revoke")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if key != nil {
		t.Fatal("expected nil for revoked key")
	}
}

func TestListKeys(t *testing.T) {
	s := setupTestStore(t)

	_, _ = s.CreateKey("key-a", "hash-a", "sk-aaa", 10)
	_, _ = s.CreateKey("key-b", "hash-b", "sk-bbb", 20)

	keys, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys[0].Name != "key-a" {
		t.Errorf("expected first key 'key-a', got %q", keys[0].Name)
	}
	if keys[1].Name != "key-b" {
		t.Errorf("expected second key 'key-b', got %q", keys[1].Name)
	}
}

func TestRevokeKey(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("to-revoke", "hash-rev", "sk-rev", 60)

	err := s.RevokeKey(id)
	if err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	// Verify it shows as revoked in the list
	keys, _ := s.ListKeys()
	for _, k := range keys {
		if k.ID == id && !k.Revoked {
			t.Error("expected key to be revoked")
		}
	}
}

func TestRevokeKey_NotFound(t *testing.T) {
	s := setupTestStore(t)

	err := s.RevokeKey(999999)
	if err == nil {
		t.Fatal("expected error for revoking non-existent key")
	}
	if err.Error() != "key not found" {
		t.Errorf("expected 'key not found' error, got: %v", err)
	}
}

func TestLogUsage(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("usage-key", "hash-usage", "sk-usg", 60)

	err := s.LogUsage(UsageEntry{
		APIKeyID:         id,
		Model:            "llama3",
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		DurationMs:       1234,
		Status:           "completed",
	})
	if err != nil {
		t.Fatalf("LogUsage: %v", err)
	}
}

func TestGetUsageStats_NoFilters(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("stats-key", "hash-stats", "sk-sta", 60)

	_ = s.LogUsage(UsageEntry{
		APIKeyID: id, Model: "llama3",
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		DurationMs: 100, Status: "completed",
	})
	_ = s.LogUsage(UsageEntry{
		APIKeyID: id, Model: "llama3",
		PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30,
		DurationMs: 200, Status: "completed",
	})

	stats, err := s.GetUsageStats(nil, nil)
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("expected at least one stat row")
	}
	if stats[0].TotalRequests != 2 {
		t.Errorf("expected 2 requests, got %d", stats[0].TotalRequests)
	}
	if stats[0].TotalTokens != 45 {
		t.Errorf("expected 45 total tokens, got %d", stats[0].TotalTokens)
	}
}

func TestGetUsageStats_WithKeyFilter(t *testing.T) {
	s := setupTestStore(t)

	id1, _ := s.CreateKey("key-1", "hash-1", "sk-111", 60)
	id2, _ := s.CreateKey("key-2", "hash-2", "sk-222", 60)

	_ = s.LogUsage(UsageEntry{APIKeyID: id1, Model: "llama3", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, DurationMs: 100, Status: "completed"})
	_ = s.LogUsage(UsageEntry{APIKeyID: id2, Model: "llama3", PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30, DurationMs: 200, Status: "completed"})

	stats, err := s.GetUsageStats(&id1, nil)
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 stat row for key filter, got %d", len(stats))
	}
	if stats[0].APIKeyID != id1 {
		t.Errorf("expected key ID %d, got %d", id1, stats[0].APIKeyID)
	}
}

func TestGetUsageStats_WithSinceFilter(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("since-key", "hash-since", "sk-sin", 60)

	_ = s.LogUsage(UsageEntry{APIKeyID: id, Model: "llama3", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, DurationMs: 100, Status: "completed"})

	// Query with a time in the past — should include the entry
	past := time.Now().Add(-1 * time.Hour)
	stats, err := s.GetUsageStats(nil, &past)
	if err != nil {
		t.Fatalf("GetUsageStats with since: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("expected stats with past since filter")
	}

	// Query with a time in the future — should exclude everything
	future := time.Now().Add(1 * time.Hour)
	stats, err = s.GetUsageStats(nil, &future)
	if err != nil {
		t.Fatalf("GetUsageStats with future since: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats with future since filter, got %d", len(stats))
	}
}

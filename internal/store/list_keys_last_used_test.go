package store

import (
	"context"
	"testing"
	"time"
)

// TestListKeys_LastUsedAt verifies that ListKeys derives last_used_at from the
// usage_logs table via a MAX(created_at) join: a key with usage reports the
// most recent request time, and a key that never served traffic reports nil.
func TestListKeys_LastUsedAt(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	acctID, err := s.CreateAccount("svc-lastused", "service")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	usedKey, err := s.CreateKeyForAccountOnly(acctID, "used-key", "hash-used", "sk-used", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly used: %v", err)
	}
	unusedKey, err := s.CreateKeyForAccountOnly(acctID, "unused-key", "hash-unused", "sk-unus", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly unused: %v", err)
	}

	// Two usage rows for the used key — the newer timestamp must win. Truncate
	// to whole seconds so the Postgres round-trip compares cleanly with .Equal.
	older := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	newest := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	for _, ts := range []time.Time{older, newest} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, model, total_tokens, status, created_at)
			 VALUES ($1, 'llama3.1:8b', 10, 'completed', $2)`, usedKey, ts,
		); err != nil {
			t.Fatalf("insert usage row: %v", err)
		}
	}

	keys, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}

	byID := make(map[int64]APIKey, len(keys))
	for _, k := range keys {
		byID[k.ID] = k
	}

	used, ok := byID[usedKey]
	if !ok {
		t.Fatalf("used key %d missing from ListKeys result", usedKey)
	}
	if used.LastUsedAt == nil {
		t.Fatal("expected LastUsedAt to be set for a key with usage, got nil")
	}
	if !used.LastUsedAt.Equal(newest) {
		t.Errorf("expected LastUsedAt %v (the most recent usage), got %v", newest, *used.LastUsedAt)
	}

	unused, ok := byID[unusedKey]
	if !ok {
		t.Fatalf("unused key %d missing from ListKeys result", unusedKey)
	}
	if unused.LastUsedAt != nil {
		t.Errorf("expected nil LastUsedAt for a key with no usage, got %v", *unused.LastUsedAt)
	}
}

// TestGetKeyByID_LastUsedAt verifies the single-key detail path derives
// last_used_at the same way the list path does.
func TestGetKeyByID_LastUsedAt(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	acctID, err := s.CreateAccount("svc-detail-lastused", "service")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	keyID, err := s.CreateKeyForAccountOnly(acctID, "detail-key", "hash-detail", "sk-det", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly: %v", err)
	}

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO usage_logs (api_key_id, model, total_tokens, status, created_at)
		 VALUES ($1, 'llama3.1:8b', 5, 'completed', $2)`, keyID, ts,
	); err != nil {
		t.Fatalf("insert usage row: %v", err)
	}

	k, err := s.GetKeyByID(keyID)
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if k == nil {
		t.Fatal("expected key, got nil")
	}
	if k.LastUsedAt == nil {
		t.Fatal("expected LastUsedAt to be set, got nil")
	}
	if !k.LastUsedAt.Equal(ts) {
		t.Errorf("expected LastUsedAt %v, got %v", ts, *k.LastUsedAt)
	}

	// A freshly created key with no usage reports nil.
	freshID, err := s.CreateKeyForAccountOnly(acctID, "fresh-key", "hash-fresh", "sk-frs", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly fresh: %v", err)
	}
	fresh, err := s.GetKeyByID(freshID)
	if err != nil {
		t.Fatalf("GetKeyByID fresh: %v", err)
	}
	if fresh.LastUsedAt != nil {
		t.Errorf("expected nil LastUsedAt for unused key, got %v", *fresh.LastUsedAt)
	}
}

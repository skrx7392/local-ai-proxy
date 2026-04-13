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
		// Drop tables in FK dependency order
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS registration_events")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_holds")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_transactions")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS account_usage_stats")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_balances")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_pricing")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS registration_tokens")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS usage_logs")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS user_sessions")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS user_id")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS account_id")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS session_token_limit")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS api_keys")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE users DROP COLUMN IF EXISTS account_id")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS users")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS accounts")
		s.Close()
	})

	// Ensure clean state
	_, _ = s.pool.Exec(ctx, "DELETE FROM registration_events")
	_, _ = s.pool.Exec(ctx, "DELETE FROM credit_holds")
	_, _ = s.pool.Exec(ctx, "DELETE FROM credit_transactions")
	_, _ = s.pool.Exec(ctx, "DELETE FROM account_usage_stats")
	_, _ = s.pool.Exec(ctx, "DELETE FROM credit_balances")
	_, _ = s.pool.Exec(ctx, "DELETE FROM credit_pricing")
	_, _ = s.pool.Exec(ctx, "DELETE FROM registration_tokens")
	_, _ = s.pool.Exec(ctx, "DELETE FROM usage_logs")
	_, _ = s.pool.Exec(ctx, "DELETE FROM user_sessions")
	_, _ = s.pool.Exec(ctx, "DELETE FROM api_keys")
	_, _ = s.pool.Exec(ctx, "DELETE FROM users")
	_, _ = s.pool.Exec(ctx, "DELETE FROM accounts")

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
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS registration_events")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_holds")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_transactions")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS account_usage_stats")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_balances")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS credit_pricing")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS registration_tokens")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS usage_logs")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS user_sessions")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS user_id")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS account_id")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE api_keys DROP COLUMN IF EXISTS session_token_limit")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS api_keys")
		_, _ = s.pool.Exec(context.Background(), "ALTER TABLE users DROP COLUMN IF EXISTS account_id")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS users")
		_, _ = s.pool.Exec(context.Background(), "DROP TABLE IF EXISTS accounts")
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

// --- User tests ---

func TestCreateUser(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateUser("alice@example.com", "hashed-pw", "Alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
}

func TestCreateUser_DuplicateEmail(t *testing.T) {
	s := setupTestStore(t)

	_, err := s.CreateUser("dup@example.com", "hashed-pw", "First")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = s.CreateUser("dup@example.com", "hashed-pw-2", "Second")
	if err == nil {
		t.Fatal("expected error for duplicate email")
	}
}

func TestGetUserByEmail(t *testing.T) {
	s := setupTestStore(t)

	_, err := s.CreateUser("bob@example.com", "hashed-pw", "Bob")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := s.GetUserByEmail("bob@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user == nil {
		t.Fatal("expected user, got nil")
	}
	if user.Email != "bob@example.com" {
		t.Errorf("expected email 'bob@example.com', got %q", user.Email)
	}
	if user.Name != "Bob" {
		t.Errorf("expected name 'Bob', got %q", user.Name)
	}
	if user.Role != "user" {
		t.Errorf("expected role 'user', got %q", user.Role)
	}
	if !user.IsActive {
		t.Error("expected is_active=true")
	}
}

func TestGetUserByEmail_NotFound(t *testing.T) {
	s := setupTestStore(t)

	user, err := s.GetUserByEmail("nonexistent@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if user != nil {
		t.Fatalf("expected nil for nonexistent email, got %+v", user)
	}
}

func TestGetUserByID(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateUser("carol@example.com", "hashed-pw", "Carol")

	user, err := s.GetUserByID(id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user == nil {
		t.Fatal("expected user, got nil")
	}
	if user.ID != id {
		t.Errorf("expected id %d, got %d", id, user.ID)
	}
	if user.Name != "Carol" {
		t.Errorf("expected name 'Carol', got %q", user.Name)
	}
}

func TestGetUserByID_NotFound(t *testing.T) {
	s := setupTestStore(t)

	user, err := s.GetUserByID(999999)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user != nil {
		t.Fatalf("expected nil for nonexistent id, got %+v", user)
	}
}

func TestUpdateUserProfile(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateUser("dave@example.com", "hashed-pw", "Dave")

	err := s.UpdateUserProfile(id, "David", "david@example.com")
	if err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}

	user, _ := s.GetUserByID(id)
	if user.Name != "David" {
		t.Errorf("expected name 'David', got %q", user.Name)
	}
	if user.Email != "david@example.com" {
		t.Errorf("expected email 'david@example.com', got %q", user.Email)
	}
}

func TestUpdateUserProfile_NotFound(t *testing.T) {
	s := setupTestStore(t)

	err := s.UpdateUserProfile(999999, "Nobody", "nobody@example.com")
	if err == nil {
		t.Fatal("expected error for non-existent user")
	}
}

func TestUpdateUserPassword(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateUser("eve@example.com", "old-hash", "Eve")

	err := s.UpdateUserPassword(id, "new-hash")
	if err != nil {
		t.Fatalf("UpdateUserPassword: %v", err)
	}

	user, _ := s.GetUserByID(id)
	if user.PasswordHash != "new-hash" {
		t.Errorf("expected password hash 'new-hash', got %q", user.PasswordHash)
	}
}

func TestUpdateUserPassword_NotFound(t *testing.T) {
	s := setupTestStore(t)

	err := s.UpdateUserPassword(999999, "new-hash")
	if err == nil {
		t.Fatal("expected error for non-existent user")
	}
}

func TestListUsers(t *testing.T) {
	s := setupTestStore(t)

	_, _ = s.CreateUser("user1@example.com", "hash1", "User1")
	_, _ = s.CreateUser("user2@example.com", "hash2", "User2")

	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
}

func TestSetUserActive(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateUser("frank@example.com", "hash", "Frank")

	// Deactivate
	err := s.SetUserActive(id, false)
	if err != nil {
		t.Fatalf("SetUserActive(false): %v", err)
	}
	user, _ := s.GetUserByID(id)
	if user.IsActive {
		t.Error("expected is_active=false after deactivation")
	}

	// Reactivate
	err = s.SetUserActive(id, true)
	if err != nil {
		t.Fatalf("SetUserActive(true): %v", err)
	}
	user, _ = s.GetUserByID(id)
	if !user.IsActive {
		t.Error("expected is_active=true after reactivation")
	}
}

func TestSetUserActive_NotFound(t *testing.T) {
	s := setupTestStore(t)

	err := s.SetUserActive(999999, false)
	if err == nil {
		t.Fatal("expected error for non-existent user")
	}
}

// --- Session tests ---

func TestCreateAndGetSession(t *testing.T) {
	s := setupTestStore(t)

	userID, _ := s.CreateUser("session-user@example.com", "hash", "SessionUser")
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	err := s.CreateSession(userID, "session-hash-123", expiresAt)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	session, err := s.GetSessionByTokenHash("session-hash-123")
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if session == nil {
		t.Fatal("expected session, got nil")
	}
	if session.UserID != userID {
		t.Errorf("expected user_id %d, got %d", userID, session.UserID)
	}
	if session.TokenHash != "session-hash-123" {
		t.Errorf("expected token_hash 'session-hash-123', got %q", session.TokenHash)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := setupTestStore(t)

	session, err := s.GetSessionByTokenHash("nonexistent-hash")
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	if session != nil {
		t.Fatalf("expected nil for nonexistent session, got %+v", session)
	}
}

func TestDeleteSession(t *testing.T) {
	s := setupTestStore(t)

	userID, _ := s.CreateUser("del-session@example.com", "hash", "DelSession")
	_ = s.CreateSession(userID, "del-hash", time.Now().Add(time.Hour))

	err := s.DeleteSession("del-hash")
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	session, _ := s.GetSessionByTokenHash("del-hash")
	if session != nil {
		t.Fatal("expected nil after deletion")
	}
}

func TestDeleteUserSessions(t *testing.T) {
	s := setupTestStore(t)

	userID, _ := s.CreateUser("multi-session@example.com", "hash", "MultiSession")
	_ = s.CreateSession(userID, "hash-1", time.Now().Add(time.Hour))
	_ = s.CreateSession(userID, "hash-2", time.Now().Add(time.Hour))

	err := s.DeleteUserSessions(userID)
	if err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}

	s1, _ := s.GetSessionByTokenHash("hash-1")
	s2, _ := s.GetSessionByTokenHash("hash-2")
	if s1 != nil || s2 != nil {
		t.Fatal("expected all sessions deleted")
	}
}

// --- Account tests ---

func TestCreateAccount(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateAccount("Test Account", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
}

func TestGetAccountByID(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateAccount("My Account", "service")

	acc, err := s.GetAccountByID(id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if acc == nil {
		t.Fatal("expected account, got nil")
	}
	if acc.Name != "My Account" {
		t.Errorf("expected name 'My Account', got %q", acc.Name)
	}
	if acc.Type != "service" {
		t.Errorf("expected type 'service', got %q", acc.Type)
	}
	if !acc.IsActive {
		t.Error("expected is_active=true")
	}
}

func TestGetAccountByID_NotFound(t *testing.T) {
	s := setupTestStore(t)

	acc, err := s.GetAccountByID(999999)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if acc != nil {
		t.Fatalf("expected nil for nonexistent id, got %+v", acc)
	}
}

func TestListAccounts(t *testing.T) {
	s := setupTestStore(t)

	_, _ = s.CreateAccount("Acc1", "personal")
	_, _ = s.CreateAccount("Acc2", "service")

	accounts, err := s.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
}

func TestSetAccountActive(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateAccount("Toggle", "personal")

	if err := s.SetAccountActive(id, false); err != nil {
		t.Fatalf("SetAccountActive(false): %v", err)
	}
	acc, _ := s.GetAccountByID(id)
	if acc.IsActive {
		t.Error("expected is_active=false")
	}

	if err := s.SetAccountActive(id, true); err != nil {
		t.Fatalf("SetAccountActive(true): %v", err)
	}
	acc, _ = s.GetAccountByID(id)
	if !acc.IsActive {
		t.Error("expected is_active=true")
	}
}

func TestSetAccountActive_NotFound(t *testing.T) {
	s := setupTestStore(t)

	err := s.SetAccountActive(999999, false)
	if err == nil {
		t.Fatal("expected error for non-existent account")
	}
}

func TestRegisterUser(t *testing.T) {
	s := setupTestStore(t)

	accountID, userID, err := s.RegisterUser("reg@example.com", "hashed-pw", "Registered")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if accountID <= 0 {
		t.Fatalf("expected positive accountID, got %d", accountID)
	}
	if userID <= 0 {
		t.Fatalf("expected positive userID, got %d", userID)
	}

	// Verify account was created
	acc, err := s.GetAccountByID(accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if acc == nil {
		t.Fatal("expected account, got nil")
	}
	if acc.Type != "personal" {
		t.Errorf("expected type 'personal', got %q", acc.Type)
	}
	if acc.Name != "Registered" {
		t.Errorf("expected name 'Registered', got %q", acc.Name)
	}

	// Verify user has account_id set
	user, err := s.GetUserByID(userID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.AccountID == nil {
		t.Fatal("expected user.AccountID to be set")
	}
	if *user.AccountID != accountID {
		t.Errorf("expected user.AccountID=%d, got %d", accountID, *user.AccountID)
	}
}

func TestRegisterUser_DuplicateEmail(t *testing.T) {
	s := setupTestStore(t)

	_, _, err := s.RegisterUser("dup-reg@example.com", "hash", "First")
	if err != nil {
		t.Fatalf("first RegisterUser: %v", err)
	}

	_, _, err = s.RegisterUser("dup-reg@example.com", "hash2", "Second")
	if err == nil {
		t.Fatal("expected error for duplicate email")
	}
}

func TestBackfillAccounts(t *testing.T) {
	s := setupTestStore(t)

	// Create a legacy user without account_id
	userID, err := s.CreateUser("legacy@example.com", "hash", "Legacy")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Create a key for this user
	keyID, err := s.CreateKeyForUser(userID, "legacy-key", "hash-legacy-key", "sk-leg", 60)
	if err != nil {
		t.Fatalf("CreateKeyForUser: %v", err)
	}

	// Verify no account_id yet
	user, _ := s.GetUserByID(userID)
	if user.AccountID != nil {
		t.Fatal("expected nil AccountID before backfill")
	}

	// Run backfill
	if err := s.BackfillAccounts(); err != nil {
		t.Fatalf("BackfillAccounts: %v", err)
	}

	// Verify user now has account_id
	user, _ = s.GetUserByID(userID)
	if user.AccountID == nil {
		t.Fatal("expected AccountID after backfill")
	}

	// Verify account was created
	acc, _ := s.GetAccountByID(*user.AccountID)
	if acc == nil {
		t.Fatal("expected account to exist")
	}
	if acc.Name != "Legacy" {
		t.Errorf("expected account name 'Legacy', got %q", acc.Name)
	}
	if acc.Type != "personal" {
		t.Errorf("expected type 'personal', got %q", acc.Type)
	}

	// Verify key now has account_id
	key, _ := s.GetKeyByHash("hash-legacy-key")
	if key == nil {
		t.Fatal("expected key")
	}
	if key.AccountID == nil {
		t.Fatal("expected key.AccountID after backfill")
	}
	if *key.AccountID != *user.AccountID {
		t.Errorf("expected key.AccountID=%d, got %d", *user.AccountID, *key.AccountID)
	}
	_ = keyID // used for creation above
}

func TestBackfillAccounts_Idempotent(t *testing.T) {
	s := setupTestStore(t)

	_, _ = s.CreateUser("idem@example.com", "hash", "Idempotent")

	// Run twice — should not error or create duplicate accounts
	if err := s.BackfillAccounts(); err != nil {
		t.Fatalf("first BackfillAccounts: %v", err)
	}
	if err := s.BackfillAccounts(); err != nil {
		t.Fatalf("second BackfillAccounts: %v", err)
	}

	accounts, _ := s.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account after two backfills, got %d", len(accounts))
	}
}

func TestCreateKeyForAccount(t *testing.T) {
	s := setupTestStore(t)

	accountID, userID, _ := s.RegisterUser("keyed@example.com", "hash", "Keyed")

	keyID, err := s.CreateKeyForAccount(userID, accountID, "acc-key", "hash-acc-key", "sk-acc", 100)
	if err != nil {
		t.Fatalf("CreateKeyForAccount: %v", err)
	}
	if keyID <= 0 {
		t.Fatalf("expected positive keyID, got %d", keyID)
	}

	// Verify key has both user_id and account_id
	key, _ := s.GetKeyByHash("hash-acc-key")
	if key == nil {
		t.Fatal("expected key")
	}
	if key.UserID == nil || *key.UserID != userID {
		t.Errorf("expected UserID=%d, got %v", userID, key.UserID)
	}
	if key.AccountID == nil || *key.AccountID != accountID {
		t.Errorf("expected AccountID=%d, got %v", accountID, key.AccountID)
	}
}

func TestGetKeyByHash_WithAllFields(t *testing.T) {
	s := setupTestStore(t)

	// Create key via admin (no user_id, no account_id)
	_, _ = s.CreateKey("admin-key", "hash-admin", "sk-adm", 60)

	key, err := s.GetKeyByHash("hash-admin")
	if err != nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if key == nil {
		t.Fatal("expected key")
	}
	if key.UserID != nil {
		t.Errorf("expected nil UserID for admin key, got %v", key.UserID)
	}
	if key.AccountID != nil {
		t.Errorf("expected nil AccountID for admin key, got %v", key.AccountID)
	}
	if key.SessionTokenLimit != nil {
		t.Errorf("expected nil SessionTokenLimit, got %v", key.SessionTokenLimit)
	}
}

// --- User-owned key tests ---

func TestCreateKeyForUser(t *testing.T) {
	s := setupTestStore(t)

	userID, _ := s.CreateUser("key-owner@example.com", "hash", "KeyOwner")

	id, err := s.CreateKeyForUser(userID, "my-key", "key-hash-user", "sk-usr", 100)
	if err != nil {
		t.Fatalf("CreateKeyForUser: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}
}

func TestListKeysByUser(t *testing.T) {
	s := setupTestStore(t)

	user1, _ := s.CreateUser("keys-u1@example.com", "hash", "User1")
	user2, _ := s.CreateUser("keys-u2@example.com", "hash", "User2")

	_, _ = s.CreateKeyForUser(user1, "u1-key-a", "hash-u1a", "sk-u1a", 60)
	_, _ = s.CreateKeyForUser(user1, "u1-key-b", "hash-u1b", "sk-u1b", 60)
	_, _ = s.CreateKeyForUser(user2, "u2-key-a", "hash-u2a", "sk-u2a", 60)

	keys, err := s.ListKeysByUser(user1)
	if err != nil {
		t.Fatalf("ListKeysByUser: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys for user1, got %d", len(keys))
	}

	keys2, _ := s.ListKeysByUser(user2)
	if len(keys2) != 1 {
		t.Fatalf("expected 1 key for user2, got %d", len(keys2))
	}
}

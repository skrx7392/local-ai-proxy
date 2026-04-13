package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These tests exercise store methods that aren't covered by the existing
// admin/user/credits integration paths.

func TestStore_Ping(t *testing.T) {
	s := setupTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestStore_PoolAccessor(t *testing.T) {
	s := setupTestStore(t)
	if s.Pool() == nil {
		t.Error("Pool() must return non-nil *pgxpool.Pool")
	}
}

func TestStore_CreateKeyForAccountOnly(t *testing.T) {
	s := setupTestStore(t)

	accountID, err := s.CreateAccount("svc-acc", "service")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	id, err := s.CreateKeyForAccountOnly(accountID, "svc-key", "hash1", "sk-abc", 120)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero id")
	}

	// Verify the key exists and is linked to the account (not a user).
	keys, err := s.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	var found bool
	for _, k := range keys {
		if k.ID == id {
			found = true
			if k.AccountID == nil || *k.AccountID != accountID {
				t.Errorf("expected AccountID=%d, got %+v", accountID, k.AccountID)
			}
			if k.UserID != nil {
				t.Errorf("expected UserID=nil, got %v", *k.UserID)
			}
		}
	}
	if !found {
		t.Error("key not found in list")
	}
}

func TestStore_CreateListRevokeRegistrationToken(t *testing.T) {
	s := setupTestStore(t)

	expiry := time.Now().Add(24 * time.Hour)
	id1, err := s.CreateRegistrationToken("tok-1", "hash-1", 10.0, 2, &expiry)
	if err != nil {
		t.Fatalf("CreateRegistrationToken: %v", err)
	}
	if _, err := s.CreateRegistrationToken("tok-2", "hash-2", 0, 1, nil); err != nil {
		t.Fatalf("CreateRegistrationToken (no expiry): %v", err)
	}

	tokens, err := s.ListRegistrationTokens()
	if err != nil {
		t.Fatalf("ListRegistrationTokens: %v", err)
	}
	if len(tokens) < 2 {
		t.Fatalf("expected >=2 tokens, got %d", len(tokens))
	}

	if err := s.RevokeRegistrationToken(id1); err != nil {
		t.Errorf("RevokeRegistrationToken: %v", err)
	}

	// Revoking non-existent returns error.
	if err := s.RevokeRegistrationToken(999999); err == nil {
		t.Error("expected error revoking non-existent token")
	}
}

func TestStore_RegisterServiceAccount(t *testing.T) {
	s := setupTestStore(t)

	const rawTok = "raw-reg-token-abc"
	const tokHash = "deadbeef-registration-token-hash"
	if _, err := s.CreateRegistrationToken("svc-rt", tokHash, 25.0, 2, nil); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	accID, keyID, grant, err := s.RegisterServiceAccount(tokHash, "svc-one", "api-hash-1", "sk-pre", 60)
	if err != nil {
		t.Fatalf("RegisterServiceAccount: %v", err)
	}
	if accID == 0 || keyID == 0 {
		t.Errorf("expected non-zero IDs, got acc=%d key=%d", accID, keyID)
	}
	if grant != 25.0 {
		t.Errorf("expected grant 25.0, got %v", grant)
	}

	// Second use consumes the second allotment.
	if _, _, _, err := s.RegisterServiceAccount(tokHash, "svc-two", "api-hash-2", "sk-pre", 60); err != nil {
		t.Errorf("second RegisterServiceAccount: %v", err)
	}

	// Third use must fail (max_uses=2 exhausted).
	if _, _, _, err := s.RegisterServiceAccount(tokHash, "svc-three", "api-hash-3", "sk-pre", 60); err == nil {
		t.Error("expected error once token is exhausted")
	}

	_ = rawTok
}

func TestStore_RegisterServiceAccount_InvalidToken(t *testing.T) {
	s := setupTestStore(t)

	if _, _, _, err := s.RegisterServiceAccount("never-existed", "svc", "h", "sk-pre", 60); err == nil {
		t.Error("expected error for unknown token")
	}
}

func TestStore_SetSessionTokenLimit(t *testing.T) {
	s := setupTestStore(t)

	keyID, err := s.CreateKey("session-lim", "hash-sesslim", "sk-pref", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	limit := 200
	if err := s.SetSessionTokenLimit(keyID, &limit); err != nil {
		t.Fatalf("SetSessionTokenLimit: %v", err)
	}

	if err := s.SetSessionTokenLimit(keyID, nil); err != nil {
		t.Errorf("SetSessionTokenLimit(nil): %v", err)
	}

	if err := s.SetSessionTokenLimit(999999, &limit); err == nil {
		t.Error("expected error for unknown key id")
	}
}

func TestStore_ListAccountsWithBalances(t *testing.T) {
	s := setupTestStore(t)

	acc1, _ := s.CreateAccount("a-one", "personal")
	if err := s.InitCreditBalance(acc1); err != nil {
		t.Fatalf("InitCreditBalance: %v", err)
	}
	if err := s.AddCredits(acc1, 12.5, "seed"); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	// Account without a balance row exercises the LEFT JOIN COALESCE branch.
	_, _ = s.CreateAccount("a-two", "service")

	accs, err := s.ListAccountsWithBalances()
	if err != nil {
		t.Fatalf("ListAccountsWithBalances: %v", err)
	}
	if len(accs) < 2 {
		t.Fatalf("expected >=2 accounts, got %d", len(accs))
	}
}

func TestStore_DeactivateUserGuarded_NotFound(t *testing.T) {
	s := setupTestStore(t)
	if err := s.DeactivateUserGuarded(999999); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
}

func TestStore_DeactivateUserGuarded_NonAdminPassesThrough(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("regular@example.com", "hash", "Regular")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := s.DeactivateUserGuarded(uid); err != nil {
		t.Fatalf("expected success deactivating a non-admin, got %v", err)
	}
	u, _ := s.GetUserByID(uid)
	if u == nil || u.IsActive {
		t.Error("user should be inactive")
	}
}

func TestStore_DeactivateUserGuarded_AlreadyInactiveAdmin(t *testing.T) {
	// An admin that is already inactive doesn't count toward the active-admin
	// census, so deactivating it again (idempotent) should not trigger the
	// last-admin guard — as long as another active admin exists.
	s := setupTestStore(t)

	keepActive, _ := s.CreateUser("keep@example.com", "h", "Keep")
	_, _ = s.pool.Exec(context.Background(), `UPDATE users SET role='admin' WHERE id = $1`, keepActive)

	already, _ := s.CreateUser("already-off@example.com", "h", "Off")
	_, _ = s.pool.Exec(context.Background(),
		`UPDATE users SET role='admin', is_active=FALSE WHERE id = $1`, already)

	if err := s.DeactivateUserGuarded(already); err != nil {
		t.Errorf("expected success for already-inactive admin, got %v", err)
	}
}

func TestStore_CreateAdminBootstrap_DuplicateEmail(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateAdminBootstrap("bs-dup@example.com", "hash", "First")
	if err != nil {
		t.Fatalf("first CreateAdminBootstrap: %v", err)
	}
	if uid == 0 {
		t.Fatal("expected non-zero user id")
	}

	if _, err := s.CreateAdminBootstrap("bs-dup@example.com", "hash", "Second"); !errors.Is(err, ErrEmailExists) {
		t.Errorf("expected ErrEmailExists, got %v", err)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if isUniqueViolation(errors.New("random")) {
		t.Error("random error should not be reported as unique violation")
	}
	if isUniqueViolation(nil) {
		t.Error("nil should not be a unique violation")
	}
}

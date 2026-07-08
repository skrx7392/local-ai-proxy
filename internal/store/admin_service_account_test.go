package store

import (
	"context"
	"testing"
)

func TestEnsureAdminServiceAccount_CreatesWithGrant(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.EnsureAdminServiceAccount(1000)
	if err != nil {
		t.Fatalf("EnsureAdminServiceAccount: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive account id, got %d", id)
	}

	acct, err := s.GetAccountByID(id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if acct == nil {
		t.Fatal("expected account to exist")
	}
	if acct.Name != AdminServiceAccountName {
		t.Errorf("expected name %q, got %q", AdminServiceAccountName, acct.Name)
	}
	if acct.Type != "service" {
		t.Errorf("expected type 'service', got %q", acct.Type)
	}
	if !acct.IsActive {
		t.Error("expected account to be active")
	}

	bal, err := s.GetCreditBalance(id)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal == nil {
		t.Fatal("expected credit balance row")
	}
	if bal.Balance != 1000 {
		t.Errorf("expected balance 1000, got %v", bal.Balance)
	}

	// Grant must leave an audit trail.
	txs, err := s.GetCreditTransactions(id, 10, 0)
	if err != nil {
		t.Fatalf("GetCreditTransactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected exactly 1 grant transaction, got %d", len(txs))
	}
	if txs[0].Type != "grant" || txs[0].Amount != 1000 {
		t.Errorf("expected grant of 1000, got type=%q amount=%v", txs[0].Type, txs[0].Amount)
	}
}

func TestEnsureAdminServiceAccount_Idempotent(t *testing.T) {
	s := setupTestStore(t)

	id1, err := s.EnsureAdminServiceAccount(500)
	if err != nil {
		t.Fatalf("first EnsureAdminServiceAccount: %v", err)
	}
	// Second call with a different grant must NOT create another account or
	// re-grant credits — the grant applies only at creation time.
	id2, err := s.EnsureAdminServiceAccount(999999)
	if err != nil {
		t.Fatalf("second EnsureAdminServiceAccount: %v", err)
	}
	if id1 != id2 {
		t.Errorf("expected same account id, got %d and %d", id1, id2)
	}

	var count int
	err = s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM accounts WHERE name = $1 AND type = 'service'`,
		AdminServiceAccountName,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 admin service account, got %d", count)
	}

	bal, err := s.GetCreditBalance(id1)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal.Balance != 500 {
		t.Errorf("expected balance to stay 500 (no re-grant), got %v", bal.Balance)
	}

	txs, err := s.GetCreditTransactions(id1, 10, 0)
	if err != nil {
		t.Fatalf("GetCreditTransactions: %v", err)
	}
	if len(txs) != 1 {
		t.Errorf("expected exactly 1 grant transaction after repeat calls, got %d", len(txs))
	}
}

func TestEnsureAdminServiceAccount_ZeroGrant(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.EnsureAdminServiceAccount(0)
	if err != nil {
		t.Fatalf("EnsureAdminServiceAccount: %v", err)
	}
	bal, err := s.GetCreditBalance(id)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal == nil {
		t.Fatal("expected credit balance row even with zero grant")
	}
	if bal.Balance != 0 {
		t.Errorf("expected zero balance, got %v", bal.Balance)
	}
	txs, err := s.GetCreditTransactions(id, 10, 0)
	if err != nil {
		t.Fatalf("GetCreditTransactions: %v", err)
	}
	if len(txs) != 0 {
		t.Errorf("expected no grant transaction for zero grant, got %d", len(txs))
	}
}

func TestBackfillAdminKeyAccounts_AttachesLegacyKeys(t *testing.T) {
	s := setupTestStore(t)

	// Legacy admin-created key: no user, no account — exactly what a
	// pre-upgrade production database contains.
	legacyID, err := s.CreateKey("legacy-admin", "hash-legacy", "sk-legacy", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	// User-owned key whose account link was lost: must reattach to the
	// OWNER's account, not the admin service account.
	userAccID, userID, err := s.RegisterUser("owner@example.com", "hash", "Owner")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	orphanID, err := s.CreateKeyForUser(userID, "orphan-user-key", "hash-orphan", "sk-orphan", 60)
	if err != nil {
		t.Fatalf("CreateKeyForUser: %v", err)
	}

	// A properly attached key must not be touched.
	attachedID, err := s.CreateKeyForAccount(userID, userAccID, "attached", "hash-attached", "sk-attach", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccount: %v", err)
	}

	adminAccID, err := s.EnsureAdminServiceAccount(1000)
	if err != nil {
		t.Fatalf("EnsureAdminServiceAccount: %v", err)
	}

	n, err := s.BackfillAdminKeyAccounts(adminAccID)
	if err != nil {
		t.Fatalf("BackfillAdminKeyAccounts: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 keys backfilled, got %d", n)
	}

	legacy, err := s.GetKeyByID(legacyID)
	if err != nil {
		t.Fatalf("GetKeyByID(legacy): %v", err)
	}
	if legacy.AccountID == nil || *legacy.AccountID != adminAccID {
		t.Errorf("expected legacy key attached to admin account %d, got %v", adminAccID, legacy.AccountID)
	}

	orphan, err := s.GetKeyByID(orphanID)
	if err != nil {
		t.Fatalf("GetKeyByID(orphan): %v", err)
	}
	if orphan.AccountID == nil || *orphan.AccountID != userAccID {
		t.Errorf("expected user key attached to owner account %d, got %v", userAccID, orphan.AccountID)
	}

	attached, err := s.GetKeyByID(attachedID)
	if err != nil {
		t.Fatalf("GetKeyByID(attached): %v", err)
	}
	if attached.AccountID == nil || *attached.AccountID != userAccID {
		t.Errorf("expected attached key to keep account %d, got %v", userAccID, attached.AccountID)
	}
}

func TestBackfillAdminKeyAccounts_Idempotent(t *testing.T) {
	s := setupTestStore(t)

	if _, err := s.CreateKey("legacy", "hash-1", "sk-1", 60); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	adminAccID, err := s.EnsureAdminServiceAccount(100)
	if err != nil {
		t.Fatalf("EnsureAdminServiceAccount: %v", err)
	}

	n1, err := s.BackfillAdminKeyAccounts(adminAccID)
	if err != nil {
		t.Fatalf("first BackfillAdminKeyAccounts: %v", err)
	}
	if n1 != 1 {
		t.Errorf("expected 1 key backfilled on first run, got %d", n1)
	}

	n2, err := s.BackfillAdminKeyAccounts(adminAccID)
	if err != nil {
		t.Fatalf("second BackfillAdminKeyAccounts: %v", err)
	}
	if n2 != 0 {
		t.Errorf("expected 0 keys backfilled on second run, got %d", n2)
	}

	// No NULL-account keys may remain.
	var remaining int
	err = s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM api_keys WHERE account_id IS NULL`).Scan(&remaining)
	if err != nil {
		t.Fatalf("count NULL-account keys: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected 0 NULL-account keys after backfill, got %d", remaining)
	}
}

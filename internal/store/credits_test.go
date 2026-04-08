package store

import (
	"math"
	"sync"
	"testing"
	"time"
)

const floatTolerance = 0.000001

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < floatTolerance
}

// --- Credit Balance tests ---

func TestInitCreditBalance(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("bal-test", "personal")

	if err := s.InitCreditBalance(accID); err != nil {
		t.Fatalf("InitCreditBalance: %v", err)
	}

	bal, err := s.GetCreditBalance(accID)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal == nil {
		t.Fatal("expected balance, got nil")
	}
	if !almostEqual(bal.Balance, 0) {
		t.Errorf("expected balance 0, got %f", bal.Balance)
	}
	if !almostEqual(bal.Reserved, 0) {
		t.Errorf("expected reserved 0, got %f", bal.Reserved)
	}
}

func TestInitCreditBalance_Idempotent(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("idem-bal", "personal")

	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	// Second init should not reset balance
	_ = s.InitCreditBalance(accID)

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Balance, 100) {
		t.Errorf("expected balance 100 after idempotent init, got %f", bal.Balance)
	}
}

func TestGetCreditBalance_NotFound(t *testing.T) {
	s := setupTestStore(t)

	bal, err := s.GetCreditBalance(999999)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal != nil {
		t.Fatalf("expected nil for nonexistent balance, got %+v", bal)
	}
}

func TestBackfillCreditBalances(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("backfill-bal", "personal")

	// No balance row yet
	bal, _ := s.GetCreditBalance(accID)
	if bal != nil {
		t.Fatal("expected nil balance before backfill")
	}

	if err := s.BackfillCreditBalances(); err != nil {
		t.Fatalf("BackfillCreditBalances: %v", err)
	}

	bal, _ = s.GetCreditBalance(accID)
	if bal == nil {
		t.Fatal("expected balance after backfill")
	}
}

func TestRegisterUser_InitsCreditBalance(t *testing.T) {
	s := setupTestStore(t)

	accountID, _, err := s.RegisterUser("credit-init@example.com", "hash", "CreditInit")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	bal, err := s.GetCreditBalance(accountID)
	if err != nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal == nil {
		t.Fatal("expected credit balance to be initialized by RegisterUser")
	}
	if !almostEqual(bal.Balance, 0) {
		t.Errorf("expected balance 0, got %f", bal.Balance)
	}
}

// --- AddCredits tests ---

func TestAddCredits(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("add-cred", "personal")
	_ = s.InitCreditBalance(accID)

	if err := s.AddCredits(accID, 500.5, "initial grant"); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Balance, 500.5) {
		t.Errorf("expected balance 500.5, got %f", bal.Balance)
	}

	// Check transaction was created
	txns, err := s.GetCreditTransactions(accID, 10, 0)
	if err != nil {
		t.Fatalf("GetCreditTransactions: %v", err)
	}
	if len(txns) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(txns))
	}
	if txns[0].Type != "grant" {
		t.Errorf("expected type 'grant', got %q", txns[0].Type)
	}
	if !almostEqual(txns[0].Amount, 500.5) {
		t.Errorf("expected amount 500.5, got %f", txns[0].Amount)
	}
}

func TestAddCredits_Multiple(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("multi-cred", "personal")
	_ = s.InitCreditBalance(accID)

	_ = s.AddCredits(accID, 100, "first")
	_ = s.AddCredits(accID, 200, "second")

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Balance, 300) {
		t.Errorf("expected balance 300, got %f", bal.Balance)
	}

	txns, _ := s.GetCreditTransactions(accID, 10, 0)
	if len(txns) != 2 {
		t.Fatalf("expected 2 transactions, got %d", len(txns))
	}
}

// --- Reserve/Settle/Release tests ---

func TestReserveCredits(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("reserve", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, err := s.ReserveCredits(accID, 30)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}
	if holdID <= 0 {
		t.Fatalf("expected positive holdID, got %d", holdID)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Reserved, 30) {
		t.Errorf("expected reserved 30, got %f", bal.Reserved)
	}
	if !almostEqual(bal.Balance, 100) {
		t.Errorf("balance should still be 100, got %f", bal.Balance)
	}
}

func TestReserveCredits_Insufficient(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("insuff", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 10, "grant")

	_, err := s.ReserveCredits(accID, 50)
	if err == nil {
		t.Fatal("expected error for insufficient credits")
	}

	// Verify no hold was created
	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Reserved, 0) {
		t.Errorf("expected reserved 0 after failed reserve, got %f", bal.Reserved)
	}
}

func TestReserveCredits_ExactBalance(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("exact", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 50, "grant")

	_, err := s.ReserveCredits(accID, 50)
	if err != nil {
		t.Fatalf("ReserveCredits with exact balance: %v", err)
	}

	// Second reserve should fail — all balance is reserved
	_, err = s.ReserveCredits(accID, 1)
	if err == nil {
		t.Fatal("expected error when all balance reserved")
	}
}

func TestSettleHold(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("settle", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 30)

	// Actual cost was 20 (less than reserved 30)
	if err := s.SettleHold(holdID, 20); err != nil {
		t.Fatalf("SettleHold: %v", err)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Balance, 80) { // 100 - 20
		t.Errorf("expected balance 80, got %f", bal.Balance)
	}
	if !almostEqual(bal.Reserved, 0) { // 30 - 30 (full hold released)
		t.Errorf("expected reserved 0, got %f", bal.Reserved)
	}

	// Check usage transaction
	txns, _ := s.GetCreditTransactions(accID, 10, 0)
	// Should have: grant + usage
	usageTxn := txns[0] // most recent first
	if usageTxn.Type != "usage" {
		t.Errorf("expected type 'usage', got %q", usageTxn.Type)
	}
	if !almostEqual(usageTxn.Amount, -20) {
		t.Errorf("expected amount -20, got %f", usageTxn.Amount)
	}
	if usageTxn.ReferenceID == nil || *usageTxn.ReferenceID != holdID {
		t.Errorf("expected reference_id=%d, got %v", holdID, usageTxn.ReferenceID)
	}
}

func TestSettleHold_AfterSweeperReleased(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("sweep-settle", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 30)

	// Simulate sweeper releasing the hold
	_ = s.ReleaseHold(holdID)

	// Settle after release should be a no-op (not error)
	if err := s.SettleHold(holdID, 20); err != nil {
		t.Fatalf("SettleHold after release should be no-op, got: %v", err)
	}

	bal, _ := s.GetCreditBalance(accID)
	// Balance should be unchanged (100) — hold was released, settle was no-op
	if !almostEqual(bal.Balance, 100) {
		t.Errorf("expected balance 100 (no-op settle), got %f", bal.Balance)
	}
}

func TestReleaseHold(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("release", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 40)

	if err := s.ReleaseHold(holdID); err != nil {
		t.Fatalf("ReleaseHold: %v", err)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Balance, 100) {
		t.Errorf("expected balance 100 after release, got %f", bal.Balance)
	}
	if !almostEqual(bal.Reserved, 0) {
		t.Errorf("expected reserved 0 after release, got %f", bal.Reserved)
	}
}

func TestReleaseHold_DoubleRelease(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("dbl-release", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 40)

	_ = s.ReleaseHold(holdID)
	// Second release should be no-op
	if err := s.ReleaseHold(holdID); err != nil {
		t.Fatalf("double ReleaseHold should be no-op, got: %v", err)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Reserved, 0) {
		t.Errorf("expected reserved 0, got %f", bal.Reserved)
	}
}

func TestConcurrentReserve(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("concurrent", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	// Try to reserve 60 credits from 10 goroutines simultaneously
	// Only 1 should succeed (60 > 100/2), the rest should fail
	var wg sync.WaitGroup
	successes := make(chan int64, 10)
	failures := make(chan struct{}, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			holdID, err := s.ReserveCredits(accID, 60)
			if err != nil {
				failures <- struct{}{}
			} else {
				successes <- holdID
			}
		}()
	}

	wg.Wait()
	close(successes)
	close(failures)

	successCount := 0
	for range successes {
		successCount++
	}
	failCount := 0
	for range failures {
		failCount++
	}

	if successCount != 1 {
		t.Errorf("expected exactly 1 successful reserve, got %d", successCount)
	}
	if failCount != 9 {
		t.Errorf("expected 9 failures, got %d", failCount)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Reserved, 60) {
		t.Errorf("expected reserved 60, got %f", bal.Reserved)
	}
}

// --- Sweeper tests ---

func TestSweepStaleHolds(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("sweep", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 30)

	// Manually backdate the hold to make it stale
	_, _ = s.pool.Exec(
		t.Context(),
		`UPDATE credit_holds SET created_at = NOW() - INTERVAL '20 minutes' WHERE id = $1`, holdID,
	)

	released, err := s.SweepStaleHolds(10 * time.Minute)
	if err != nil {
		t.Fatalf("SweepStaleHolds: %v", err)
	}
	if released != 1 {
		t.Errorf("expected 1 released, got %d", released)
	}

	bal, _ := s.GetCreditBalance(accID)
	if !almostEqual(bal.Reserved, 0) {
		t.Errorf("expected reserved 0 after sweep, got %f", bal.Reserved)
	}
}

func TestSweepStaleHolds_DoesNotTouchRecent(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("sweep-recent", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	_, _ = s.ReserveCredits(accID, 30)

	released, _ := s.SweepStaleHolds(10 * time.Minute)
	if released != 0 {
		t.Errorf("expected 0 released for recent holds, got %d", released)
	}
}

func TestCleanupSettledHolds(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("cleanup", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 10)
	_ = s.SettleHold(holdID, 5)

	// Backdate the settled_at
	_, _ = s.pool.Exec(
		t.Context(),
		`UPDATE credit_holds SET settled_at = NOW() - INTERVAL '31 days' WHERE id = $1`, holdID,
	)

	deleted, err := s.CleanupSettledHolds(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("CleanupSettledHolds: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted, got %d", deleted)
	}
}

// --- Pricing tests ---

func TestUpsertAndGetPricing(t *testing.T) {
	s := setupTestStore(t)

	if err := s.UpsertPricing("llama3.1:8b", 0.002, 0.002, 500); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	p, err := s.GetPricingByModel("llama3.1:8b")
	if err != nil {
		t.Fatalf("GetPricingByModel: %v", err)
	}
	if p == nil {
		t.Fatal("expected pricing, got nil")
	}
	if !almostEqual(p.PromptRate, 0.002) {
		t.Errorf("expected prompt_rate 0.002, got %f", p.PromptRate)
	}
	if p.TypicalCompletion != 500 {
		t.Errorf("expected typical_completion 500, got %d", p.TypicalCompletion)
	}
}

func TestGetPricingByModel_NotFound(t *testing.T) {
	s := setupTestStore(t)

	p, err := s.GetPricingByModel("nonexistent-model")
	if err != nil {
		t.Fatalf("GetPricingByModel: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil for nonexistent model, got %+v", p)
	}
}

func TestListActivePricing(t *testing.T) {
	s := setupTestStore(t)

	_ = s.UpsertPricing("model-a", 0.001, 0.001, 300)
	_ = s.UpsertPricing("model-b", 0.002, 0.002, 500)

	pricing, err := s.ListActivePricing()
	if err != nil {
		t.Fatalf("ListActivePricing: %v", err)
	}
	if len(pricing) < 2 {
		t.Fatalf("expected at least 2 pricing entries, got %d", len(pricing))
	}
}

func TestDeletePricing(t *testing.T) {
	s := setupTestStore(t)

	_ = s.UpsertPricing("delete-me", 0.001, 0.001, 300)
	p, _ := s.GetPricingByModel("delete-me")

	if err := s.DeletePricing(p.ID); err != nil {
		t.Fatalf("DeletePricing: %v", err)
	}

	p2, _ := s.GetPricingByModel("delete-me")
	if p2 != nil {
		t.Error("expected nil after delete (deactivation)")
	}
}

func TestUpsertPricing_Reactivates(t *testing.T) {
	s := setupTestStore(t)

	_ = s.UpsertPricing("reactivate", 0.001, 0.001, 300)
	p, _ := s.GetPricingByModel("reactivate")
	_ = s.DeletePricing(p.ID)

	// Upsert should reactivate
	_ = s.UpsertPricing("reactivate", 0.003, 0.003, 600)

	p2, _ := s.GetPricingByModel("reactivate")
	if p2 == nil {
		t.Fatal("expected pricing to be reactivated")
	}
	if !almostEqual(p2.PromptRate, 0.003) {
		t.Errorf("expected updated prompt_rate 0.003, got %f", p2.PromptRate)
	}
}

// --- AccountUsageStats tests ---

func TestUpdateAccountUsageStats(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("stats", "personal")

	// First update
	if err := s.UpdateAccountUsageStats(accID, "llama3.1:8b", 100); err != nil {
		t.Fatalf("UpdateAccountUsageStats: %v", err)
	}

	stats, err := s.GetAccountUsageStats(accID, "llama3.1:8b")
	if err != nil {
		t.Fatalf("GetAccountUsageStats: %v", err)
	}
	if stats == nil {
		t.Fatal("expected stats, got nil")
	}
	if stats.RequestCount != 1 {
		t.Errorf("expected request_count 1, got %d", stats.RequestCount)
	}
	if stats.AvgCompletionTokens != 100 {
		t.Errorf("expected avg 100, got %d", stats.AvgCompletionTokens)
	}

	// Second update: avg should be (100*1 + 200) / 2 = 150
	_ = s.UpdateAccountUsageStats(accID, "llama3.1:8b", 200)
	stats, _ = s.GetAccountUsageStats(accID, "llama3.1:8b")
	if stats.RequestCount != 2 {
		t.Errorf("expected request_count 2, got %d", stats.RequestCount)
	}
	if stats.AvgCompletionTokens != 150 {
		t.Errorf("expected avg 150, got %d", stats.AvgCompletionTokens)
	}
}

func TestGetAccountUsageStats_NotFound(t *testing.T) {
	s := setupTestStore(t)

	stats, err := s.GetAccountUsageStats(999999, "nonexistent")
	if err != nil {
		t.Fatalf("GetAccountUsageStats: %v", err)
	}
	if stats != nil {
		t.Fatalf("expected nil, got %+v", stats)
	}
}

// --- GetAccountCreditStatus tests ---

func TestGetAccountCreditStatus(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("status-test", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 200, "grant")
	_, _ = s.ReserveCredits(accID, 50)

	isActive, balance, reserved, err := s.GetAccountCreditStatus(accID)
	if err != nil {
		t.Fatalf("GetAccountCreditStatus: %v", err)
	}
	if !isActive {
		t.Error("expected isActive=true")
	}
	if !almostEqual(balance, 200) {
		t.Errorf("expected balance 200, got %f", balance)
	}
	if !almostEqual(reserved, 50) {
		t.Errorf("expected reserved 50, got %f", reserved)
	}
}

func TestGetAccountCreditStatus_Inactive(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("inactive-status", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.SetAccountActive(accID, false)

	isActive, _, _, err := s.GetAccountCreditStatus(accID)
	if err != nil {
		t.Fatalf("GetAccountCreditStatus: %v", err)
	}
	if isActive {
		t.Error("expected isActive=false")
	}
}

// --- GetSessionTokenUsage tests ---

func TestGetSessionTokenUsage(t *testing.T) {
	s := setupTestStore(t)
	keyID, _ := s.CreateKey("session-usage", "hash-su", "sk-su0", 60)

	_ = s.LogUsage(UsageEntry{APIKeyID: keyID, Model: "llama3", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, DurationMs: 100, Status: "completed"})
	_ = s.LogUsage(UsageEntry{APIKeyID: keyID, Model: "llama3", PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300, DurationMs: 200, Status: "completed"})

	consumed, oldest, err := s.GetSessionTokenUsage(keyID, 6*time.Hour)
	if err != nil {
		t.Fatalf("GetSessionTokenUsage: %v", err)
	}
	if consumed != 450 {
		t.Errorf("expected consumed 450, got %d", consumed)
	}
	if oldest == nil {
		t.Fatal("expected oldest timestamp, got nil")
	}
}

func TestGetSessionTokenUsage_Empty(t *testing.T) {
	s := setupTestStore(t)
	keyID, _ := s.CreateKey("session-empty", "hash-se", "sk-se0", 60)

	consumed, oldest, err := s.GetSessionTokenUsage(keyID, 6*time.Hour)
	if err != nil {
		t.Fatalf("GetSessionTokenUsage: %v", err)
	}
	if consumed != 0 {
		t.Errorf("expected consumed 0, got %d", consumed)
	}
	if oldest != nil {
		t.Errorf("expected nil oldest for empty usage, got %v", oldest)
	}
}

// --- Transaction pagination tests ---

func TestGetCreditTransactions_Pagination(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("txn-page", "personal")
	_ = s.InitCreditBalance(accID)

	for i := 0; i < 5; i++ {
		_ = s.AddCredits(accID, float64(i+1)*10, "grant")
	}

	// Page 1
	txns, _ := s.GetCreditTransactions(accID, 2, 0)
	if len(txns) != 2 {
		t.Fatalf("expected 2 transactions in page 1, got %d", len(txns))
	}

	// Page 2
	txns2, _ := s.GetCreditTransactions(accID, 2, 2)
	if len(txns2) != 2 {
		t.Fatalf("expected 2 transactions in page 2, got %d", len(txns2))
	}

	// Page 3
	txns3, _ := s.GetCreditTransactions(accID, 2, 4)
	if len(txns3) != 1 {
		t.Fatalf("expected 1 transaction in page 3, got %d", len(txns3))
	}
}

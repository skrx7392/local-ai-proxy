package store

import (
	"sync"
	"testing"
	"time"
)

func resolveArgs(extID string) FederatedIdentity {
	return FederatedIdentity{
		Source:      "openwebui",
		ExternalID:  extID,
		Email:       "alice@example.com",
		DisplayName: "Alice",
	}
}

func TestResolveEndUserAccount_ProvisionsAccountWithAllowance(t *testing.T) {
	s := setupTestStore(t)

	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	res, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, now)
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	if res.AccountID <= 0 {
		t.Fatalf("expected positive account id, got %d", res.AccountID)
	}
	if !res.Provisioned {
		t.Error("expected Provisioned=true on first sight")
	}
	if !res.AllowanceGranted {
		t.Error("expected AllowanceGranted=true on first sight")
	}

	acct, err := s.GetAccountByID(res.AccountID)
	if err != nil || acct == nil {
		t.Fatalf("GetAccountByID: %v, acct=%v", err, acct)
	}
	if acct.Type != "end_user" {
		t.Errorf("expected type end_user, got %q", acct.Type)
	}
	if acct.Name != "alice@example.com" {
		t.Errorf("expected account named after email, got %q", acct.Name)
	}

	bal, err := s.GetCreditBalance(res.AccountID)
	if err != nil || bal == nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if bal.Balance != 5.0 {
		t.Errorf("expected balance 5.0 after allowance grant, got %v", bal.Balance)
	}

	// Audit trail: a monthly_allowance transaction must exist.
	txns, err := s.GetCreditTransactions(res.AccountID, 10, 0)
	if err != nil {
		t.Fatalf("GetCreditTransactions: %v", err)
	}
	found := false
	for _, txn := range txns {
		if txn.Type == "monthly_allowance" {
			found = true
		}
	}
	if !found {
		t.Error("expected a monthly_allowance credit transaction")
	}

	// Registration event recorded with the trusted_header source.
	var evCount int
	err = s.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM registration_events WHERE account_id = $1 AND source = 'trusted_header'`,
		res.AccountID).Scan(&evCount)
	if err != nil || evCount != 1 {
		t.Errorf("expected 1 trusted_header registration event, got %d (err %v)", evCount, err)
	}
}

func TestResolveEndUserAccount_IdempotentAndUpdatesMetadata(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	first, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, now)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Same identity, changed email: same account, refreshed metadata, no new grant.
	changed := resolveArgs("owui-1")
	changed.Email = "alice@new.example.com"
	second, err := s.ResolveEndUserAccount(changed, 5.0, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second.AccountID != first.AccountID {
		t.Fatalf("identity must map to a stable account: %d vs %d", first.AccountID, second.AccountID)
	}
	if second.Provisioned {
		t.Error("expected Provisioned=false on repeat sight")
	}
	if second.AllowanceGranted {
		t.Error("expected no second grant within the same month")
	}

	var email string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT email FROM federated_identities WHERE source='openwebui' AND external_id='owui-1'`).Scan(&email); err != nil {
		t.Fatalf("identity lookup: %v", err)
	}
	if email != "alice@new.example.com" {
		t.Errorf("expected refreshed email, got %q", email)
	}
}

func TestResolveEndUserAccount_MonthlyReset_NoRollover(t *testing.T) {
	s := setupTestStore(t)
	july := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	res, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, july)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Simulate spend: burn 4.5, leaving 0.5 unspent.
	holdID, err := s.ReserveCredits(res.AccountID, 4.5)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}
	if _, err := s.SettleHold(holdID, 4.5); err != nil {
		t.Fatalf("SettleHold: %v", err)
	}

	// New month: balance resets TO the grant (0.5 does not roll over into 5.5).
	august := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	res2, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, august)
	if err != nil {
		t.Fatalf("august resolve: %v", err)
	}
	if !res2.AllowanceGranted {
		t.Error("expected AllowanceGranted=true in a new month")
	}
	bal, _ := s.GetCreditBalance(res.AccountID)
	if bal.Balance != 5.0 {
		t.Errorf("expected reset-to-grant balance 5.0, got %v", bal.Balance)
	}

	// Same month again: no double grant.
	res3, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, august.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("third resolve: %v", err)
	}
	if res3.AllowanceGranted {
		t.Error("expected no double grant within August")
	}
}

func TestResolveEndUserAccount_MonthlyGrantOverride(t *testing.T) {
	s := setupTestStore(t)
	july := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	res, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, july)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Admin raises the override; next month's grant uses it.
	override := 100.0
	if err := s.SetMonthlyGrant(res.AccountID, &override); err != nil {
		t.Fatalf("SetMonthlyGrant: %v", err)
	}
	august := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	if _, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, august); err != nil {
		t.Fatalf("august resolve: %v", err)
	}
	bal, _ := s.GetCreditBalance(res.AccountID)
	if bal.Balance != 100.0 {
		t.Errorf("expected override grant 100.0, got %v", bal.Balance)
	}

	// Explicit zero blocks (balance resets to 0 next month).
	zero := 0.0
	if err := s.SetMonthlyGrant(res.AccountID, &zero); err != nil {
		t.Fatalf("SetMonthlyGrant(0): %v", err)
	}
	september := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res3, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, september)
	if err != nil {
		t.Fatalf("september resolve: %v", err)
	}
	if !res3.AllowanceGranted {
		t.Error("expected a (zero) grant event in a new month")
	}
	bal, _ = s.GetCreditBalance(res.AccountID)
	if bal.Balance != 0 {
		t.Errorf("expected blocked balance 0, got %v", bal.Balance)
	}

	// SetMonthlyGrant(nil) reverts to the env default.
	if err := s.SetMonthlyGrant(res.AccountID, nil); err != nil {
		t.Fatalf("SetMonthlyGrant(nil): %v", err)
	}
	october := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, october); err != nil {
		t.Fatalf("october resolve: %v", err)
	}
	bal, _ = s.GetCreditBalance(res.AccountID)
	if bal.Balance != 5.0 {
		t.Errorf("expected default grant 5.0 after revert, got %v", bal.Balance)
	}
}

func TestResolveEndUserAccount_ConcurrentFirstSight(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	const n = 8
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.ResolveEndUserAccount(resolveArgs("owui-race"), 5.0, now)
			if err == nil {
				ids[i] = res.AccountID
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("split-brain accounts: %v", ids)
		}
	}

	// Exactly one identity, one grant — and no orphaned accounts/balances/events
	// from losing provisioners (their transactions must have rolled back whole).
	var identities, grants, accounts, events int
	_ = s.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM federated_identities WHERE external_id='owui-race'`).Scan(&identities)
	_ = s.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM credit_transactions WHERE account_id=$1 AND type='monthly_allowance'`, ids[0]).Scan(&grants)
	_ = s.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM accounts WHERE type='end_user'`).Scan(&accounts)
	_ = s.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM registration_events WHERE source='trusted_header'`).Scan(&events)
	if identities != 1 {
		t.Errorf("expected 1 identity row, got %d", identities)
	}
	if grants != 1 {
		t.Errorf("expected exactly 1 allowance grant, got %d", grants)
	}
	if accounts != 1 {
		t.Errorf("expected 1 end_user account (no orphans from race losers), got %d", accounts)
	}
	if events != 1 {
		t.Errorf("expected 1 registration event, got %d", events)
	}
}

func TestSetTrustUserHeaders_RoundTrip(t *testing.T) {
	s := setupTestStore(t)

	keyID, err := s.CreateKey("openwebui-shared", "hash-trust", "laip_tr", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	key, err := s.GetKeyByHash("hash-trust")
	if err != nil || key == nil {
		t.Fatalf("GetKeyByHash: %v", err)
	}
	if key.TrustUserHeaders {
		t.Error("expected trust_user_headers to default to false")
	}

	if err := s.SetTrustUserHeaders(keyID, true); err != nil {
		t.Fatalf("SetTrustUserHeaders: %v", err)
	}
	key, _ = s.GetKeyByHash("hash-trust")
	if !key.TrustUserHeaders {
		t.Error("expected trust_user_headers=true after set")
	}

	byID, err := s.GetKeyByID(keyID)
	if err != nil || byID == nil || !byID.TrustUserHeaders {
		t.Errorf("GetKeyByID must surface trust flag (err %v)", err)
	}

	if err := s.SetTrustUserHeaders(99999999, true); err != ErrKeyNotFound {
		t.Errorf("expected ErrKeyNotFound for missing key, got %v", err)
	}
}

func TestLogUsage_AccountAttribution(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	keyID, err := s.CreateKey("openwebui-shared", "hash-attr", "laip_at", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	res, err := s.ResolveEndUserAccount(resolveArgs("owui-1"), 5.0, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	entry := UsageEntry{
		APIKeyID:  keyID,
		Model:     "gemma4:e4b",
		Status:    "completed",
		AccountID: &res.AccountID,
	}
	if err := s.LogUsage(entry); err != nil {
		t.Fatalf("LogUsage: %v", err)
	}

	var got *int64
	if err := s.pool.QueryRow(t.Context(),
		`SELECT account_id FROM usage_logs WHERE api_key_id = $1`, keyID).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got == nil || *got != res.AccountID {
		t.Errorf("expected usage row attributed to account %d, got %v", res.AccountID, got)
	}

	// Legacy path: nil AccountID stays NULL.
	if err := s.LogUsage(UsageEntry{APIKeyID: keyID, Model: "gemma4:e4b", Status: "completed"}); err != nil {
		t.Fatalf("LogUsage legacy: %v", err)
	}
}

// Analytics must attribute rows by the BILLING account (usage_logs.account_id)
// with a fallback to the key's account for pre-attribution rows — the Codex P1
// from the EUA-2 review.
func TestAnalytics_BillingAccountAttribution(t *testing.T) {
	s := setupTestStore(t)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	sharedAcc, sharedUser, err := s.RegisterUser("shared-analytics@example.com", "hash", "Shared")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	keyID, err := s.CreateKeyForAccount(sharedUser, sharedAcc, "openwebui-shared", "hash-analytics", "laip_an", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccount: %v", err)
	}

	endUser, err := s.ResolveEndUserAccount(resolveArgs("owui-analytics"), 5.0, now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// One end-user-attributed row, one legacy row (NULL account_id).
	if err := s.LogUsage(UsageEntry{APIKeyID: keyID, Model: "gemma4:e4b", TotalTokens: 100,
		CreditsCharged: 0.2, Status: "completed", AccountID: &endUser.AccountID}); err != nil {
		t.Fatalf("LogUsage attributed: %v", err)
	}
	if err := s.LogUsage(UsageEntry{APIKeyID: keyID, Model: "gemma4:e4b", TotalTokens: 40,
		CreditsCharged: 0.1, Status: "completed"}); err != nil {
		t.Fatalf("LogUsage legacy: %v", err)
	}

	// Filter by end-user account: only the attributed row.
	sum, err := s.GetUsageSummary(UsageFilter{AccountID: &endUser.AccountID})
	if err != nil {
		t.Fatalf("GetUsageSummary(end user): %v", err)
	}
	if sum.TotalTokens != 100 {
		t.Errorf("end-user filter: expected 100 tokens, got %d", sum.TotalTokens)
	}

	// Filter by shared account: only the legacy row (fallback via key join).
	sum, err = s.GetUsageSummary(UsageFilter{AccountID: &sharedAcc})
	if err != nil {
		t.Fatalf("GetUsageSummary(shared): %v", err)
	}
	if sum.TotalTokens != 40 {
		t.Errorf("shared filter: expected 40 tokens (legacy fallback), got %d", sum.TotalTokens)
	}

	// By-user rollup: the end-user account appears as its own owner row.
	rows, err := s.GetUsageByUser(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageByUser: %v", err)
	}
	var foundEndUser bool
	for _, r := range rows {
		if r.AccountID != nil && *r.AccountID == endUser.AccountID {
			foundEndUser = true
			if r.TotalTokens != 100 {
				t.Errorf("end-user owner row: expected 100 tokens, got %d", r.TotalTokens)
			}
		}
	}
	if !foundEndUser {
		t.Error("expected an owner row for the end-user account")
	}
}

// All key-listing paths must surface trust_user_headers consistently — the
// Codex P2 from the EUA-2 review (ListKeysByUser dropped it).
func TestListKeysByUser_SurfacesTrustFlag(t *testing.T) {
	s := setupTestStore(t)

	_, userID, err := s.RegisterUser("trust-list@example.com", "hash", "TrustList")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	keyID, err := s.CreateKeyForUser(userID, "user-key", "hash-user-trust", "laip_ut", 60)
	if err != nil {
		t.Fatalf("CreateKeyForUser: %v", err)
	}
	if err := s.SetTrustUserHeaders(keyID, true); err != nil {
		t.Fatalf("SetTrustUserHeaders: %v", err)
	}

	keys, err := s.ListKeysByUser(userID)
	if err != nil {
		t.Fatalf("ListKeysByUser: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if !keys[0].TrustUserHeaders {
		t.Error("ListKeysByUser must surface trust_user_headers=true")
	}
}

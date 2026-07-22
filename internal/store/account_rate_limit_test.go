package store

import (
	"context"
	"testing"
	"time"
)

// Per-account rate-limit override plumbing
// (docs/design/per-account-rate-limiting.md §4.2).

func TestSetAccountRateLimit_SetAndClear(t *testing.T) {
	s := setupTestStore(t)
	res, err := s.ResolveEndUserAccount(FederatedIdentity{
		Source: "openwebui", ExternalID: "rl-1", Email: "rl@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}

	perMin := 45
	if err := s.SetAccountRateLimit(res.AccountID, &perMin); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}
	var got *int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT rate_limit_per_min FROM accounts WHERE id = $1`, res.AccountID).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got == nil || *got != 45 {
		t.Errorf("expected override 45, got %v", got)
	}

	// nil clears back to the class default.
	if err := s.SetAccountRateLimit(res.AccountID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := s.pool.QueryRow(context.Background(),
		`SELECT rate_limit_per_min FROM accounts WHERE id = $1`, res.AccountID).Scan(&got); err != nil {
		t.Fatalf("read back after clear: %v", err)
	}
	if got != nil {
		t.Errorf("expected NULL after clear, got %v", *got)
	}
}

func TestSetAccountRateLimit_NotFound(t *testing.T) {
	s := setupTestStore(t)
	perMin := 45
	if err := s.SetAccountRateLimit(999999, &perMin); err == nil {
		t.Error("expected error for unknown account")
	}
}

func TestGetKeyByHash_IncludesAccountRateLimit(t *testing.T) {
	s := setupTestStore(t)
	accountID, _, err := s.RegisterUser("rl-svc@example.com", "hash", "rl-svc")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if _, err := s.CreateKeyForAccountOnly(accountID, "rl-svc-key", "hash-rl-svc", "laip_rl", 60); err != nil {
		t.Fatalf("CreateKeyForAccountOnly: %v", err)
	}

	key, err := s.GetKeyByHash("hash-rl-svc")
	if err != nil || key == nil {
		t.Fatalf("GetKeyByHash: %v, %v", key, err)
	}
	if key.AccountRateLimitPerMin != nil {
		t.Errorf("expected nil override before set, got %v", *key.AccountRateLimitPerMin)
	}

	perMin := 77
	if err := s.SetAccountRateLimit(accountID, &perMin); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}
	key, err = s.GetKeyByHash("hash-rl-svc")
	if err != nil || key == nil {
		t.Fatalf("GetKeyByHash after set: %v, %v", key, err)
	}
	if key.AccountRateLimitPerMin == nil || *key.AccountRateLimitPerMin != 77 {
		t.Errorf("expected override 77 on key lookup, got %v", key.AccountRateLimitPerMin)
	}
}

func TestGetKeyByHash_LegacyKeyWithoutAccount(t *testing.T) {
	s := setupTestStore(t)
	if _, err := s.CreateKey("legacy-rl", "hash-legacy-rl", "laip_lg", 60); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	key, err := s.GetKeyByHash("hash-legacy-rl")
	if err != nil || key == nil {
		t.Fatalf("GetKeyByHash: %v, %v", key, err)
	}
	if key.AccountRateLimitPerMin != nil {
		t.Errorf("legacy nil-account key must have nil override, got %v", *key.AccountRateLimitPerMin)
	}
}

func TestResolveEndUserAccount_CarriesRateLimit(t *testing.T) {
	s := setupTestStore(t)
	id := FederatedIdentity{Source: "openwebui", ExternalID: "rl-eua", Email: "rl-eua@example.com"}

	res, err := s.ResolveEndUserAccount(id, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	if res.RateLimitPerMin != nil {
		t.Errorf("fresh end-user account must have nil override, got %v", *res.RateLimitPerMin)
	}

	perMin := 12
	if err := s.SetAccountRateLimit(res.AccountID, &perMin); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}
	res2, err := s.ResolveEndUserAccount(id, 5.0, time.Now())
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if res2.AccountID != res.AccountID {
		t.Fatalf("expected stable account, got %d then %d", res.AccountID, res2.AccountID)
	}
	if res2.RateLimitPerMin == nil || *res2.RateLimitPerMin != 12 {
		t.Errorf("expected override 12 carried out of resolve, got %v", res2.RateLimitPerMin)
	}
}

func TestListAccountsWithBalances_IncludesRateLimit(t *testing.T) {
	s := setupTestStore(t)
	res, err := s.ResolveEndUserAccount(FederatedIdentity{
		Source: "openwebui", ExternalID: "rl-list", Email: "rl-list@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	perMin := 20
	if err := s.SetAccountRateLimit(res.AccountID, &perMin); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}

	rows, err := s.ListAccountsWithBalances()
	if err != nil {
		t.Fatalf("ListAccountsWithBalances: %v", err)
	}
	for _, r := range rows {
		if r.ID == res.AccountID {
			if r.RateLimitPerMin == nil || *r.RateLimitPerMin != 20 {
				t.Errorf("expected override 20 in listing, got %v", r.RateLimitPerMin)
			}
			return
		}
	}
	t.Fatal("account not found in listing")
}

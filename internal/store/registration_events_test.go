package store

import (
	"context"
	"testing"
)

func TestLogUsage_PersistsCreditsCharged(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateKey("credit-key", "hash-credit", "sk-cr", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	entry := UsageEntry{
		APIKeyID:         id,
		Model:            "llama3.1:8b",
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		DurationMs:       200,
		Status:           "completed",
		CreditsCharged:   0.425,
	}
	if err := s.LogUsage(entry); err != nil {
		t.Fatalf("LogUsage: %v", err)
	}

	var got float64
	err = s.pool.QueryRow(context.Background(),
		`SELECT credits_charged FROM usage_logs WHERE api_key_id = $1`, id,
	).Scan(&got)
	if err != nil {
		t.Fatalf("query credits_charged: %v", err)
	}
	if diff := got - 0.425; diff < -0.0001 || diff > 0.0001 {
		t.Errorf("expected credits_charged=0.425, got %f", got)
	}
}

func TestLogUsage_DefaultZeroCredits(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("zero-credit", "hash-zero", "sk-zr", 60)

	// Omit CreditsCharged → zero value
	if err := s.LogUsage(UsageEntry{
		APIKeyID:     id,
		Model:        "llama3.1:8b",
		TotalTokens:  1,
		PromptTokens: 1,
		Status:       "completed",
	}); err != nil {
		t.Fatalf("LogUsage: %v", err)
	}

	var got float64
	_ = s.pool.QueryRow(context.Background(),
		`SELECT credits_charged FROM usage_logs WHERE api_key_id = $1`, id,
	).Scan(&got)
	if got != 0 {
		t.Errorf("expected 0 credits, got %f", got)
	}
}

func TestBackfillRegistrationEvents_UsersAndServiceAccounts(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Register a user (personal account + user row).
	personalAccountID, userID, err := s.RegisterUser("b@example.com", "hash", "Bob")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	// Create a service account directly.
	svcAccountID, err := s.CreateAccount("svc-backfill", "service")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	// Wipe any registration_events rows from the live writers so the backfill
	// is the only thing producing rows (PR 0 registration flow does not yet
	// write events; if a future change wires it, this test still asserts
	// backfill is idempotent below).
	if _, err := s.pool.Exec(ctx, "DELETE FROM registration_events"); err != nil {
		t.Fatalf("wipe: %v", err)
	}

	if err := s.BackfillRegistrationEvents(); err != nil {
		t.Fatalf("BackfillRegistrationEvents: %v", err)
	}

	var userCount int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM registration_events WHERE kind='user' AND user_id=$1 AND source='backfill'`,
		userID,
	).Scan(&userCount)
	if err != nil {
		t.Fatalf("count user events: %v", err)
	}
	if userCount != 1 {
		t.Errorf("expected 1 backfill event for user, got %d", userCount)
	}

	var svcCount int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM registration_events WHERE kind='service' AND account_id=$1 AND source='backfill'`,
		svcAccountID,
	).Scan(&svcCount)
	if err != nil {
		t.Fatalf("count service events: %v", err)
	}
	if svcCount != 1 {
		t.Errorf("expected 1 backfill event for service account, got %d", svcCount)
	}

	// Idempotent: running again should not create duplicates.
	if err := s.BackfillRegistrationEvents(); err != nil {
		t.Fatalf("second BackfillRegistrationEvents: %v", err)
	}
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM registration_events WHERE source='backfill'`,
	).Scan(&userCount)
	if err != nil {
		t.Fatalf("count after second backfill: %v", err)
	}
	if userCount != 2 {
		t.Errorf("expected exactly 2 backfill rows (user + service) after 2 runs, got %d", userCount)
	}

	// Personal accounts should NOT get a 'service' backfill row.
	var personalSvcCount int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM registration_events WHERE kind='service' AND account_id=$1`,
		personalAccountID,
	).Scan(&personalSvcCount)
	if err != nil {
		t.Fatalf("count personal-as-service: %v", err)
	}
	if personalSvcCount != 0 {
		t.Errorf("expected 0 service backfill rows for personal account, got %d", personalSvcCount)
	}
}

func TestBackfillRegistrationEvents_SkipsAlreadyRecorded(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Pre-insert a registration_event from an admin bootstrap.
	userID, err := s.CreateAdminBootstrap("pre@example.com", "hash", "Pre")
	if err != nil {
		t.Fatalf("CreateAdminBootstrap: %v", err)
	}

	if err := s.BackfillRegistrationEvents(); err != nil {
		t.Fatalf("BackfillRegistrationEvents: %v", err)
	}

	var count int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM registration_events WHERE user_id=$1`, userID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 registration_event for user (bootstrap row retained, no backfill dup), got %d", count)
	}

	var src string
	err = s.pool.QueryRow(ctx,
		`SELECT source FROM registration_events WHERE user_id=$1`, userID,
	).Scan(&src)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if src != "admin_bootstrap" {
		t.Errorf("expected existing source retained ('admin_bootstrap'), got %q", src)
	}
}

// --- Registration-event writer tests on live paths ---

func TestRegisterUser_WritesRegistrationEvent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	accountID, userID, err := s.RegisterUser("live-signup@example.com", "hash", "Live")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	var count int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM registration_events
		 WHERE kind='user' AND user_id=$1 AND account_id=$2 AND source='public_signup'`,
		userID, accountID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 registration_event for public_signup, got %d", count)
	}
}

func TestRegisterServiceAccount_WritesRegistrationEvent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a registration token.
	tokHash := "svc-reg-token-hash"
	tokID, err := s.CreateRegistrationToken("svc-token", tokHash, 5.0, 1, nil)
	if err != nil {
		t.Fatalf("CreateRegistrationToken: %v", err)
	}

	accountID, _, _, err := s.RegisterServiceAccount(tokHash, "my-svc", "hash-svc-key", "sk-svx", 60)
	if err != nil {
		t.Fatalf("RegisterServiceAccount: %v", err)
	}

	var count int
	var gotTokenID *int64
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*), MAX(registration_token_id) FROM registration_events
		 WHERE kind='service' AND account_id=$1 AND source='registration_token'`,
		accountID,
	).Scan(&count, &gotTokenID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 registration_event for service registration, got %d", count)
	}
	if gotTokenID == nil || *gotTokenID != tokID {
		t.Errorf("expected registration_token_id=%d, got %v", tokID, gotTokenID)
	}
}

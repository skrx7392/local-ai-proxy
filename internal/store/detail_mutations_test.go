package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestGetKeyByID_ReturnsKey(t *testing.T) {
	s := setupTestStore(t)

	id, err := s.CreateKey("detail-key", "hash-det", "sk-dt", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	got, err := s.GetKeyByID(id)
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected key, got nil")
	}
	if got.ID != id || got.Name != "detail-key" || got.RateLimit != 60 {
		t.Errorf("unexpected key: %+v", got)
	}
}

func TestGetKeyByID_ReturnsRevokedKey(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("rev-key", "hash-rv", "sk-rv", 30)
	if err := s.RevokeKey(id); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	got, err := s.GetKeyByID(id)
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if got == nil || !got.Revoked {
		t.Errorf("expected revoked key, got %+v", got)
	}
}

func TestGetKeyByID_NotFound(t *testing.T) {
	s := setupTestStore(t)
	got, err := s.GetKeyByID(99999)
	if err != nil {
		t.Fatalf("GetKeyByID: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing key, got %+v", got)
	}
}

func TestUpdateKeyRateLimit_Updates(t *testing.T) {
	s := setupTestStore(t)

	id, _ := s.CreateKey("rl-key", "hash-rl", "sk-rl", 60)
	if err := s.UpdateKeyRateLimit(id, 500); err != nil {
		t.Fatalf("UpdateKeyRateLimit: %v", err)
	}

	got, _ := s.GetKeyByID(id)
	if got.RateLimit != 500 {
		t.Errorf("expected rate_limit=500, got %d", got.RateLimit)
	}
}

func TestUpdateKeyRateLimit_NotFound(t *testing.T) {
	s := setupTestStore(t)
	err := s.UpdateKeyRateLimit(99999, 100)
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestUpdateUserRoleGuarded_PromoteUser(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("promote@example.com", "h", "Promote")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.UpdateUserRoleGuarded(uid, "admin"); err != nil {
		t.Fatalf("UpdateUserRoleGuarded: %v", err)
	}
	u, _ := s.GetUserByID(uid)
	if u.Role != "admin" {
		t.Errorf("expected role=admin, got %q", u.Role)
	}
}

func TestUpdateUserRoleGuarded_DemoteLastAdmin_ReturnsErr(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateAdminBootstrap("only@example.com", "h", "Only")
	if err != nil {
		t.Fatalf("CreateAdminBootstrap: %v", err)
	}

	err = s.UpdateUserRoleGuarded(uid, "user")
	if !errors.Is(err, ErrLastActiveAdmin) {
		t.Errorf("expected ErrLastActiveAdmin, got %v", err)
	}
	u, _ := s.GetUserByID(uid)
	if u.Role != "admin" {
		t.Errorf("role must remain admin after rejection, got %q", u.Role)
	}
}

func TestUpdateUserRoleGuarded_DemoteSecondAdmin_OK(t *testing.T) {
	s := setupTestStore(t)

	_, err := s.CreateAdminBootstrap("keep@example.com", "h", "Keep")
	if err != nil {
		t.Fatalf("CreateAdminBootstrap keep: %v", err)
	}
	victim, err := s.CreateAdminBootstrap("victim@example.com", "h", "Victim")
	if err != nil {
		t.Fatalf("CreateAdminBootstrap victim: %v", err)
	}

	if err := s.UpdateUserRoleGuarded(victim, "user"); err != nil {
		t.Fatalf("UpdateUserRoleGuarded: %v", err)
	}
	u, _ := s.GetUserByID(victim)
	if u.Role != "user" {
		t.Errorf("expected role=user, got %q", u.Role)
	}
}

func TestUpdateUserRoleGuarded_InactiveAdmin_DemoteOK(t *testing.T) {
	// Inactive admins don't count toward the active-admin total, so demoting
	// one is allowed even if it's the "only admin by role".
	s := setupTestStore(t)

	_, _ = s.CreateAdminBootstrap("active@example.com", "h", "Active")
	sleeper, _ := s.CreateAdminBootstrap("sleeper@example.com", "h", "Sleeper")
	if err := s.SetUserActive(sleeper, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}

	if err := s.UpdateUserRoleGuarded(sleeper, "user"); err != nil {
		t.Fatalf("expected demote of inactive admin to succeed, got %v", err)
	}
}

func TestUpdateUserRoleGuarded_InvalidRole(t *testing.T) {
	s := setupTestStore(t)
	uid, _ := s.CreateUser("bad@example.com", "h", "Bad")
	err := s.UpdateUserRoleGuarded(uid, "superadmin")
	if !errors.Is(err, ErrInvalidRole) {
		t.Errorf("expected ErrInvalidRole, got %v", err)
	}
}

func TestUpdateUserRoleGuarded_NotFound(t *testing.T) {
	s := setupTestStore(t)
	err := s.UpdateUserRoleGuarded(99999, "admin")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("expected ErrUserNotFound, got %v", err)
	}
}

func TestUpdateUserRoleGuarded_Noop(t *testing.T) {
	// Setting the same role should succeed as a noop (no guardrail trip).
	s := setupTestStore(t)
	uid, _ := s.CreateAdminBootstrap("noop@example.com", "h", "Noop")
	if err := s.UpdateUserRoleGuarded(uid, "admin"); err != nil {
		t.Errorf("noop role change should succeed, got %v", err)
	}
}

func TestUpdateUserRoleGuarded_ConcurrentRaceSerializes(t *testing.T) {
	// Two concurrent demotions from admin→user, when only two active admins
	// exist, must serialize on the advisory lock. Exactly one should win.
	s := setupTestStore(t)

	adminA, err := s.CreateAdminBootstrap("race-a@example.com", "h", "A")
	if err != nil {
		t.Fatalf("CreateAdminBootstrap A: %v", err)
	}
	adminB, err := s.CreateAdminBootstrap("race-b@example.com", "h", "B")
	if err != nil {
		t.Fatalf("CreateAdminBootstrap B: %v", err)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, id := range []int64{adminA, adminB} {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			e := s.UpdateUserRoleGuarded(uid, "user")
			mu.Lock()
			errs = append(errs, e)
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	var successes, conflicts int
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, ErrLastActiveAdmin):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", e)
		}
	}
	if successes != 1 {
		t.Errorf("expected 1 success, got %d (errs=%v)", successes, errs)
	}
	if conflicts != 1 {
		t.Errorf("expected 1 last_admin conflict, got %d (errs=%v)", conflicts, errs)
	}

	// Exactly one admin still present.
	ctx := context.Background()
	var count int
	_ = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE role='admin' AND is_active=TRUE`,
	).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 remaining active admin, got %d", count)
	}
}

func TestListRegistrationEvents_ReturnsRowsNewestFirst(t *testing.T) {
	s := setupTestStore(t)

	// Create two users via RegisterUser (public_signup) — produces two events.
	accountA, userA, err := s.RegisterUser("a@example.com", "h", "A")
	if err != nil {
		t.Fatalf("RegisterUser A: %v", err)
	}
	_, userB, err := s.RegisterUser("b@example.com", "h", "B")
	if err != nil {
		t.Fatalf("RegisterUser B: %v", err)
	}

	events, total, err := s.ListRegistrationEvents(50, 0)
	if err != nil {
		t.Fatalf("ListRegistrationEvents: %v", err)
	}
	if total < 2 {
		t.Fatalf("expected at least 2 events, got total=%d", total)
	}
	if len(events) < 2 {
		t.Fatalf("expected at least 2 event rows, got %d", len(events))
	}
	// Most recent first.
	if events[0].UserID == nil || *events[0].UserID != userB {
		t.Errorf("expected first row to be userB (%d), got %+v", userB, events[0].UserID)
	}
	if events[1].UserID == nil || *events[1].UserID != userA {
		t.Errorf("expected second row to be userA (%d), got %+v", userA, events[1].UserID)
	}
	// Enriched fields present.
	if events[0].UserEmail == nil || *events[0].UserEmail != "b@example.com" {
		t.Errorf("expected user email 'b@example.com', got %+v", events[0].UserEmail)
	}
	if events[1].AccountID == nil || *events[1].AccountID != accountA {
		t.Errorf("expected account for user A = %d, got %+v", accountA, events[1].AccountID)
	}
}

func TestListRegistrationEvents_Pagination(t *testing.T) {
	s := setupTestStore(t)

	for i := 0; i < 5; i++ {
		email := string(rune('a'+i)) + "@pag.test"
		if _, _, err := s.RegisterUser(email, "h", "U"); err != nil {
			t.Fatalf("RegisterUser: %v", err)
		}
	}

	page1, total, _ := s.ListRegistrationEvents(2, 0)
	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
	if len(page1) != 2 {
		t.Errorf("expected 2 rows on page1, got %d", len(page1))
	}

	page3, _, _ := s.ListRegistrationEvents(2, 4)
	if len(page3) != 1 {
		t.Errorf("expected 1 row on page3 (offset=4 total=5), got %d", len(page3))
	}
}

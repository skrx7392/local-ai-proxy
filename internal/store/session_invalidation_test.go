package store

import (
	"testing"
	"time"
)

func sessionExists(t *testing.T, s *Store, tokenHash string) bool {
	t.Helper()
	sess, err := s.GetSessionByTokenHash(tokenHash)
	if err != nil {
		t.Fatalf("GetSessionByTokenHash: %v", err)
	}
	return sess != nil
}

func createTestSession(t *testing.T, s *Store, userID int64, tokenHash string) {
	t.Helper()
	if err := s.CreateSession(userID, tokenHash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestDeleteUserSessionsExcept_KeepsOnlyGivenSession(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("except@example.com", "h", "Except")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	otherUID, err := s.CreateUser("except-other@example.com", "h", "Other")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	createTestSession(t, s, uid, "hash-keep")
	createTestSession(t, s, uid, "hash-drop-1")
	createTestSession(t, s, uid, "hash-drop-2")
	createTestSession(t, s, otherUID, "hash-unrelated")

	if err := s.DeleteUserSessionsExcept(uid, "hash-keep"); err != nil {
		t.Fatalf("DeleteUserSessionsExcept: %v", err)
	}

	if !sessionExists(t, s, "hash-keep") {
		t.Error("kept session should survive")
	}
	if sessionExists(t, s, "hash-drop-1") || sessionExists(t, s, "hash-drop-2") {
		t.Error("other sessions of the user should be deleted")
	}
	if !sessionExists(t, s, "hash-unrelated") {
		t.Error("sessions of other users must be untouched")
	}
}

func TestDeactivateUserGuarded_PurgesSessions(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("deact-sess@example.com", "h", "Deact")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	createTestSession(t, s, uid, "hash-deact")

	if err := s.DeactivateUserGuarded(uid); err != nil {
		t.Fatalf("DeactivateUserGuarded: %v", err)
	}
	if sessionExists(t, s, "hash-deact") {
		t.Error("sessions must be purged when a user is deactivated")
	}
}

func TestDeactivateUserGuarded_AlreadyInactive_NoPurge(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("deact-noop@example.com", "h", "Noop")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.DeactivateUserGuarded(uid); err != nil {
		t.Fatalf("first deactivate: %v", err)
	}
	// A session created after deactivation (edge case) must survive a
	// second, no-op deactivate call.
	createTestSession(t, s, uid, "hash-noop")

	if err := s.DeactivateUserGuarded(uid); err != nil {
		t.Fatalf("second deactivate: %v", err)
	}
	if !sessionExists(t, s, "hash-noop") {
		t.Error("no-op deactivate must not purge sessions")
	}
}

func TestUpdateUserRoleGuarded_DemotePurgesSessions(t *testing.T) {
	s := setupTestStore(t)

	admin1, err := s.CreateUser("demote-a1@example.com", "h", "A1")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	admin2, err := s.CreateUser("demote-a2@example.com", "h", "A2")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.UpdateUserRoleGuarded(admin1, "admin"); err != nil {
		t.Fatalf("promote a1: %v", err)
	}
	if err := s.UpdateUserRoleGuarded(admin2, "admin"); err != nil {
		t.Fatalf("promote a2: %v", err)
	}
	createTestSession(t, s, admin1, "hash-demote")

	if err := s.UpdateUserRoleGuarded(admin1, "user"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if sessionExists(t, s, "hash-demote") {
		t.Error("sessions must be purged when an admin is demoted")
	}
}

func TestUpdateUserRoleGuarded_PromotePurgesSessions(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("promote-sess@example.com", "h", "P")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	createTestSession(t, s, uid, "hash-promote")

	if err := s.UpdateUserRoleGuarded(uid, "admin"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// A promoted user's old 7-day session would outlive the 6-hour admin
	// session policy; role changes force a fresh login.
	if sessionExists(t, s, "hash-promote") {
		t.Error("sessions must be purged when a user is promoted")
	}
}

func TestUpdateUserRoleGuarded_NoopKeepsSessions(t *testing.T) {
	s := setupTestStore(t)

	uid, err := s.CreateUser("noop-role@example.com", "h", "N")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	createTestSession(t, s, uid, "hash-role-noop")

	if err := s.UpdateUserRoleGuarded(uid, "user"); err != nil {
		t.Fatalf("noop role change: %v", err)
	}
	if !sessionExists(t, s, "hash-role-noop") {
		t.Error("no-op role change must not purge sessions")
	}
}

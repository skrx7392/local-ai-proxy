package store

import (
	"testing"
)

func TestSettleHold_ReturnsActualCost(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("settle-actual-cost", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, err := s.ReserveCredits(accID, 30)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}

	charged, err := s.SettleHold(holdID, 20)
	if err != nil {
		t.Fatalf("SettleHold: %v", err)
	}
	if !almostEqual(charged, 20) {
		t.Errorf("expected charged=20, got %f", charged)
	}
}

func TestSettleHold_AfterReleaseReturnsZero(t *testing.T) {
	s := setupTestStore(t)
	accID, _ := s.CreateAccount("settle-after-release", "personal")
	_ = s.InitCreditBalance(accID)
	_ = s.AddCredits(accID, 100, "grant")

	holdID, _ := s.ReserveCredits(accID, 30)
	_ = s.ReleaseHold(holdID)

	charged, err := s.SettleHold(holdID, 20)
	if err != nil {
		t.Fatalf("SettleHold after release: %v", err)
	}
	if charged != 0 {
		t.Errorf("expected 0 charged for already-released hold, got %f", charged)
	}
}

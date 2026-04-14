package credits

import (
	"testing"
)

func TestSeedDefaultPricing(t *testing.T) {
	s := setupTestStore(t)

	if err := SeedDefaultPricing(s); err != nil {
		t.Fatalf("first SeedDefaultPricing: %v", err)
	}

	// Must be idempotent (UpsertPricing uses ON CONFLICT).
	if err := SeedDefaultPricing(s); err != nil {
		t.Fatalf("second SeedDefaultPricing: %v", err)
	}

	pricing, err := s.ListActivePricing()
	if err != nil {
		t.Fatalf("ListActivePricing: %v", err)
	}
	if len(pricing) < 7 {
		t.Errorf("expected >=7 seeded pricing rows, got %d", len(pricing))
	}
}

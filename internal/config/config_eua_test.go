package config

import "testing"

// Non-finite grants would poison every downstream balance comparison (NaN
// comparisons are all false → unlimited spend), so Load must reject them.
func TestLoad_EndUserMonthlyGrant_Validation(t *testing.T) {
	t.Setenv("ADMIN_KEY", "test-admin")
	t.Setenv("DATABASE_URL", "postgres://x/y")

	for _, bad := range []string{"NaN", "+Inf", "-Inf", "Infinity", "-1"} {
		t.Setenv("END_USER_MONTHLY_GRANT", bad)
		if _, err := Load(); err == nil {
			t.Errorf("END_USER_MONTHLY_GRANT=%q must be rejected", bad)
		}
	}

	t.Setenv("END_USER_MONTHLY_GRANT", "7.5")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid grant rejected: %v", err)
	}
	if cfg.EndUserMonthlyGrant != 7.5 {
		t.Errorf("expected 7.5, got %v", cfg.EndUserMonthlyGrant)
	}

	t.Setenv("END_USER_MONTHLY_GRANT", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("default case errored: %v", err)
	}
	if cfg.EndUserMonthlyGrant != 5.0 {
		t.Errorf("expected default 5.0, got %v", cfg.EndUserMonthlyGrant)
	}
}

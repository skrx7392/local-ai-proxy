package config

import (
	"strings"
	"testing"
)

// Account-level rate-limit env parsing
// (docs/design/per-account-rate-limiting.md §4.1). Base env so Load()
// passes its required-var checks.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ADMIN_KEY", "test-admin")
	t.Setenv("DATABASE_URL", "postgres://test")
}

func TestLoad_AccountRateLimitDefaults(t *testing.T) {
	setBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AccountRateLimitPerMin != 300 {
		t.Errorf("expected ACCOUNT_RATELIMIT_PER_MIN default 300, got %d", cfg.AccountRateLimitPerMin)
	}
	if cfg.EndUserRateLimitPerMin != 30 {
		t.Errorf("expected END_USER_RATELIMIT_PER_MIN default 30, got %d", cfg.EndUserRateLimitPerMin)
	}
}

func TestLoad_AccountRateLimitOverrides(t *testing.T) {
	setBaseEnv(t)
	t.Setenv("ACCOUNT_RATELIMIT_PER_MIN", "120")
	t.Setenv("END_USER_RATELIMIT_PER_MIN", "10")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AccountRateLimitPerMin != 120 || cfg.EndUserRateLimitPerMin != 10 {
		t.Errorf("expected 120/10, got %d/%d", cfg.AccountRateLimitPerMin, cfg.EndUserRateLimitPerMin)
	}
}

func TestLoad_AccountRateLimitInvalid(t *testing.T) {
	// No 0=disabled semantic: silently disabling a security limit is worse
	// than failing to boot.
	cases := []struct {
		name, key, value string
	}{
		{"zero account", "ACCOUNT_RATELIMIT_PER_MIN", "0"},
		{"negative account", "ACCOUNT_RATELIMIT_PER_MIN", "-5"},
		{"non-numeric account", "ACCOUNT_RATELIMIT_PER_MIN", "abc"},
		{"above cap account", "ACCOUNT_RATELIMIT_PER_MIN", "10001"},
		{"zero end-user", "END_USER_RATELIMIT_PER_MIN", "0"},
		{"above cap end-user", "END_USER_RATELIMIT_PER_MIN", "999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected boot failure for %s=%s", tc.key, tc.value)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error should name the variable, got: %v", err)
			}
		})
	}
}

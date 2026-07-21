package config

import "testing"

// A malformed webhook URL would make every cap-hit notification fail
// silently at runtime, so Load must reject it at boot instead.
func TestLoad_CreditAlertWebhookURL_Validation(t *testing.T) {
	t.Setenv("ADMIN_KEY", "test-admin")
	t.Setenv("DATABASE_URL", "postgres://x/y")

	for _, bad := range []string{"://nope", "ftp://host/hook", "not a url", "/relative/only"} {
		t.Setenv("CREDIT_ALERT_WEBHOOK_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("CREDIT_ALERT_WEBHOOK_URL=%q must be rejected", bad)
		}
	}

	t.Setenv("CREDIT_ALERT_WEBHOOK_URL", "http://openwebui-discord-bot.openwebui.svc.cluster.local:8080/credit-request")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("valid webhook URL rejected: %v", err)
	}
	if cfg.CreditAlertWebhookURL == "" {
		t.Error("expected webhook URL to be carried through")
	}

	// Unset = disabled, never an error.
	t.Setenv("CREDIT_ALERT_WEBHOOK_URL", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unset case errored: %v", err)
	}
	if cfg.CreditAlertWebhookURL != "" {
		t.Errorf("expected empty (disabled), got %q", cfg.CreditAlertWebhookURL)
	}
}

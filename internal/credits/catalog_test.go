package credits

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestFreshDatabaseHasNoPricing pins the seed-removal invariant: nothing
// seeds the pricing catalog, so a fresh database has zero active rows until
// an operator adds pricing via the admin API.
func TestFreshDatabaseHasNoPricing(t *testing.T) {
	s := setupTestStore(t)

	pricing, err := s.ListActivePricing()
	if err != nil {
		t.Fatalf("ListActivePricing: %v", err)
	}
	if len(pricing) != 0 {
		t.Errorf("expected 0 pricing rows on a fresh database, got %d", len(pricing))
	}
}

func TestWarnIfPricingEmpty_EmptyCatalogWarns(t *testing.T) {
	s := setupTestStore(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if err := WarnIfPricingEmpty(s, logger); err != nil {
		t.Fatalf("WarnIfPricingEmpty: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected a warn-level log on an empty catalog, got: %q", out)
	}
	if !strings.Contains(out, "pricing catalog is empty") {
		t.Errorf("expected empty-catalog message, got: %q", out)
	}
}

func TestWarnIfPricingEmpty_PricedCatalogSilent(t *testing.T) {
	s := setupTestStore(t)

	if err := s.UpsertPricing("test-model:1b", 0.001, 0.001, 300); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	if err := WarnIfPricingEmpty(s, logger); err != nil {
		t.Fatalf("WarnIfPricingEmpty: %v", err)
	}

	if out := buf.String(); out != "" {
		t.Errorf("expected no log output when pricing exists, got: %q", out)
	}
}

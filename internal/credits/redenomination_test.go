package credits

// Proof tests for the OSS-5 pricing re-denomination: rates changed from
// credits per TOKEN to credits per MILLION tokens (per-MTok). The migration
// and the new charge math must be provably cost-neutral — effective prices,
// reserves, and settled charges are identical to the old per-token world.

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// equalAt6dp reports whether two credit amounts are equal at the 6 decimal
// places the database stores (credit_balances / credit_holds /
// credit_transactions are all DECIMAL(15,6)).
func equalAt6dp(a, b float64) bool {
	return math.Round(a*1e6) == math.Round(b*1e6)
}

// TestEstimateCost_PerMTokMatchesOldPerTokenMath is the unit-level
// equal-cost proof. Expected costs are HARDCODED from the old per-token
// math (cost = tokens × per-token rate); the new math (cost = tokens ×
// per-MTok rate / 1e6) on ×1e6-migrated rates must reproduce them exactly
// at the database's 6-decimal precision (and to 1e-9 in float64).
func TestEstimateCost_PerMTokMatchesOldPerTokenMath(t *testing.T) {
	cases := []struct {
		name                   string
		promptTokens           int
		completionTokens       int
		oldPromptRatePerToken  float64
		oldCompletionRatePerTk float64
		// wantCost is the OLD math's result, hardcoded — not computed here.
		wantCost float64
	}{
		// Production pricing row at migration time: gemma4:e4b at 0.004/token.
		{"prod gemma4:e4b", 1234, 567, 0.004, 0.004, 7.204},
		// Asymmetric rates catch a prompt/completion column swap.
		{"asymmetric rates", 1234, 567, 0.004, 0.001, 5.503},
		{"small request", 10, 5, 0.002, 0.002, 0.03},
		{"long-tail decimal rate", 100, 200, 0.0000123456789, 0.0000123456789, 0.00370370367},
		{"large volume", 100000, 50000, 0.01, 0.005, 1250},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Guard: the hardcoded expectation really is what the old math
			// (tokens × per-token rate) produced.
			oldCost := float64(c.promptTokens)*c.oldPromptRatePerToken +
				float64(c.completionTokens)*c.oldCompletionRatePerTk
			if math.Abs(oldCost-c.wantCost) > 1e-9 {
				t.Fatalf("fixture error: old math gives %.12f, hardcoded want %.12f", oldCost, c.wantCost)
			}

			// The migration backfill is rate_mtok = rate * 1e6.
			p := &store.CreditPricing{
				PromptRatePerMTok:     c.oldPromptRatePerToken * 1e6,
				CompletionRatePerMTok: c.oldCompletionRatePerTk * 1e6,
			}
			got := EstimateCost(p, c.promptTokens, c.completionTokens)

			if math.Abs(got-c.wantCost) > 1e-9 {
				t.Errorf("EstimateCost = %.12f, want %.12f (old per-token math)", got, c.wantCost)
			}
			if !equalAt6dp(got, c.wantCost) {
				t.Errorf("EstimateCost = %.6f differs from old math %.6f at DB precision", got, c.wantCost)
			}
		})
	}
}

// TestRedenomination_MigrationIdempotent proves the schema.sql migration is
// safe under the "re-run the whole schema on every boot" pattern: a legacy
// row (per-token columns set, *_mtok columns NULL) is backfilled ×1e6
// exactly once, and repeated boots never multiply the value again. It also
// proves the backfill never clobbers a rate that was later re-priced
// through the new per-MTok API.
func TestRedenomination_MigrationIdempotent(t *testing.T) {
	s := setupTestStore(t)
	dbURL := os.Getenv("DATABASE_URL")
	ctx := context.Background()

	// Simulate a row written by the previous (per-token) release: only the
	// legacy columns are populated; the *_mtok columns stay NULL.
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO credit_pricing (model_id, prompt_rate, completion_rate, typical_completion)
		 VALUES ('oss5-legacy:test', 0.004, 0.001, 500)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE credit_pricing
		 SET prompt_rate_mtok = NULL, completion_rate_mtok = NULL
		 WHERE model_id = 'oss5-legacy:test'`); err != nil {
		t.Fatalf("clear mtok columns: %v", err)
	}

	readRaw := func(t *testing.T, model string) (prompt, completion string) {
		t.Helper()
		if err := s.Pool().QueryRow(ctx,
			`SELECT prompt_rate_mtok::text, completion_rate_mtok::text
			 FROM credit_pricing WHERE model_id = $1`, model).Scan(&prompt, &completion); err != nil {
			t.Fatalf("read raw mtok columns: %v", err)
		}
		return prompt, completion
	}

	// Boot the app three more times. store.New re-applies the embedded
	// schema.sql in full — exactly what production does on every deploy.
	for boot := 1; boot <= 3; boot++ {
		s2, err := store.New(context.Background(), dbURL)
		if err != nil {
			t.Fatalf("boot %d: %v", boot, err)
		}
		s2.Close()

		prompt, completion := readRaw(t, "oss5-legacy:test")
		if prompt != "4000.000000" {
			t.Fatalf("boot %d: prompt_rate_mtok = %s, want 4000.000000 (backfill compounded or mis-scaled)", boot, prompt)
		}
		if completion != "1000.000000" {
			t.Fatalf("boot %d: completion_rate_mtok = %s, want 1000.000000", boot, completion)
		}
	}

	// A rate written through the new per-MTok API must survive further
	// boots untouched, even when it has more precision than the deprecated
	// per-token column can mirror (2000.000001 / 1e6 rounds at 10dp).
	if err := s.UpsertPricing("oss5-repriced:test", 2000.000001, 3000, 500); err != nil {
		t.Fatalf("UpsertPricing: %v", err)
	}
	s3, err := store.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("boot after reprice: %v", err)
	}
	s3.Close()
	prompt, completion := readRaw(t, "oss5-repriced:test")
	if prompt != "2000.000001" {
		t.Fatalf("prompt_rate_mtok = %s after reboot, want 2000.000001 (backfill clobbered a repriced row)", prompt)
	}
	if completion != "3000.000000" {
		t.Fatalf("completion_rate_mtok = %s after reboot, want 3000.000000", completion)
	}
}

// TestRedenomination_EqualCostProof is the end-to-end equal-cost proof:
// a legacy per-token pricing row is migrated by re-running the schema, and
// then BOTH money paths — the reserve estimate (EstimateCost sizing
// ReserveCredits) and the settle charge (EstimateCost feeding SettleHold) —
// produce, on the migrated ×1e6 rates, exactly the credits_charged that the
// old per-token math produced. Expected values are hardcoded from the old
// math: reserve = 1234×0.004 + 567×0.001 = 5.503, settle = 903×0.004 +
// 411×0.001 = 4.023.
func TestRedenomination_EqualCostProof(t *testing.T) {
	s := setupTestStore(t)
	dbURL := os.Getenv("DATABASE_URL")
	ctx := context.Background()

	const (
		reservePromptTokens    = 1234
		reserveCompletionToks  = 567
		settlePromptTokens     = 903
		settleCompletionTokens = 411
		// Old per-token rates and the costs the OLD math charged for them.
		wantReserve = 5.503 // 1234×0.004 + 567×0.001
		wantSettle  = 4.023 // 903×0.004 + 411×0.001
	)

	// Legacy row exactly as the per-token release left it, then one "boot"
	// to run the ×1e6 backfill.
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO credit_pricing (model_id, prompt_rate, completion_rate, typical_completion)
		 VALUES ('oss5-cost:test', 0.004, 0.001, 500)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`UPDATE credit_pricing SET prompt_rate_mtok = NULL, completion_rate_mtok = NULL
		 WHERE model_id = 'oss5-cost:test'`); err != nil {
		t.Fatalf("clear mtok columns: %v", err)
	}
	s2, err := store.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("migration boot: %v", err)
	}
	s2.Close()

	p, err := s.GetPricingByModel("oss5-cost:test")
	if err != nil || p == nil {
		t.Fatalf("GetPricingByModel: %v (pricing=%v)", err, p)
	}
	if !equalAt6dp(p.PromptRatePerMTok, 4000) || !equalAt6dp(p.CompletionRatePerMTok, 1000) {
		t.Fatalf("migrated rates = %.6f/%.6f per MTok, want 4000/1000", p.PromptRatePerMTok, p.CompletionRatePerMTok)
	}

	// Reserve estimate — same code path the proxy uses to size the hold.
	reserve := EstimateCost(p, reservePromptTokens, reserveCompletionToks)
	if !equalAt6dp(reserve, wantReserve) || math.Abs(reserve-wantReserve) > 1e-9 {
		t.Fatalf("reserve estimate = %.12f, want %.12f (old per-token math)", reserve, wantReserve)
	}

	// Settle charge — same code path settleCredits/settleStreamCredits use.
	settleCost := EstimateCost(p, settlePromptTokens, settleCompletionTokens)
	if !equalAt6dp(settleCost, wantSettle) || math.Abs(settleCost-wantSettle) > 1e-9 {
		t.Fatalf("settle cost = %.12f, want %.12f (old per-token math)", settleCost, wantSettle)
	}

	// Drive the amounts through the real reserve/settle flow and verify the
	// database-recorded charge is identical to the old world.
	accID, err := s.CreateAccount("oss5-cost-proof", "personal")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if err := s.InitCreditBalance(accID); err != nil {
		t.Fatalf("InitCreditBalance: %v", err)
	}
	if err := s.AddCredits(accID, 1000, "test grant"); err != nil {
		t.Fatalf("AddCredits: %v", err)
	}

	holdID, err := s.ReserveCredits(accID, reserve)
	if err != nil {
		t.Fatalf("ReserveCredits: %v", err)
	}
	var heldAmount float64
	if err := s.Pool().QueryRow(ctx,
		`SELECT amount FROM credit_holds WHERE id = $1`, holdID).Scan(&heldAmount); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if !equalAt6dp(heldAmount, wantReserve) {
		t.Errorf("hold amount = %.6f, want %.6f", heldAmount, wantReserve)
	}

	charged, err := s.SettleHold(holdID, settleCost)
	if err != nil {
		t.Fatalf("SettleHold: %v", err)
	}
	if !equalAt6dp(charged, wantSettle) {
		t.Errorf("charged = %.6f, want %.6f", charged, wantSettle)
	}

	bal, err := s.GetCreditBalance(accID)
	if err != nil || bal == nil {
		t.Fatalf("GetCreditBalance: %v", err)
	}
	if !equalAt6dp(bal.Balance, 1000-wantSettle) {
		t.Errorf("balance = %.6f, want %.6f", bal.Balance, 1000-wantSettle)
	}
	if !equalAt6dp(bal.Reserved, 0) {
		t.Errorf("reserved = %.6f, want 0", bal.Reserved)
	}

	txns, err := s.GetCreditTransactions(accID, 10, 0)
	if err != nil {
		t.Fatalf("GetCreditTransactions: %v", err)
	}
	var usageAmount *float64
	for i := range txns {
		if txns[i].Type == "usage" {
			usageAmount = &txns[i].Amount
			break
		}
	}
	if usageAmount == nil {
		t.Fatal("no usage transaction recorded")
	}
	if !equalAt6dp(*usageAmount, -wantSettle) {
		t.Errorf("usage transaction amount = %.6f, want %.6f", *usageAmount, -wantSettle)
	}
}

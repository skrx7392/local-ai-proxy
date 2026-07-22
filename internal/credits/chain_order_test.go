package credits

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/creditrequest"
	"github.com/krishna/local-ai-proxy/internal/ratelimit"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// Chain-order regression (docs/design/per-account-rate-limiting.md §3.3):
// main.go mounts billing → rateLimit → creditGate. RecordCapHit — the
// credit-request/Discord trigger — only fires inside the gate's 402 branch,
// so it must still fire for an over-cap request that PASSES the rate gates,
// and an over-cap AND over-rate request must see 429 (rate gate first).

// buildChain mirrors the /api/v1/ middleware order from cmd/proxy/main.go.
func buildChain(db *store.Store, capHits *creditrequest.Recorder, limits ratelimit.Limits) http.Handler {
	clockNow := time.Now
	keys := ratelimit.NewWithClock(clockNow)
	accounts := ratelimit.NewWithClock(clockNow)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rl := ratelimit.Middleware(keys, accounts, limits, nil)
	gate := CreditGate(db, nil, capHits)
	return billing.Middleware(db, 0)(rl(gate(next)))
}

func overCapRequest(t *testing.T, chain http.Handler, key *store.APIKey, externalID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
	req.Header.Set(billing.HeaderUserID, externalID)
	req.Header.Set(billing.HeaderUserEmail, externalID+"@example.com")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req.WithContext(auth.WithKey(req.Context(), key)))
	return rec
}

func TestChainOrder_CapHitStillFiresThroughRateLimiter(t *testing.T) {
	db := setupTestStore(t)
	capHits := creditrequest.New(db, "", 0) // zero default grant: capped from the first request
	chain := buildChain(db, capHits, ratelimit.Limits{EndUserPerMin: 100, ServicePerMin: 300})

	sharedAcc, _, _ := db.RegisterUser("chain-shared@example.com", "hash", "ChainShared")
	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true, RateLimit: 100}

	rec := overCapRequest(t, chain, key, "chain-cap-1")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("rate-passing over-cap request must 402, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "monthly_limit_reached") {
		t.Errorf("expected monthly_limit_reached, got: %s", rec.Body.String())
	}

	capHits.Wait()
	rows, err := db.ListCreditRequests("pending", time.Now())
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("cap-hit through the reordered chain must file exactly 1 credit request, got %d", len(rows))
	}
}

func TestChainOrder_OverCapAndOverRateSees429(t *testing.T) {
	db := setupTestStore(t)
	capHits := creditrequest.New(db, "", 0)
	chain := buildChain(db, capHits, ratelimit.Limits{EndUserPerMin: 1, ServicePerMin: 300})

	sharedAcc, _, _ := db.RegisterUser("chain-shared-2@example.com", "hash", "ChainShared2")
	key := &store.APIKey{ID: 1, AccountID: &sharedAcc, TrustUserHeaders: true, RateLimit: 100}

	// First request consumes the 1/min account budget and 402s at the gate.
	if rec := overCapRequest(t, chain, key, "chain-cap-2"); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("first request should 402, got %d", rec.Code)
	}
	// Second request is over-rate AND over-cap: the rate gate wins (429).
	rec := overCapRequest(t, chain, key, "chain-cap-2")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-rate + over-cap must 429 under the new order, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("expected rate_limit_exceeded body, got: %s", rec.Body.String())
	}
}

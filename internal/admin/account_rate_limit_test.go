package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// PUT /api/admin/accounts/{id}/rate-limit + effective values on the accounts
// listing (docs/design/per-account-rate-limiting.md §4.3).

func TestAdmin_SetAccountRateLimit(t *testing.T) {
	h, s := setupAdminTest(t)
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-rl-1", Email: "rl@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	idStr := strconv.FormatInt(res.AccountID, 10)

	rec := adminPut(t, h, "/api/admin/accounts/"+idStr+"/rate-limit", `{"rate_limit_per_min":45}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status          string `json:"status"`
		RateLimitPerMin *int   `json:"rate_limit_per_min"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Status != "updated" || resp.RateLimitPerMin == nil || *resp.RateLimitPerMin != 45 {
		t.Errorf("unexpected response: %s", rec.Body.String())
	}

	// Explicit null clears the override.
	rec = adminPut(t, h, "/api/admin/accounts/"+idStr+"/rate-limit", `{"rate_limit_per_min":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	res2, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-rl-1", Email: "rl@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if res2.RateLimitPerMin != nil {
		t.Errorf("expected cleared override, got %v", *res2.RateLimitPerMin)
	}
}

func TestAdmin_SetAccountRateLimit_Errors(t *testing.T) {
	h, s := setupAdminTest(t)
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-rl-err", Email: "rl-err@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	idStr := strconv.FormatInt(res.AccountID, 10)
	path := "/api/admin/accounts/" + idStr + "/rate-limit"

	// Absent field ≠ null: {} must 400, never silently clear.
	if rec := adminPut(t, h, path, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("absent field: expected 400, got %d", rec.Code)
	}
	// Explicit 0 rejected — blocking is credits/deactivation's job, not a
	// 429 that SDKs retry forever.
	if rec := adminPut(t, h, path, `{"rate_limit_per_min":0}`); rec.Code != http.StatusBadRequest {
		t.Errorf("zero: expected 400, got %d", rec.Code)
	}
	if rec := adminPut(t, h, path, `{"rate_limit_per_min":-5}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative: expected 400, got %d", rec.Code)
	}
	if rec := adminPut(t, h, path, `{"rate_limit_per_min":10001}`); rec.Code != http.StatusBadRequest {
		t.Errorf("above cap: expected 400, got %d", rec.Code)
	}
	if rec := adminPut(t, h, path, `{"rate_limit_per_min":"fast"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer: expected 400, got %d", rec.Code)
	}
	if rec := adminPut(t, h, "/api/admin/accounts/999999/rate-limit", `{"rate_limit_per_min":45}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown account: expected 404, got %d", rec.Code)
	}
}

func TestListAccounts_EffectiveRateLimit(t *testing.T) {
	_, s := setupAdminTest(t)
	// Fresh handler with class defaults wired (setupAdminTest passes zero
	// Options; effective values need the env defaults).
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(s, testAdminKey, usageCh, Options{
		AccountRateLimitPerMin: 300,
		EndUserRateLimitPerMin: 30,
	})

	// End-user account with an override; service (personal) account without.
	eua, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: "admin-rl-list", Email: "rl-list@example.com",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	override := 12
	if err := s.SetAccountRateLimit(eua.AccountID, &override); err != nil {
		t.Fatalf("SetAccountRateLimit: %v", err)
	}
	svcAcc, _, err := s.RegisterUser("svc-rl@example.com", "hash", "svc-rl")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	req := adminGet(t, h, "/api/admin/accounts?envelope=1&limit=100")
	if req.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", req.Code, req.Body.String())
	}
	var envelope struct {
		Data []struct {
			ID                       int64 `json:"id"`
			RateLimitPerMin          *int  `json:"rate_limit_per_min"`
			EffectiveRateLimitPerMin int   `json:"effective_rate_limit_per_min"`
		} `json:"data"`
	}
	if err := json.Unmarshal(req.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("parse: %v", err)
	}
	rows := envelope.Data

	var sawEUA, sawSvc bool
	for _, r := range rows {
		switch r.ID {
		case eua.AccountID:
			sawEUA = true
			if r.RateLimitPerMin == nil || *r.RateLimitPerMin != 12 {
				t.Errorf("end-user override: expected 12, got %v", r.RateLimitPerMin)
			}
			if r.EffectiveRateLimitPerMin != 12 {
				t.Errorf("end-user effective: expected 12 (override wins), got %d", r.EffectiveRateLimitPerMin)
			}
		case svcAcc:
			sawSvc = true
			if r.RateLimitPerMin != nil {
				t.Errorf("service override: expected null, got %v", *r.RateLimitPerMin)
			}
			if r.EffectiveRateLimitPerMin != 300 {
				t.Errorf("service effective: expected class default 300, got %d", r.EffectiveRateLimitPerMin)
			}
		}
	}
	if !sawEUA || !sawSvc {
		t.Fatalf("missing accounts in listing (eua=%v svc=%v)", sawEUA, sawSvc)
	}
}

func TestConfigSnapshot_IncludesAccountRateLimits(t *testing.T) {
	_, s := setupAdminTest(t)
	usageCh := make(chan store.UsageEntry, 10)
	h := NewHandler(s, testAdminKey, usageCh, Options{
		Snapshot: ConfigSnapshot{
			AccountRateLimitPerMinute: 300,
			EndUserRateLimitPerMinute: 30,
		},
	})

	rec := adminGet(t, h, "/api/admin/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var snap map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if snap["account_rate_limit_per_minute"] != float64(300) {
		t.Errorf("expected account_rate_limit_per_minute 300, got %v", snap["account_rate_limit_per_minute"])
	}
	if snap["end_user_rate_limit_per_minute"] != float64(30) {
		t.Errorf("expected end_user_rate_limit_per_minute 30, got %v", snap["end_user_rate_limit_per_minute"])
	}
}

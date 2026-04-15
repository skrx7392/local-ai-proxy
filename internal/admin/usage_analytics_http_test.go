package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// seedUsageFixture provisions a small analytics fixture:
//   - 1 personal account with 1 user-owned key
//   - 1 service account with 1 service key
//   - usage rows at known timestamps so tests can pick windows that
//     deterministically select subsets.
//
// Returns key IDs and the anchor time `t0`.
type usageFixture struct {
	personalAcct, serviceAcct int64
	userID                    int64
	keyUser, keyService       int64
	t0                        time.Time
}

func seedUsageFixtureHTTP(t *testing.T, s *store.Store) usageFixture {
	t.Helper()
	ctx := context.Background()

	// --- accounts + user + keys
	personal, err := s.CreateAccount("alice-personal", "personal")
	if err != nil {
		t.Fatalf("CreateAccount personal: %v", err)
	}
	svc, err := s.CreateAccount("svc-acct", "service")
	if err != nil {
		t.Fatalf("CreateAccount service: %v", err)
	}
	uid, err := s.CreateUser("alice@example.com", "hash", "Alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	keyUser, err := s.CreateKeyForAccount(uid, personal, "alice-key", "hashU", "sk-u", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccount user: %v", err)
	}
	keyService, err := s.CreateKey("svc-key", "hashS", "sk-s", 60)
	if err != nil {
		t.Fatalf("CreateKey service: %v", err)
	}
	// Link the service key to the service account directly.
	if _, err := s.Pool().Exec(ctx, `UPDATE api_keys SET account_id = $1 WHERE id = $2`, svc, keyService); err != nil {
		t.Fatalf("link service key to account: %v", err)
	}

	// --- usage rows at fixed timestamps so filter windows are deterministic.
	// t0 chosen well in the past so a default (now-7d, now) window includes
	// them. We insert spanning 10 hours starting at t0.
	t0 := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)

	type row struct {
		keyID        int64
		model        string
		prompt, comp int
		dur          int64
		credit       float64
		status       string
		offset       time.Duration
	}
	rows := []row{
		{keyUser, "llama3.1:8b", 100, 50, 200, 0.30, "completed", 0},
		{keyUser, "llama3.1:8b", 200, 100, 300, 0.60, "completed", 1 * time.Hour},
		{keyUser, "gpt-4o-mini", 50, 25, 150, 0.15, "completed", 2 * time.Hour},
		{keyService, "llama3.1:8b", 500, 250, 500, 1.50, "completed", 5 * time.Hour},
		{keyService, "gpt-4o-mini", 20, 10, 80, 0.06, "error", 6 * time.Hour},
	}
	for _, r := range rows {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.keyID, r.model, r.prompt, r.comp, r.prompt+r.comp, r.dur, r.status, r.credit, t0.Add(r.offset),
		); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}
	if _, err := s.Pool().Exec(ctx, "ANALYZE usage_logs, api_keys"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	return usageFixture{
		personalAcct: personal,
		serviceAcct:  svc,
		userID:       uid,
		keyUser:      keyUser,
		keyService:   keyService,
		t0:           t0,
	}
}

func doAdmin(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- summary --------------------------------------------------------------

func TestUsageSummary_HappyPath(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage/summary?since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data       usageSummaryDTO `json:"data"`
		Pagination *Pagination     `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Pagination != nil {
		t.Errorf("summary must not include pagination, got %+v", body.Pagination)
	}
	if body.Data.Requests != 5 {
		t.Errorf("requests = %d, want 5", body.Data.Requests)
	}
	if body.Data.Errors != 1 {
		t.Errorf("errors = %d, want 1", body.Data.Errors)
	}
	// Credits = 0.30+0.60+0.15+1.50+0.06 = 2.61
	if body.Data.Credits < 2.60 || body.Data.Credits > 2.62 {
		t.Errorf("credits = %f, want ~2.61", body.Data.Credits)
	}
}

func TestUsageSummary_SilentlyIgnoresPaginationParams(t *testing.T) {
	// limit/offset are meaningless on summary. Rather than 400 on unknown
	// query params, the handler accepts and ignores them so forward-compat
	// changes (e.g. adding filters to summary later) don't break clients.
	h, s := setupAdminTest(t)
	seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage/summary?limit=5&offset=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestUsageSummary_ModelFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage/summary?model=llama3.1:8b&since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data usageSummaryDTO `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Data.Requests != 3 {
		t.Errorf("requests = %d, want 3 (only llama rows)", body.Data.Requests)
	}
}

// --- by-model -------------------------------------------------------------

func TestUsageByModel_GroupingAndOrder(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage/by-model?since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data       []modelUsageDTO `json:"data"`
		Pagination *Pagination     `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Pagination == nil {
		t.Fatal("by-model must include pagination envelope")
	}
	if body.Pagination.Total != 2 {
		t.Errorf("total = %d, want 2 models", body.Pagination.Total)
	}
	if len(body.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(body.Data))
	}
	// Ordered by total_tokens desc: llama has 150+300+750 = 1200,
	// gpt has 75+30 = 105. llama must come first.
	if body.Data[0].Model != "llama3.1:8b" {
		t.Errorf("first model = %q, want llama3.1:8b", body.Data[0].Model)
	}
}

func TestUsageByModel_PaginationTotalIsPreSlice(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage/by-model?limit=1&offset=0&since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Data       []modelUsageDTO `json:"data"`
		Pagination *Pagination     `json:"pagination"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Errorf("len(data) = %d, want 1 (limited)", len(body.Data))
	}
	if body.Pagination == nil || body.Pagination.Total != 2 {
		t.Errorf("pagination.total must reflect pre-slice count, got %+v", body.Pagination)
	}
}

// --- by-user --------------------------------------------------------------

func TestUsageByUser_OwnerTypeDerivation(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage/by-user?since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data       []ownerUsageDTO `json:"data"`
		Pagination *Pagination     `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 owners (alice + svc), body=%s", len(body.Data), rec.Body.String())
	}

	var sawUser, sawService bool
	for _, row := range body.Data {
		switch row.OwnerType {
		case "user":
			sawUser = true
			if row.UserID == nil || *row.UserID != fx.userID {
				t.Errorf("user row user_id = %v, want %d", row.UserID, fx.userID)
			}
		case "service":
			sawService = true
			if row.AccountID == nil || *row.AccountID != fx.serviceAcct {
				t.Errorf("service row account_id = %v, want %d", row.AccountID, fx.serviceAcct)
			}
			if row.UserID != nil {
				t.Errorf("service row should have nil user_id, got %v", row.UserID)
			}
		default:
			t.Errorf("unexpected owner_type %q", row.OwnerType)
		}
	}
	if !sawUser || !sawService {
		t.Errorf("expected both user and service rows; sawUser=%v sawService=%v", sawUser, sawService)
	}
}

// --- timeseries -----------------------------------------------------------
//
// Timeseries is locked to the **detail envelope** shape per PLAN.md §Locked
// Decision #20: `{"data": {"interval": "hour"|"day", "buckets": [...]}}`.
// List-envelope (`{data: [...], pagination}`) would lie about pagination.total
// and force the FE to re-key on bucket count. All tests below assert the
// object-wrapped detail shape; body.Data is an object, not an array.

type timeseriesBody struct {
	Data struct {
		Interval string                `json:"interval"`
		Buckets  []timeseriesBucketDTO `json:"buckets"`
	} `json:"data"`
	Pagination *Pagination `json:"pagination"`
}

func TestUsageTimeseries_GapFillMiddleHole(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	// Fixture has rows at t0, t0+1h, t0+2h, t0+5h, t0+6h.
	// Asking for [t0, t0+7h) hourly must produce 7 buckets with
	// non-zero values at indexes 0,1,2,5,6 and zeros at 3,4.
	since := fx.t0
	until := fx.t0.Add(7 * time.Hour)
	path := fmt.Sprintf("/api/admin/usage/timeseries?interval=hour&since=%s&until=%s",
		since.Format(time.RFC3339), until.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body timeseriesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Pagination != nil {
		t.Errorf("timeseries must not include pagination, got %+v", body.Pagination)
	}
	if body.Data.Interval != "hour" {
		t.Errorf("interval = %q, want %q", body.Data.Interval, "hour")
	}
	if len(body.Data.Buckets) != 7 {
		t.Fatalf("len(buckets) = %d, want 7 hourly buckets", len(body.Data.Buckets))
	}

	wantRequests := []int{1, 1, 1, 0, 0, 1, 1}
	for i, want := range wantRequests {
		if body.Data.Buckets[i].Requests != want {
			t.Errorf("bucket[%d].requests = %d, want %d (bucket=%v)",
				i, body.Data.Buckets[i].Requests, want, body.Data.Buckets[i].Bucket)
		}
	}
}

func TestUsageTimeseries_DefaultIntervalHourlyForShortWindow(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	// A 2-hour window should default to interval=hour (<=48h).
	since := fx.t0
	until := fx.t0.Add(2 * time.Hour)
	path := fmt.Sprintf("/api/admin/usage/timeseries?since=%s&until=%s",
		since.Format(time.RFC3339), until.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body timeseriesBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Data.Interval != "hour" {
		t.Errorf("interval = %q, want %q for <=48h window", body.Data.Interval, "hour")
	}
	if len(body.Data.Buckets) != 2 {
		t.Errorf("len(buckets) = %d, want 2 (hourly buckets over 2h)", len(body.Data.Buckets))
	}
}

// Ranges wider than the 48h cutoff must default to interval="day". PLAN.md §20
// fixes the cutoff so a dashboard asking for a week of data doesn't quietly
// return 168 hourly buckets.
func TestUsageTimeseries_DefaultIntervalDailyForLongWindow(t *testing.T) {
	h, s := setupAdminTest(t)
	seedUsageFixtureHTTP(t, s)

	// Use day-aligned boundaries so the bucket count is deterministic. The
	// gap-fill loop truncates `since` down to the interval boundary, so a
	// since not on 00:00 UTC yields one extra bucket. Aligning here keeps
	// the assertion tight.
	dayStart := time.Now().UTC().Truncate(24 * time.Hour)
	since := dayStart.Add(-5 * 24 * time.Hour)
	until := dayStart.Add(2 * 24 * time.Hour) // 7 whole days, >48h
	path := fmt.Sprintf("/api/admin/usage/timeseries?since=%s&until=%s",
		since.Format(time.RFC3339), until.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body timeseriesBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Data.Interval != "day" {
		t.Errorf("interval = %q, want %q for >48h window", body.Data.Interval, "day")
	}
	if len(body.Data.Buckets) != 7 {
		t.Errorf("len(buckets) = %d, want 7 daily buckets", len(body.Data.Buckets))
	}
}

// Empty range (window with zero rows) must still return a dense series of
// zero-valued buckets — not an empty array. PLAN.md §20 calls this out because
// the FE chart renders axis ticks from bucket timestamps; an empty array would
// leave it with no x-axis at all.
func TestUsageTimeseries_EmptyRangeReturnsZeroBuckets(t *testing.T) {
	h, s := setupAdminTest(t)
	seedUsageFixtureHTTP(t, s)

	// Pick a 3h window far from any fixture row so every bucket is empty.
	farFuture := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Hour)
	since := farFuture
	until := farFuture.Add(3 * time.Hour)
	path := fmt.Sprintf("/api/admin/usage/timeseries?interval=hour&since=%s&until=%s",
		since.Format(time.RFC3339), until.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body timeseriesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Interval != "hour" {
		t.Errorf("interval = %q, want %q", body.Data.Interval, "hour")
	}
	if len(body.Data.Buckets) != 3 {
		t.Fatalf("len(buckets) = %d, want 3 zero-valued buckets (not an empty array)", len(body.Data.Buckets))
	}
	for i, b := range body.Data.Buckets {
		if b.Requests != 0 || b.TotalTokens != 0 || b.Credits != 0 || b.Errors != 0 {
			t.Errorf("bucket[%d] should be zero-valued, got %+v", i, b)
		}
	}
}

func TestUsageTimeseries_InvalidInterval(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)
	rec := doAdmin(t, h, "/api/admin/usage/timeseries?interval=week&since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- validation -----------------------------------------------------------

func TestUsageEndpoints_ValidationErrors(t *testing.T) {
	h, _ := setupAdminTest(t)

	cases := []struct {
		name, path string
	}{
		{"bad since", "/api/admin/usage/summary?since=not-a-date"},
		{"bad until", "/api/admin/usage/summary?until=xxx"},
		{"since equals until", "/api/admin/usage/summary?since=2026-04-10&until=2026-04-10"},
		{"negative account_id", "/api/admin/usage/summary?account_id=-1"},
		{"zero api_key_id", "/api/admin/usage/by-model?api_key_id=0"},
		{"limit over cap", "/api/admin/usage/by-user?limit=501"},
		{"negative offset", "/api/admin/usage/by-user?offset=-1"},
		{"bad interval", "/api/admin/usage/timeseries?interval=fortnight"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doAdmin(t, h, c.path)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- auth ----------------------------------------------------------------

func TestUsageEndpoints_RequireAuth(t *testing.T) {
	h, _ := setupAdminTest(t)

	for _, path := range []string{
		"/api/admin/usage/summary",
		"/api/admin/usage/by-model",
		"/api/admin/usage/by-user",
		"/api/admin/usage/timeseries",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestUsageEndpoints_InvalidAdminKey(t *testing.T) {
	h, _ := setupAdminTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage/summary", nil)
	req.Header.Set("X-Admin-Key", "wrong-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// --- legacy /api/admin/usage regression ------------------------------------

func TestLegacyUsageEndpoint_StillReturnsBareArray(t *testing.T) {
	// BE 7 made envelope the default on /api/admin/usage. The legacy raw-array
	// shape is still reachable via ?envelope=0 for one deprecation cycle so
	// ad-hoc X-Admin-Key scripts aren't broken mid-cycle.
	h, s := setupAdminTest(t)
	seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage?envelope=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("legacy usage body must be a JSON array, got: %v (body=%s)", err, rec.Body.String())
	}
}

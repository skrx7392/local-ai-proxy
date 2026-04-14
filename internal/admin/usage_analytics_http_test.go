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

	var body struct {
		Data       []timeseriesBucketDTO `json:"data"`
		Pagination *Pagination           `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Pagination != nil {
		t.Errorf("timeseries must not include pagination, got %+v", body.Pagination)
	}
	if len(body.Data) != 7 {
		t.Fatalf("len(data) = %d, want 7 hourly buckets", len(body.Data))
	}

	wantRequests := []int{1, 1, 1, 0, 0, 1, 1}
	for i, want := range wantRequests {
		if body.Data[i].Requests != want {
			t.Errorf("bucket[%d].requests = %d, want %d (bucket=%v)",
				i, body.Data[i].Requests, want, body.Data[i].Bucket)
		}
	}
}

func TestUsageTimeseries_DefaultIntervalRespectsWindow(t *testing.T) {
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
	var body struct {
		Data []timeseriesBucketDTO `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 2 {
		t.Errorf("len(data) = %d, want 2 (hourly buckets over 2h)", len(body.Data))
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
	// BE 2 must not disturb the existing /api/admin/usage shape. BE 4 flips
	// it to enveloped on opt-in; BE 7 makes envelope the default. Until then,
	// the response body must remain a plain JSON array (no "data" wrapper).
	h, s := setupAdminTest(t)
	seedUsageFixtureHTTP(t, s)

	rec := doAdmin(t, h, "/api/admin/usage")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil {
		t.Fatalf("legacy usage body must be a JSON array, got: %v (body=%s)", err, rec.Body.String())
	}
}

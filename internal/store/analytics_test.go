package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// seedAnalyticsFixture creates a small, deterministic dataset for analytics
// tests: two accounts (one personal with a user, one service), three keys,
// two models, 9 usage rows across a 3-day window.
type analyticsFixture struct {
	personalAccountID int64
	serviceAccountID  int64
	userID            int64
	userEmail         string
	userName          string
	keyUser           int64 // user-owned key
	keyUser2          int64 // second user-owned key (personal account)
	keyService        int64 // service-account key
	t0                time.Time
}

func seedAnalyticsFixture(t *testing.T, s *Store) analyticsFixture {
	t.Helper()
	ctx := context.Background()

	personalAccountID, userID, err := s.RegisterUser("alice@example.com", "hash", "Alice")
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	svcAccountID, err := s.CreateAccount("svc-1", "service")
	if err != nil {
		t.Fatalf("CreateAccount service: %v", err)
	}
	if err := s.InitCreditBalance(svcAccountID); err != nil {
		t.Fatalf("InitCreditBalance: %v", err)
	}

	keyUser, err := s.CreateKeyForAccount(userID, personalAccountID, "alice-primary", "hash-user-1", "sk-u1", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccount user: %v", err)
	}
	keyUser2, err := s.CreateKeyForAccount(userID, personalAccountID, "alice-secondary", "hash-user-2", "sk-u2", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccount user 2: %v", err)
	}
	keyService, err := s.CreateKeyForAccountOnly(svcAccountID, "svc-key", "hash-svc-1", "sk-sv1", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly: %v", err)
	}

	t0 := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)

	// Model A rows for user (various times, all success).
	rows := []struct {
		keyID  int64
		model  string
		prompt int
		comp   int
		total  int
		dur    int64
		credit float64
		status string
		offset time.Duration
	}{
		{keyUser, "llama3.1:8b", 100, 50, 150, 200, 0.30, "completed", 0 * time.Hour},
		{keyUser, "llama3.1:8b", 200, 100, 300, 300, 0.60, "completed", 1 * time.Hour},
		{keyUser, "gpt-4o-mini", 50, 25, 75, 150, 0.15, "completed", 2 * time.Hour},
		{keyUser2, "llama3.1:8b", 80, 40, 120, 180, 0.24, "completed", 3 * time.Hour},
		{keyUser2, "gpt-4o-mini", 30, 15, 45, 100, 0.09, "error", 4 * time.Hour},
		{keyService, "llama3.1:8b", 500, 250, 750, 500, 1.50, "completed", 5 * time.Hour},
		{keyService, "llama3.1:8b", 600, 300, 900, 600, 1.80, "completed", 25 * time.Hour},
		{keyService, "gpt-4o-mini", 40, 20, 60, 120, 0.12, "completed", 26 * time.Hour},
		{keyService, "gpt-4o-mini", 20, 10, 30, 80, 0.06, "error", 27 * time.Hour},
	}

	for _, r := range rows {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.keyID, r.model, r.prompt, r.comp, r.total, r.dur, r.status, r.credit, t0.Add(r.offset),
		)
		if err != nil {
			t.Fatalf("insert usage row: %v", err)
		}
	}

	// Refresh planner statistics so EXPLAIN tests observe realistic row
	// counts. Without ANALYZE, fresh PG reports 0 rows per table and picks
	// whichever index happens to come first alphabetically.
	if _, err := s.pool.Exec(ctx, "ANALYZE usage_logs, api_keys, users, accounts"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	return analyticsFixture{
		personalAccountID: personalAccountID,
		serviceAccountID:  svcAccountID,
		userID:            userID,
		userEmail:         "alice@example.com",
		userName:          "Alice",
		keyUser:           keyUser,
		keyUser2:          keyUser2,
		keyService:        keyService,
		t0:                t0,
	}
}

// seedAnalyticsFixtureLarge inflates the seeded dataset so the planner has
// enough statistical signal to prefer the selectivity-appropriate indexes in
// EXPLAIN tests. Called only by the EXPLAIN tests to keep unit-test fixture
// runs fast.
func seedAnalyticsFixtureLarge(t *testing.T, s *Store) analyticsFixture {
	t.Helper()
	fx := seedAnalyticsFixture(t, s)
	ctx := context.Background()

	// Insert 2000 rows with selective model/key distribution so the planner
	// can distinguish between indexes on different columns.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at)
		 SELECT
		   CASE WHEN i % 3 = 0 THEN $1::bigint WHEN i % 3 = 1 THEN $2::bigint ELSE $3::bigint END,
		   'model-' || (i % 20),
		   10, 5, 15, 100, 'completed', 0.01, NOW() - (i * INTERVAL '1 minute')
		 FROM generate_series(1, 2000) AS i`,
		fx.keyUser, fx.keyUser2, fx.keyService,
	); err != nil {
		t.Fatalf("seed extra rows: %v", err)
	}

	if _, err := s.pool.Exec(ctx, "ANALYZE usage_logs, api_keys, users, accounts"); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	return fx
}

func TestGetUsageSummary_Empty(t *testing.T) {
	s := setupTestStore(t)

	summary, err := s.GetUsageSummary(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if summary.Requests != 0 {
		t.Errorf("expected 0 requests, got %d", summary.Requests)
	}
	if summary.TotalTokens != 0 {
		t.Errorf("expected 0 total tokens, got %d", summary.TotalTokens)
	}
	if summary.Credits != 0 {
		t.Errorf("expected 0 credits, got %f", summary.Credits)
	}
	if summary.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", summary.Errors)
	}
}

func TestGetUsageSummary_NoFilter(t *testing.T) {
	s := setupTestStore(t)
	seedAnalyticsFixture(t, s)

	summary, err := s.GetUsageSummary(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if summary.Requests != 9 {
		t.Errorf("expected 9 requests, got %d", summary.Requests)
	}
	wantTotal := 150 + 300 + 75 + 120 + 45 + 750 + 900 + 60 + 30
	if summary.TotalTokens != wantTotal {
		t.Errorf("expected total_tokens=%d, got %d", wantTotal, summary.TotalTokens)
	}
	wantPrompt := 100 + 200 + 50 + 80 + 30 + 500 + 600 + 40 + 20
	if summary.PromptTokens != wantPrompt {
		t.Errorf("expected prompt_tokens=%d, got %d", wantPrompt, summary.PromptTokens)
	}
	if summary.Errors != 2 {
		t.Errorf("expected 2 errors, got %d", summary.Errors)
	}
	// Credits sum: 0.30+0.60+0.15+0.24+0.09+1.50+1.80+0.12+0.06 = 4.86
	wantCredits := 4.86
	if diff := summary.Credits - wantCredits; diff < -0.0001 || diff > 0.0001 {
		t.Errorf("expected credits ~= %f, got %f", wantCredits, summary.Credits)
	}
}

func TestGetUsageSummary_FilterByAccountID(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	summary, err := s.GetUsageSummary(UsageFilter{AccountID: &fx.serviceAccountID})
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if summary.Requests != 4 {
		t.Errorf("expected 4 service requests, got %d", summary.Requests)
	}
	if summary.Errors != 1 {
		t.Errorf("expected 1 error in service rows, got %d", summary.Errors)
	}
}

func TestGetUsageSummary_FilterByAPIKeyID(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	summary, err := s.GetUsageSummary(UsageFilter{APIKeyID: &fx.keyUser})
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if summary.Requests != 3 {
		t.Errorf("expected 3 rows for keyUser, got %d", summary.Requests)
	}
}

func TestGetUsageSummary_FilterByUserID(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	summary, err := s.GetUsageSummary(UsageFilter{UserID: &fx.userID})
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if summary.Requests != 5 {
		t.Errorf("expected 5 rows for userID, got %d", summary.Requests)
	}
}

func TestGetUsageSummary_FilterByModel(t *testing.T) {
	s := setupTestStore(t)
	seedAnalyticsFixture(t, s)

	model := "gpt-4o-mini"
	summary, err := s.GetUsageSummary(UsageFilter{Model: &model})
	if err != nil {
		t.Fatalf("GetUsageSummary: %v", err)
	}
	if summary.Requests != 4 {
		t.Errorf("expected 4 rows for gpt-4o-mini, got %d", summary.Requests)
	}
}

func TestGetUsageSummary_FilterByTimeRange(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	// Since covers rows at offsets 25, 26, 27 only (fx.t0 + 24h).
	since := fx.t0.Add(24 * time.Hour)
	summary, err := s.GetUsageSummary(UsageFilter{Since: &since})
	if err != nil {
		t.Fatalf("GetUsageSummary with since: %v", err)
	}
	if summary.Requests != 3 {
		t.Errorf("expected 3 rows since t0+24h, got %d", summary.Requests)
	}

	// Until excludes later half (offsets 25, 26, 27).
	until := fx.t0.Add(24 * time.Hour)
	summary, err = s.GetUsageSummary(UsageFilter{Until: &until})
	if err != nil {
		t.Fatalf("GetUsageSummary with until: %v", err)
	}
	if summary.Requests != 6 {
		t.Errorf("expected 6 rows until t0+24h, got %d", summary.Requests)
	}
}

func TestGetUsageByModel_OrderedByTokensDesc(t *testing.T) {
	s := setupTestStore(t)
	seedAnalyticsFixture(t, s)

	rows, err := s.GetUsageByModel(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageByModel: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 model rows, got %d", len(rows))
	}
	// llama3.1:8b total = 150+300+120+750+900 = 2220
	// gpt-4o-mini total = 75+45+60+30 = 210
	if rows[0].Model != "llama3.1:8b" {
		t.Errorf("expected first model 'llama3.1:8b', got %q", rows[0].Model)
	}
	if rows[0].TotalTokens != 2220 {
		t.Errorf("expected total_tokens=2220, got %d", rows[0].TotalTokens)
	}
	if rows[1].Model != "gpt-4o-mini" {
		t.Errorf("expected second model 'gpt-4o-mini', got %q", rows[1].Model)
	}
	if rows[1].TotalTokens != 210 {
		t.Errorf("expected total_tokens=210, got %d", rows[1].TotalTokens)
	}
}

func TestGetUsageByModel_FilterByAccountID(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	rows, err := s.GetUsageByModel(UsageFilter{AccountID: &fx.serviceAccountID})
	if err != nil {
		t.Fatalf("GetUsageByModel: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// service account only: llama = 750+900 = 1650, gpt = 60+30 = 90
	if rows[0].Model != "llama3.1:8b" || rows[0].TotalTokens != 1650 {
		t.Errorf("unexpected first row: %+v", rows[0])
	}
}

func TestGetUsageByUser_ReturnsUserAndServiceRows(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	rows, err := s.GetUsageByUser(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageByUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 owner rows, got %d: %+v", len(rows), rows)
	}

	var userRow, svcRow *OwnerUsageRow
	for i := range rows {
		r := rows[i]
		if r.UserID != nil {
			userRow = &rows[i]
		} else if r.AccountID != nil {
			svcRow = &rows[i]
		}
	}
	if userRow == nil {
		t.Fatal("expected a user-owned row")
	}
	if svcRow == nil {
		t.Fatal("expected a service-account row")
	}
	if userRow.Email == nil || *userRow.Email != fx.userEmail {
		t.Errorf("expected email %q, got %v", fx.userEmail, userRow.Email)
	}
	// user has 5 rows across both keys totaling 150+300+75+120+45 = 690
	if userRow.Requests != 5 {
		t.Errorf("expected 5 user requests, got %d", userRow.Requests)
	}
	if userRow.TotalTokens != 690 {
		t.Errorf("expected user total_tokens=690, got %d", userRow.TotalTokens)
	}
	// Expect 2 distinct keys for user
	if userRow.KeyCount != 2 {
		t.Errorf("expected user key_count=2, got %d", userRow.KeyCount)
	}
	// service account: 4 rows, total = 750+900+60+30 = 1740, 1 distinct key
	if svcRow.Requests != 4 {
		t.Errorf("expected 4 service requests, got %d", svcRow.Requests)
	}
	if svcRow.TotalTokens != 1740 {
		t.Errorf("expected service total_tokens=1740, got %d", svcRow.TotalTokens)
	}
	if svcRow.KeyCount != 1 {
		t.Errorf("expected service key_count=1, got %d", svcRow.KeyCount)
	}
}

func TestGetUsageByUser_OrderedByTokensDesc(t *testing.T) {
	s := setupTestStore(t)
	seedAnalyticsFixture(t, s)

	rows, err := s.GetUsageByUser(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageByUser: %v", err)
	}
	// service account sum (1740) > user sum (690)
	if rows[0].TotalTokens < rows[len(rows)-1].TotalTokens {
		t.Errorf("expected rows ordered by total_tokens desc, got: %+v", rows)
	}
}

func TestGetUsageByUser_UnattributedKey(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create an admin-created key with no user_id and no account_id.
	keyID, err := s.CreateKey("admin-only-key", "hash-admin-only", "sk-adm", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged)
		 VALUES ($1, 'llama3.1:8b', 10, 5, 15, 50, 'completed', 0)`, keyID,
	)
	if err != nil {
		t.Fatalf("insert usage row: %v", err)
	}

	rows, err := s.GetUsageByUser(UsageFilter{})
	if err != nil {
		t.Fatalf("GetUsageByUser: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].UserID != nil || rows[0].AccountID != nil {
		t.Errorf("expected both user_id and account_id nil, got %+v", rows[0])
	}
}

func TestGetUsageTimeseries_HourBuckets(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	buckets, err := s.GetUsageTimeseries(UsageFilter{}, "hour")
	if err != nil {
		t.Fatalf("GetUsageTimeseries: %v", err)
	}
	// 9 distinct hours (0..5, 25..27) → 9 buckets
	if len(buckets) != 9 {
		t.Errorf("expected 9 hour buckets, got %d: %+v", len(buckets), buckets)
	}
	// Ordered ascending by bucket
	for i := 1; i < len(buckets); i++ {
		if !buckets[i].Bucket.After(buckets[i-1].Bucket) {
			t.Errorf("buckets not ordered ascending at i=%d", i)
		}
	}
	// Each hour has exactly one row here, so requests=1 per bucket.
	for _, b := range buckets {
		if b.Requests != 1 {
			t.Errorf("expected 1 request per bucket, got %d at %v", b.Requests, b.Bucket)
		}
	}
	// First bucket is at t0 (UTC-truncated). Credits 0.30.
	if diff := buckets[0].Credits - 0.30; diff < -0.0001 || diff > 0.0001 {
		t.Errorf("expected first bucket credits=0.30, got %f", buckets[0].Credits)
	}
	_ = fx
}

func TestGetUsageTimeseries_DayBuckets(t *testing.T) {
	s := setupTestStore(t)
	seedAnalyticsFixture(t, s)

	buckets, err := s.GetUsageTimeseries(UsageFilter{}, "day")
	if err != nil {
		t.Fatalf("GetUsageTimeseries: %v", err)
	}
	// Rows span two UTC days (t0-t0+5h, t0+25h-t0+27h), which may collapse
	// into 1 or 2 day buckets depending on t0 alignment. Require 1 or 2.
	if len(buckets) < 1 || len(buckets) > 2 {
		t.Errorf("expected 1-2 day buckets, got %d", len(buckets))
	}
}

func TestGetUsageTimeseries_InvalidInterval(t *testing.T) {
	s := setupTestStore(t)

	_, err := s.GetUsageTimeseries(UsageFilter{}, "minute")
	if err == nil {
		t.Error("expected error for invalid interval, got nil")
	}
}

func TestGetUsageTimeseries_WithFilters(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixture(t, s)

	buckets, err := s.GetUsageTimeseries(UsageFilter{AccountID: &fx.serviceAccountID}, "hour")
	if err != nil {
		t.Fatalf("GetUsageTimeseries: %v", err)
	}
	total := 0
	for _, b := range buckets {
		total += b.Requests
	}
	if total != 4 {
		t.Errorf("expected 4 service requests across buckets, got %d", total)
	}
}

// --- EXPLAIN assertions ---

func TestExplain_ByModel_UsesModelIndex(t *testing.T) {
	s := setupTestStore(t)
	seedAnalyticsFixtureLarge(t, s)
	model := "model-1" // one of the 20 distinct models; 100 rows of 2000
	if !planUsesIndex(t, s,
		`SELECT ul.model, COUNT(*) FROM usage_logs ul JOIN api_keys k ON ul.api_key_id = k.id
		 WHERE ul.model = $1 GROUP BY ul.model`,
		[]any{model},
		"idx_usage_logs_model_created",
	) {
		t.Error("expected plan to use idx_usage_logs_model_created for by-model with model filter")
	}
}

func TestExplain_Summary_WithAccountFilter_UsesAccountIndex(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixtureLarge(t, s)
	if !planUsesIndex(t, s,
		`SELECT COUNT(*) FROM usage_logs ul JOIN api_keys k ON ul.api_key_id = k.id
		 WHERE k.account_id = $1`,
		[]any{fx.serviceAccountID},
		"idx_api_keys_account_id",
	) {
		t.Error("expected plan to use idx_api_keys_account_id for summary with account_id")
	}
}

func TestExplain_Summary_WithAPIKeyFilter_UsesKeyIndex(t *testing.T) {
	s := setupTestStore(t)
	fx := seedAnalyticsFixtureLarge(t, s)
	// Either the single-column api_key_id index or the composite
	// (api_key_id, created_at) index is acceptable — both satisfy the
	// contract "don't seq-scan usage_logs to find rows for a single key".
	// The planner picks whichever is cheaper for the current statistics.
	query := `SELECT COUNT(*) FROM usage_logs ul JOIN api_keys k ON ul.api_key_id = k.id
	          WHERE ul.api_key_id = $1`
	args := []any{fx.keyUser}
	if !(planUsesIndex(t, s, query, args, "idx_usage_logs_key_id") ||
		planUsesIndex(t, s, query, args, "idx_usage_logs_key_created")) {
		t.Error("expected plan to use an api_key_id-based index for summary with api_key_id")
	}
}

// planUsesIndex runs EXPLAIN (FORMAT JSON) for the given query and searches
// the resulting plan tree for the named index. Returns true if the index is
// referenced anywhere in the plan. The planner session state (enable_seqscan
// off) and the EXPLAIN itself must run on the same backend, so acquire a
// dedicated connection for the whole check.
func planUsesIndex(t *testing.T, s *Store, query string, args []any, indexName string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Disable seqscan on this connection so the planner surfaces indexed
	// access paths even on tiny test tables where a seq scan would cost
	// less in the abstract.
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}

	explainSQL := "EXPLAIN (FORMAT JSON) " + query
	var raw []byte
	if err := conn.QueryRow(ctx, explainSQL, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	// Reset so the connection isn't poisoned for the next user of the pool.
	_, _ = conn.Exec(ctx, "RESET enable_seqscan")

	var plans []map[string]any
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatalf("parse EXPLAIN output: %v", err)
	}
	for _, p := range plans {
		if planContainsIndex(p["Plan"], indexName) {
			return true
		}
	}
	return false
}

func planContainsIndex(node any, indexName string) bool {
	m, ok := node.(map[string]any)
	if !ok {
		return false
	}
	if s, ok := m["Index Name"].(string); ok && s == indexName {
		return true
	}
	if children, ok := m["Plans"].([]any); ok {
		for _, c := range children {
			if planContainsIndex(c, indexName) {
				return true
			}
		}
	}
	return false
}

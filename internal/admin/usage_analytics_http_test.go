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
//   - 2 nodes; usage rows split between nodeA, nodeB, and NULL node_id
//   - usage rows at known timestamps so tests can pick windows that
//     deterministically select subsets.
//
// Returns key IDs, node IDs, and the anchor time `t0`.
type usageFixture struct {
	personalAcct, serviceAcct int64
	userID                    int64
	keyUser, keyService       int64
	nodeA, nodeB              int64
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
	// Registration links user → personal account; CreateUser alone doesn't.
	// Mirror that here so account→user lookups behave like production.
	if _, err := s.Pool().Exec(ctx, `UPDATE users SET account_id = $1 WHERE id = $2`, personal, uid); err != nil {
		t.Fatalf("link user to personal account: %v", err)
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

	nodeA, err := s.CreateNode(store.Node{Name: "usage-http-a", BaseURL: "http://usage-http-a:11434"})
	if err != nil {
		t.Fatalf("CreateNode a: %v", err)
	}
	nodeB, err := s.CreateNode(store.Node{Name: "usage-http-b", BaseURL: "http://usage-http-b:11434"})
	if err != nil {
		t.Fatalf("CreateNode b: %v", err)
	}

	// --- usage rows at fixed timestamps so filter windows are deterministic.
	// t0 chosen well in the past so a default (now-7d, now) window includes
	// them. We insert spanning 10 hours starting at t0. Node attribution:
	// nodeA serves rows 0,2,4; nodeB row 1; row 3 predates routing (NULL).
	t0 := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)

	type row struct {
		keyID        int64
		model        string
		prompt, comp int
		dur          int64
		credit       float64
		status       string
		offset       time.Duration
		nodeID       *int64
	}
	rows := []row{
		{keyUser, "llama3.1:8b", 100, 50, 200, 0.30, "completed", 0, &nodeA},
		{keyUser, "llama3.1:8b", 200, 100, 300, 0.60, "completed", 1 * time.Hour, &nodeB},
		{keyUser, "gpt-4o-mini", 50, 25, 150, 0.15, "completed", 2 * time.Hour, &nodeA},
		{keyService, "llama3.1:8b", 500, 250, 500, 1.50, "completed", 5 * time.Hour, nil},
		{keyService, "gpt-4o-mini", 20, 10, 80, 0.06, "error", 6 * time.Hour, &nodeA},
	}
	for _, r := range rows {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at, node_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			r.keyID, r.model, r.prompt, r.comp, r.prompt+r.comp, r.dur, r.status, r.credit, t0.Add(r.offset), r.nodeID,
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
		nodeA:        nodeA,
		nodeB:        nodeB,
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

func TestUsageSummary_NodeFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	path := fmt.Sprintf("/api/admin/usage/summary?node_id=%d&since=%s", fx.nodeA, fx.t0.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data usageSummaryDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// nodeA rows: 150 + 75 + 30 tokens, one error, credits 0.30+0.15+0.06.
	// The nodeB row and the NULL-node row must be excluded.
	if body.Data.Requests != 3 {
		t.Errorf("requests = %d, want 3 (nodeA rows only)", body.Data.Requests)
	}
	if body.Data.TotalTokens != 255 {
		t.Errorf("total_tokens = %d, want 255", body.Data.TotalTokens)
	}
	if body.Data.Errors != 1 {
		t.Errorf("errors = %d, want 1", body.Data.Errors)
	}
	if body.Data.Credits < 0.50 || body.Data.Credits > 0.52 {
		t.Errorf("credits = %f, want ~0.51", body.Data.Credits)
	}
}

func TestUsageSummary_NodeFilter_UnknownNodeReturnsZeros(t *testing.T) {
	// A node_id that matches no rows is not an error — the FE per-node usage
	// link must render an empty dashboard, not a failure state.
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	path := fmt.Sprintf("/api/admin/usage/summary?node_id=999999&since=%s", fx.t0.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data usageSummaryDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Requests != 0 || body.Data.TotalTokens != 0 {
		t.Errorf("expected zero summary for unknown node, got %+v", body.Data)
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

func TestUsageByModel_NodeFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	path := fmt.Sprintf("/api/admin/usage/by-model?node_id=%d&since=%s", fx.nodeA, fx.t0.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []modelUsageDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 models on nodeA, body=%s", len(body.Data), rec.Body.String())
	}
	// nodeA: llama = 150 tokens (1 request; the nodeB llama row and the
	// NULL-node llama row are excluded), gpt = 75+30 = 105 tokens (2 requests).
	if body.Data[0].Model != "llama3.1:8b" || body.Data[0].Requests != 1 || body.Data[0].TotalTokens != 150 {
		t.Errorf("unexpected first row: %+v", body.Data[0])
	}
	if body.Data[1].Model != "gpt-4o-mini" || body.Data[1].Requests != 2 || body.Data[1].TotalTokens != 105 {
		t.Errorf("unexpected second row: %+v", body.Data[1])
	}
}

// seedPerfMetricsUsage adds rows for two extra models on top of the base
// fixture, with hand-computable performance metrics:
//
//	bench:7b (5 rows):
//	  completed  p=100 c=200 dur=1000
//	  completed  p=100 c=400 dur=2000
//	  completed  p=0   c=0   dur=3000  (usage extraction failed — no tokens)
//	  error      p=50  c=0   dur=100
//	  partial    p=80  c=40  dur=500
//	allfail:1b (1 row):
//	  error      p=10  c=0   dur=50
func seedPerfMetricsUsage(t *testing.T, s *store.Store, fx usageFixture) {
	t.Helper()
	ctx := context.Background()
	type row struct {
		model        string
		prompt, comp int
		dur          int64
		status       string
	}
	rows := []row{
		{"bench:7b", 100, 200, 1000, "completed"},
		{"bench:7b", 100, 400, 2000, "completed"},
		{"bench:7b", 0, 0, 3000, "completed"},
		{"bench:7b", 50, 0, 100, "error"},
		{"bench:7b", 80, 40, 500, "partial"},
		{"allfail:1b", 10, 0, 50, "error"},
	}
	for i, r := range rows {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at, node_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			fx.keyUser, r.model, r.prompt, r.comp, r.prompt+r.comp, r.dur, r.status, 0.1, fx.t0.Add(time.Duration(i)*time.Minute), &fx.nodeA,
		); err != nil {
			t.Fatalf("insert perf usage: %v", err)
		}
	}
}

func TestUsageByModel_PerformanceMetrics(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)
	seedPerfMetricsUsage(t, s, fx)

	rec := doAdmin(t, h, "/api/admin/usage/by-model?since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []modelUsageDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byModel := make(map[string]modelUsageDTO, len(body.Data))
	for _, d := range body.Data {
		byModel[d.Model] = d
	}

	bench, ok := byModel["bench:7b"]
	if !ok {
		t.Fatalf("bench:7b missing, body=%s", rec.Body.String())
	}
	if bench.Requests != 5 || bench.TotalTokens != 970 {
		t.Errorf("bench requests/total = %d/%d, want 5/970", bench.Requests, bench.TotalTokens)
	}
	if bench.PromptTokens != 330 || bench.CompletionTokens != 640 {
		t.Errorf("bench prompt/completion = %d/%d, want 330/640", bench.PromptTokens, bench.CompletionTokens)
	}
	// Speed uses only completed rows that actually recorded completion
	// tokens: (200+400) tokens / (1000+2000)ms = 200 tok/s. The zero-token
	// completed row and the error/partial rows must not dilute it.
	if bench.TokPerSec == nil || *bench.TokPerSec < 199.99 || *bench.TokPerSec > 200.01 {
		t.Errorf("bench tok_per_sec = %v, want 200", bench.TokPerSec)
	}
	// Percentiles cover all completed rows (latency is real even when token
	// extraction failed): durations {1000, 2000, 3000}.
	if bench.P50DurationMs == nil || *bench.P50DurationMs != 2000 {
		t.Errorf("bench p50 = %v, want 2000", bench.P50DurationMs)
	}
	if bench.P95DurationMs == nil || *bench.P95DurationMs < 2899.99 || *bench.P95DurationMs > 2900.01 {
		t.Errorf("bench p95 = %v, want 2900", bench.P95DurationMs)
	}
	if bench.ErrorCount != 1 || bench.PartialCount != 1 {
		t.Errorf("bench errors/partials = %d/%d, want 1/1", bench.ErrorCount, bench.PartialCount)
	}

	// A model with no completed rows has no speed or latency percentiles —
	// null, not zero (zero would read as "instant").
	allfail, ok := byModel["allfail:1b"]
	if !ok {
		t.Fatalf("allfail:1b missing, body=%s", rec.Body.String())
	}
	if allfail.TokPerSec != nil || allfail.P50DurationMs != nil || allfail.P95DurationMs != nil {
		t.Errorf("allfail speed/percentiles = %v/%v/%v, want all null",
			allfail.TokPerSec, allfail.P50DurationMs, allfail.P95DurationMs)
	}
	if allfail.ErrorCount != 1 || allfail.Requests != 1 {
		t.Errorf("allfail errors/requests = %d/%d, want 1/1", allfail.ErrorCount, allfail.Requests)
	}

	// Pre-existing fixture model keeps its established aggregates (guard
	// against the new aggregates changing grouping or filtering).
	llama := byModel["llama3.1:8b"]
	if llama.Requests != 3 || llama.TotalTokens != 1200 {
		t.Errorf("llama requests/total = %d/%d, want 3/1200", llama.Requests, llama.TotalTokens)
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

func TestUsageByUser_NodeFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	path := fmt.Sprintf("/api/admin/usage/by-user?node_id=%d&since=%s", fx.nodeA, fx.t0.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []ownerUsageDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2 owners on nodeA, body=%s", len(body.Data), rec.Body.String())
	}
	for _, row := range body.Data {
		switch row.OwnerType {
		case "user":
			// keyUser rows on nodeA: 150 + 75 tokens across 2 requests. The
			// nodeB row (300 tokens) is excluded.
			if row.Requests != 2 || row.TotalTokens != 225 {
				t.Errorf("user row = %+v, want 2 requests / 225 tokens", row)
			}
		case "service":
			// keyService rows on nodeA: only the error row (30 tokens). The
			// NULL-node row (750 tokens) is excluded.
			if row.Requests != 1 || row.TotalTokens != 30 {
				t.Errorf("service row = %+v, want 1 request / 30 tokens", row)
			}
		default:
			t.Errorf("unexpected owner_type %q", row.OwnerType)
		}
	}
}

// --- by-account -----------------------------------------------------------

// seedEndUserUsage extends the base fixture with the EUA population the
// by-user grouping can't represent cleanly:
//   - an end_user account with a federated identity, whose usage rides the
//     SERVICE key (trusted-header attribution: usage_logs.account_id is the
//     billing account, not the key's own account)
//   - a key with no account at all (legacy admin key) → unattributed row.
//
// Returns (endUserAcct, orphanKey).
func seedEndUserUsage(t *testing.T, s *store.Store, fx usageFixture) (int64, int64) {
	t.Helper()
	ctx := context.Background()

	eu, err := s.CreateAccount("openwebui:u-777", "end_user")
	if err != nil {
		t.Fatalf("CreateAccount end_user: %v", err)
	}
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO federated_identities (source, external_id, account_id, email, display_name)
		 VALUES ('openwebui', 'u-777', $1, 'chat-user@example.com', 'Chat User')`, eu); err != nil {
		t.Fatalf("insert federated identity: %v", err)
	}

	orphanKey, err := s.CreateKey("legacy-admin-key", "hashL", "sk-l", 60)
	if err != nil {
		t.Fatalf("CreateKey orphan: %v", err)
	}

	type row struct {
		keyID  int64
		acct   *int64
		tokens int
		credit float64
		offset time.Duration
	}
	rows := []row{
		{fx.keyService, &eu, 2000, 0.80, 3 * time.Hour},
		{fx.keyService, &eu, 1000, 0.40, 4 * time.Hour},
		{orphanKey, nil, 10, 0.01, 7 * time.Hour},
	}
	for _, r := range rows {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, account_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at)
			 VALUES ($1, $2, 'gemma4:e4b', $3, 0, $3, 100, 'completed', $4, $5)`,
			r.keyID, r.acct, r.tokens, r.credit, fx.t0.Add(r.offset),
		); err != nil {
			t.Fatalf("insert eua usage: %v", err)
		}
	}
	return eu, orphanKey
}

func TestUsageByAccount_GroupsByBillingAccount(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)
	eu, _ := seedEndUserUsage(t, s, fx)

	rec := doAdmin(t, h, "/api/admin/usage/by-account?since="+fx.t0.Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data       []accountUsageDTO `json:"data"`
		Pagination *Pagination       `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Pagination == nil {
		t.Fatalf("by-account must use the list envelope, body=%s", rec.Body.String())
	}
	// end_user (3000 tokens) > service (780) > personal (525) > unattributed (10)
	if len(body.Data) != 4 {
		t.Fatalf("len(data) = %d, want 4 accounts, body=%s", len(body.Data), rec.Body.String())
	}

	euRow := body.Data[0]
	if euRow.AccountID == nil || *euRow.AccountID != eu {
		t.Fatalf("first row account_id = %v, want end_user acct %d (ordered by tokens desc)", euRow.AccountID, eu)
	}
	if euRow.AccountType == nil || *euRow.AccountType != "end_user" {
		t.Errorf("end_user row account_type = %v, want end_user", euRow.AccountType)
	}
	if euRow.Email == nil || *euRow.Email != "chat-user@example.com" {
		t.Errorf("end_user row email = %v, want federated identity email", euRow.Email)
	}
	if euRow.Requests != 2 || euRow.TotalTokens != 3000 || euRow.KeyCount != 1 {
		t.Errorf("end_user row = %+v, want 2 requests / 3000 tokens / 1 key", euRow)
	}

	svcRow := body.Data[1]
	if svcRow.AccountID == nil || *svcRow.AccountID != fx.serviceAcct {
		t.Errorf("second row account_id = %v, want service acct %d", svcRow.AccountID, fx.serviceAcct)
	}
	// The service key carried end-user traffic too, but those rows bill the
	// end_user account: the service row must count ONLY unattributed key rows.
	if svcRow.Requests != 2 || svcRow.TotalTokens != 780 {
		t.Errorf("service row = %+v, want 2 requests / 780 tokens", svcRow)
	}
	if svcRow.Email != nil {
		t.Errorf("service row email = %v, want nil", svcRow.Email)
	}

	personalRow := body.Data[2]
	if personalRow.AccountID == nil || *personalRow.AccountID != fx.personalAcct {
		t.Errorf("third row account_id = %v, want personal acct %d", personalRow.AccountID, fx.personalAcct)
	}
	if personalRow.AccountType == nil || *personalRow.AccountType != "personal" {
		t.Errorf("personal row account_type = %v, want personal", personalRow.AccountType)
	}
	// Personal accounts fall back to the owning user's email for display.
	if personalRow.Email == nil || *personalRow.Email != "alice@example.com" {
		t.Errorf("personal row email = %v, want alice@example.com", personalRow.Email)
	}

	orphanRow := body.Data[3]
	if orphanRow.AccountID != nil || orphanRow.AccountName != nil || orphanRow.AccountType != nil || orphanRow.Email != nil {
		t.Errorf("unattributed row should be all-NULL identity, got %+v", orphanRow)
	}
	if orphanRow.Requests != 1 || orphanRow.TotalTokens != 10 {
		t.Errorf("unattributed row = %+v, want 1 request / 10 tokens", orphanRow)
	}
}

func TestUsageByAccount_AccountFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)
	eu, _ := seedEndUserUsage(t, s, fx)

	path := fmt.Sprintf("/api/admin/usage/by-account?account_id=%d&since=%s", eu, fx.t0.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []accountUsageDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("len(data) = %d, want just the filtered end_user account, body=%s", len(body.Data), rec.Body.String())
	}
	if body.Data[0].AccountID == nil || *body.Data[0].AccountID != eu {
		t.Errorf("row account_id = %v, want %d", body.Data[0].AccountID, eu)
	}
	if body.Data[0].TotalTokens != 3000 {
		t.Errorf("row tokens = %d, want 3000", body.Data[0].TotalTokens)
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

func TestUsageTimeseries_NodeFilter(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)

	// nodeA rows sit at t0, t0+2h, t0+6h. Asking for [t0, t0+7h) hourly with
	// node_id=nodeA must produce 7 buckets with requests only at 0, 2, 6 —
	// the nodeB row (t0+1h) and the NULL-node row (t0+5h) become zero buckets.
	since := fx.t0
	until := fx.t0.Add(7 * time.Hour)
	path := fmt.Sprintf("/api/admin/usage/timeseries?interval=hour&node_id=%d&since=%s&until=%s",
		fx.nodeA, since.Format(time.RFC3339), until.Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body timeseriesBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Buckets) != 7 {
		t.Fatalf("len(buckets) = %d, want 7 hourly buckets", len(body.Data.Buckets))
	}
	wantRequests := []int{1, 0, 1, 0, 0, 0, 1}
	for i, want := range wantRequests {
		if body.Data.Buckets[i].Requests != want {
			t.Errorf("bucket[%d].requests = %d, want %d (bucket=%v)",
				i, body.Data.Buckets[i].Requests, want, body.Data.Buckets[i].Bucket)
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

// --- timeseries-by-model ---------------------------------------------------

func TestUsageTimeseriesByModel_SeriesMetricsAndGapFill(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)
	seedPerfMetricsUsage(t, s, fx)

	path := fmt.Sprintf("/api/admin/usage/timeseries-by-model?interval=hour&since=%s&until=%s",
		fx.t0.Format(time.RFC3339), fx.t0.Add(3*time.Hour).Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data       timeseriesByModelResponseDTO `json:"data"`
		Pagination *Pagination                  `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Pagination != nil {
		t.Error("timeseries-by-model is a detail envelope; pagination must be absent")
	}
	if body.Data.Interval != "hour" {
		t.Errorf("interval = %q, want hour", body.Data.Interval)
	}

	// Series ordered by window total_tokens desc:
	// bench 970 > llama 450 (rows at t0, t0+1h) > gpt 75 > allfail 10.
	models := make([]string, len(body.Data.Series))
	for i, s := range body.Data.Series {
		models[i] = s.Model
	}
	want := []string{"bench:7b", "llama3.1:8b", "gpt-4o-mini", "allfail:1b"}
	if len(models) != len(want) {
		t.Fatalf("series models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Fatalf("series models = %v, want %v", models, want)
		}
	}

	// Every series is dense over [since, until): exactly 3 aligned hour buckets.
	for _, series := range body.Data.Series {
		if len(series.Buckets) != 3 {
			t.Fatalf("%s: len(buckets) = %d, want 3", series.Model, len(series.Buckets))
		}
		for i, b := range series.Buckets {
			wantBucket := fx.t0.Add(time.Duration(i) * time.Hour)
			if !b.Bucket.Equal(wantBucket) {
				t.Errorf("%s bucket[%d] = %v, want %v", series.Model, i, b.Bucket, wantBucket)
			}
		}
	}

	bench := body.Data.Series[0]
	b0 := bench.Buckets[0]
	if b0.Requests != 5 || b0.Errors != 1 {
		t.Errorf("bench[0] requests/errors = %d/%d, want 5/1", b0.Requests, b0.Errors)
	}
	if b0.PromptTokens != 330 || b0.CompletionTokens != 640 || b0.TotalTokens != 970 {
		t.Errorf("bench[0] tokens = %d/%d/%d, want 330/640/970",
			b0.PromptTokens, b0.CompletionTokens, b0.TotalTokens)
	}
	if b0.TokPerSec == nil || *b0.TokPerSec < 199.99 || *b0.TokPerSec > 200.01 {
		t.Errorf("bench[0] tok_per_sec = %v, want 200", b0.TokPerSec)
	}
	if b0.P95DurationMs == nil || *b0.P95DurationMs < 2899.99 || *b0.P95DurationMs > 2900.01 {
		t.Errorf("bench[0] p95 = %v, want 2900", b0.P95DurationMs)
	}
	// avg covers all 5 rows: (1000+2000+3000+100+500)/5 = 1320.
	if b0.AvgDurationMs < 1319.99 || b0.AvgDurationMs > 1320.01 {
		t.Errorf("bench[0] avg = %v, want 1320", b0.AvgDurationMs)
	}
	// Gap-filled buckets: zero counts, null speed/percentiles.
	for i := 1; i <= 2; i++ {
		gb := bench.Buckets[i]
		if gb.Requests != 0 || gb.TotalTokens != 0 {
			t.Errorf("bench[%d] not zero-filled: %+v", i, gb)
		}
		if gb.TokPerSec != nil || gb.P95DurationMs != nil {
			t.Errorf("bench[%d] speed/p95 = %v/%v, want null", i, gb.TokPerSec, gb.P95DurationMs)
		}
	}

	// llama has one real row in bucket 0 (150 tok) and one in bucket 1 (300).
	llama := body.Data.Series[1]
	if llama.Buckets[0].TotalTokens != 150 || llama.Buckets[1].TotalTokens != 300 || llama.Buckets[2].TotalTokens != 0 {
		t.Errorf("llama totals = %d/%d/%d, want 150/300/0",
			llama.Buckets[0].TotalTokens, llama.Buckets[1].TotalTokens, llama.Buckets[2].TotalTokens)
	}

	// allfail has no completed rows anywhere: null speed/p95 even in its real bucket.
	allfail := body.Data.Series[3]
	if allfail.Buckets[0].Errors != 1 || allfail.Buckets[0].TokPerSec != nil || allfail.Buckets[0].P95DurationMs != nil {
		t.Errorf("allfail[0] = %+v, want errors=1 and null speed/p95", allfail.Buckets[0])
	}
}

func TestUsageTimeseriesByModel_CapsSeriesAtTopModels(t *testing.T) {
	h, s := setupAdminTest(t)
	fx := seedUsageFixtureHTTP(t, s)
	ctx := context.Background()

	// 14 extra models, one completed row each, descending token weight so the
	// expected cut is deterministic (weight 14000 down to 1000).
	for i := 1; i <= 14; i++ {
		if _, err := s.Pool().Exec(ctx,
			`INSERT INTO usage_logs (api_key_id, model, prompt_tokens, completion_tokens, total_tokens, duration_ms, status, credits_charged, created_at)
			 VALUES ($1, $2, $3, 0, $3, 100, 'completed', 0, $4)`,
			fx.keyUser, fmt.Sprintf("capmodel-%02d", i), i*1000, fx.t0,
		); err != nil {
			t.Fatalf("insert cap row: %v", err)
		}
	}

	path := fmt.Sprintf("/api/admin/usage/timeseries-by-model?interval=hour&since=%s&until=%s",
		fx.t0.Format(time.RFC3339), fx.t0.Add(time.Hour).Format(time.RFC3339))
	rec := doAdmin(t, h, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data timeseriesByModelResponseDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Series) != maxTimeseriesModels {
		t.Fatalf("len(series) = %d, want cap %d", len(body.Data.Series), maxTimeseriesModels)
	}
	// Top of the cut must be the heaviest extra model; fixture models with
	// less window traffic than the cut line must be gone.
	if body.Data.Series[0].Model != "capmodel-14" {
		t.Errorf("series[0] = %q, want capmodel-14", body.Data.Series[0].Model)
	}
	for _, sr := range body.Data.Series {
		if sr.Model == "capmodel-01" {
			t.Error("capmodel-01 (lightest) should have been cut by the cap")
		}
	}
}

func TestUsageTimeseriesByModel_Validation(t *testing.T) {
	// Own handler: keeps the shared validation table under the 10 req/min
	// admin bucket.
	h, _ := setupAdminTest(t)
	for _, path := range []string{
		"/api/admin/usage/timeseries-by-model?interval=fortnight",
		"/api/admin/usage/timeseries-by-model?since=2026-04-10&until=2026-04-10",
		// Oversized gap-fill windows are rejected on BOTH timeseries
		// endpoints, not silently allocated (1970→9999 hourly ≈ 70M buckets).
		"/api/admin/usage/timeseries-by-model?interval=hour&since=1970-01-01&until=9999-01-01",
		"/api/admin/usage/timeseries?interval=hour&since=1970-01-01&until=9999-01-01",
		"/api/admin/usage/timeseries?interval=day&since=1970-01-01&until=9999-01-01",
	} {
		rec := doAdmin(t, h, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
	// A window right at the cap still works: 83 days hourly = 1992 buckets.
	rec := doAdmin(t, h, "/api/admin/usage/timeseries?interval=hour&since=2026-01-01&until=2026-03-25")
	if rec.Code != http.StatusOK {
		t.Errorf("at-cap window: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
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
		{"by-account limit over cap", "/api/admin/usage/by-account?limit=501"},
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

// Own handler (and therefore own X-Admin-Key rate-limit bucket): folding these
// into TestUsageEndpoints_ValidationErrors would push that test past the
// 10 req/min admin bucket and turn expected 400s into 429s.
func TestUsageEndpoints_InvalidNodeID(t *testing.T) {
	h, _ := setupAdminTest(t)

	for _, path := range []string{
		"/api/admin/usage/summary?node_id=abc",
		"/api/admin/usage/by-model?node_id=abc",
		"/api/admin/usage/by-user?node_id=abc",
		"/api/admin/usage/by-account?node_id=abc",
		"/api/admin/usage/timeseries?node_id=abc",
		"/api/admin/usage/timeseries-by-model?node_id=abc",
	} {
		t.Run(path, func(t *testing.T) {
			rec := doAdmin(t, h, path)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUsageEndpoints_InvalidNodeIDErrorShape(t *testing.T) {
	// The 400 body must be byte-compatible with the legacy /api/admin/usage
	// endpoint's node_id error (PR #49): code=invalid_node_id,
	// type=invalid_request_error, message="Invalid node_id parameter".
	h, _ := setupAdminTest(t)

	rec := doAdmin(t, h, "/api/admin/usage/summary?node_id=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "invalid_node_id" {
		t.Errorf("error.code = %q, want invalid_node_id", body.Error.Code)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", body.Error.Type)
	}
	if body.Error.Message != "Invalid node_id parameter" {
		t.Errorf("error.message = %q, want %q", body.Error.Message, "Invalid node_id parameter")
	}
}

// --- auth ----------------------------------------------------------------

func TestUsageEndpoints_RequireAuth(t *testing.T) {
	h, _ := setupAdminTest(t)

	for _, path := range []string{
		"/api/admin/usage/summary",
		"/api/admin/usage/by-model",
		"/api/admin/usage/by-user",
		"/api/admin/usage/by-account",
		"/api/admin/usage/timeseries",
		"/api/admin/usage/timeseries-by-model",
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

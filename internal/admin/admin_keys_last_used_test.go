package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAdmin_ListKeys_IncludesLastUsedAt verifies GET /api/admin/keys surfaces
// last_used_at derived from usage_logs: a key with usage carries an RFC3339
// timestamp, an unused key carries JSON null.
func TestAdmin_ListKeys_IncludesLastUsedAt(t *testing.T) {
	h, s := setupAdminTest(t)
	ctx := context.Background()

	acctID, err := s.CreateAccount("svc-http-lastused", "service")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	usedKey, err := s.CreateKeyForAccountOnly(acctID, "used-key", "hash-http-used", "sk-huse", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly used: %v", err)
	}
	unusedKey, err := s.CreateKeyForAccountOnly(acctID, "unused-key", "hash-http-unused", "sk-hunu", 60)
	if err != nil {
		t.Fatalf("CreateKeyForAccountOnly unused: %v", err)
	}

	ts := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Second)
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO usage_logs (api_key_id, model, total_tokens, status, created_at)
		 VALUES ($1, 'llama3.1:8b', 12, 'completed', $2)`, usedKey, ts,
	); err != nil {
		t.Fatalf("insert usage row: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET /api/admin/keys defaults to the enveloped shape (envelope="" → true).
	var body struct {
		Data []keyResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	byID := make(map[int64]keyResponse, len(body.Data))
	for _, k := range body.Data {
		byID[k.ID] = k
	}

	used, ok := byID[usedKey]
	if !ok {
		t.Fatalf("used key %d missing from response", usedKey)
	}
	if used.LastUsedAt == nil {
		t.Fatal("expected last_used_at for a key with usage, got null")
	}
	parsed, err := time.Parse(time.RFC3339, *used.LastUsedAt)
	if err != nil {
		t.Fatalf("last_used_at is not RFC3339: %q (%v)", *used.LastUsedAt, err)
	}
	if !parsed.Equal(ts) {
		t.Errorf("expected last_used_at %v, got %v", ts, parsed)
	}

	unused, ok := byID[unusedKey]
	if !ok {
		t.Fatalf("unused key %d missing from response", unusedKey)
	}
	if unused.LastUsedAt != nil {
		t.Errorf("expected null last_used_at for unused key, got %q", *unused.LastUsedAt)
	}
}

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/health"
	"github.com/krishna/local-ai-proxy/internal/poller"
	"github.com/krishna/local-ai-proxy/internal/registry"
	"github.com/krishna/local-ai-proxy/internal/store"
)

const testNodesFile = "/etc/laip/nodes.json"

// setupNodesTest builds a full admin handler wired to a real registry and
// poller over the test database, mirroring main.go's BE-7 wiring.
func setupNodesTest(t *testing.T) (http.Handler, *store.Store, *registry.Registry, *poller.Poller) {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping admin nodes integration test")
	}

	ctx := context.Background()
	s, err := store.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	wipe := func() {
		c := context.Background()
		// FK-aware order: usage_logs references api_keys and nodes.
		_, _ = s.Pool().Exec(c, "DELETE FROM usage_logs")
		_, _ = s.Pool().Exec(c, "DELETE FROM nodes")
		_, _ = s.Pool().Exec(c, "DELETE FROM api_keys")
		_, _ = s.Pool().Exec(c, "DELETE FROM users")
		_, _ = s.Pool().Exec(c, "DELETE FROM federated_identities")
		_, _ = s.Pool().Exec(c, "DELETE FROM accounts")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})

	reg := registry.New()
	p := poller.New(s, reg, nil, poller.Options{ProbeTimeout: 2 * time.Second})
	usageCh := make(chan store.UsageEntry, 100)
	h := NewHandler(s, testAdminKey, usageCh, Options{
		Snapshot:  ConfigSnapshot{NodesFile: testNodesFile},
		Checker:   health.NewChecker(nil, reg, nil, 0),
		Registry:  reg,
		Refresher: p,
	})
	return h, s, reg, p
}

// nodesReq performs an authenticated admin request and returns the recorder.
func nodesReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// nodeBackend is a controllable fake ollama backend.
type nodeBackend struct {
	srv  *httptest.Server
	fail atomic.Bool
}

func newNodeBackend(t *testing.T, models ...string) *nodeBackend {
	t.Helper()
	b := &nodeBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		entries := make([]string, len(models))
		for i, m := range models {
			entries[i] = fmt.Sprintf(`{"name":%q}`, m)
		}
		fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(entries, ","))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// assertNoRawSecret fails if any raw secret leaked into an admin response.
func assertNoRawSecret(t *testing.T, body string, rawSecrets ...string) {
	t.Helper()
	for _, s := range rawSecrets {
		if strings.Contains(body, s) {
			t.Errorf("response leaked raw secret %q: %s", s, body)
		}
	}
}

func decodeNodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) nodeDTO {
	t.Helper()
	var env struct {
		Data nodeDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode node envelope: %v (body: %s)", err, rec.Body.String())
	}
	return env.Data
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errObj, _ := resp["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func errMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	errObj, _ := resp["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	return msg
}

// --- POST /api/admin/nodes -------------------------------------------------

func TestAdminNodes_Create_ProbesAndRoutesBeforeResponding(t *testing.T) {
	h, _, reg, _ := setupNodesTest(t)
	backend := newNodeBackend(t, "llama3:8b", "qwen3:32b")
	const rawSecret = "Bearer sk-raw-node-secret-12345"

	body := fmt.Sprintf(`{"name":"mac-studio","base_url":"%s/","backend_type":"ollama","auth_header":%q}`,
		backend.srv.URL, rawSecret)
	rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	assertNoRawSecret(t, rec.Body.String(), rawSecret)

	n := decodeNodeEnvelope(t, rec)
	if n.ID <= 0 {
		t.Errorf("expected positive id, got %d", n.ID)
	}
	if n.Name != "mac-studio" {
		t.Errorf("name = %q", n.Name)
	}
	// Canonicalized: trailing slash trimmed.
	if n.BaseURL != backend.srv.URL {
		t.Errorf("base_url = %q, want canonical %q", n.BaseURL, backend.srv.URL)
	}
	if n.Source != "api" || !n.Enabled {
		t.Errorf("source/enabled = %q/%v, want api/true", n.Source, n.Enabled)
	}
	if n.AuthHeader == nil || !strings.Contains(*n.AuthHeader, "…") {
		t.Errorf("auth_header = %v, want masked value", n.AuthHeader)
	}
	// Live state from the synchronous initial probe.
	if n.Health != "healthy" {
		t.Errorf("health = %q, want healthy", n.Health)
	}
	if len(n.Models) != 2 || n.Models[0] != "llama3:8b" {
		t.Errorf("models = %v, want discovered list", n.Models)
	}
	if n.LastCheckedAt == nil {
		t.Error("last_checked_at = null, want probe timestamp")
	}
	if n.LastError != "" {
		t.Errorf("last_error = %q, want empty", n.LastError)
	}
	// The node must already be routable when the response returns.
	if _, err := reg.Resolve("llama3:8b"); err != nil {
		t.Errorf("model not routable after create: %v", err)
	}
}

func TestAdminNodes_Create_ValidationErrors(t *testing.T) {
	h, _, _, _ := setupNodesTest(t)

	cases := []struct {
		name, body, wantIn string
	}{
		{"bad scheme", `{"name":"n","base_url":"ftp://host/x"}`, "scheme"},
		{"v1 suffix", `{"name":"n","base_url":"http://host:11434/v1"}`, "/v1"},
		{"bad backend_type", `{"name":"n","base_url":"http://host:11434","backend_type":"vllm"}`, "backend_type"},
		{"missing name", `{"base_url":"http://host:11434"}`, "name"},
		{"bad auth header", `{"name":"n","base_url":"http://host:11434","auth_header":"x\r\ny"}`, "auth_header"},
		{"bad health path", `{"name":"n","base_url":"http://host:11434","health_path":"http://evil"}`, "health_path"},
		{"bad timeout", `{"name":"n","base_url":"http://host:11434","timeout_seconds":-5}`, "timeout_seconds"},
	}
	for _, c := range cases {
		rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		if msg := errMessage(t, rec); !strings.Contains(msg, c.wantIn) {
			t.Errorf("%s: error message %q should mention %q", c.name, msg, c.wantIn)
		}
	}
}

func TestAdminNodes_Create_NameConflict409(t *testing.T) {
	h, _, _, _ := setupNodesTest(t)
	backend := newNodeBackend(t, "m")

	body := fmt.Sprintf(`{"name":"dup","base_url":%q}`, backend.srv.URL)
	if rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "node_name_exists" {
		t.Errorf("error code = %q, want node_name_exists", code)
	}
}

func TestAdminNodes_Create_ProbeFailureStillCreates(t *testing.T) {
	h, _, _, _ := setupNodesTest(t)
	backend := newNodeBackend(t, "m")
	backend.fail.Store(true)

	body := fmt.Sprintf(`{"name":"downer","base_url":%q}`, backend.srv.URL)
	rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 even when the initial probe fails, got %d: %s", rec.Code, rec.Body.String())
	}
	n := decodeNodeEnvelope(t, rec)
	if n.Health != "unhealthy" {
		t.Errorf("health = %q, want unhealthy (decisive initial probe)", n.Health)
	}
	if n.LastError == "" {
		t.Error("last_error empty, want probe failure recorded")
	}
	if n.LastCheckedAt == nil {
		t.Error("last_checked_at = null, want probe timestamp")
	}
}

// --- GET /api/admin/nodes --------------------------------------------------

func TestAdminNodes_List_JoinsLiveState(t *testing.T) {
	h, s, _, p := setupNodesTest(t)
	backend := newNodeBackend(t, "llama3:8b")
	const rawSecret = "Bearer sk-list-raw-secret-999"

	probedID, err := s.CreateNode(store.Node{
		Name: "probed", BaseURL: backend.srv.URL, BackendType: "ollama",
		AuthHeader: strPtr(rawSecret),
	})
	if err != nil {
		t.Fatalf("CreateNode probed: %v", err)
	}
	if _, err := s.CreateNode(store.Node{Name: "unprobed", BaseURL: "http://unprobed:11434"}); err != nil {
		t.Fatalf("CreateNode unprobed: %v", err)
	}
	if err := p.RefreshNode(context.Background(), probedID); err != nil {
		t.Fatalf("RefreshNode: %v", err)
	}

	rec := nodesReq(t, h, http.MethodGet, "/api/admin/nodes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertNoRawSecret(t, rec.Body.String(), rawSecret)

	var env struct {
		Data       []nodeDTO   `json:"data"`
		Pagination *Pagination `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list envelope: %v (body: %s)", err, rec.Body.String())
	}
	if env.Pagination == nil || env.Pagination.Total != 2 {
		t.Fatalf("pagination = %+v, want total 2", env.Pagination)
	}
	if len(env.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(env.Data))
	}

	byName := map[string]nodeDTO{}
	for _, n := range env.Data {
		byName[n.Name] = n
	}
	probed := byName["probed"]
	if probed.Health != "healthy" {
		t.Errorf("probed health = %q, want healthy", probed.Health)
	}
	if len(probed.Models) != 1 || probed.Models[0] != "llama3:8b" {
		t.Errorf("probed models = %v, want [llama3:8b]", probed.Models)
	}
	if probed.AuthHeader == nil || !strings.Contains(*probed.AuthHeader, "…") {
		t.Errorf("probed auth_header = %v, want masked", probed.AuthHeader)
	}
	if probed.Source != "api" || !probed.Enabled {
		t.Errorf("probed source/enabled = %q/%v, want api/true", probed.Source, probed.Enabled)
	}

	unprobed := byName["unprobed"]
	if unprobed.Health != "unknown" {
		t.Errorf("unprobed health = %q, want unknown", unprobed.Health)
	}
	if unprobed.Models == nil || len(unprobed.Models) != 0 {
		t.Errorf("unprobed models = %v, want empty list", unprobed.Models)
	}
	if unprobed.LastCheckedAt != nil {
		t.Errorf("unprobed last_checked_at = %v, want null", *unprobed.LastCheckedAt)
	}
}

// --- GET /api/admin/nodes/{id} ----------------------------------------------

func TestAdminNodes_Get(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)

	id, err := s.CreateNode(store.Node{Name: "solo", BaseURL: "http://solo:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	rec := nodesReq(t, h, http.MethodGet, fmt.Sprintf("/api/admin/nodes/%d", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	n := decodeNodeEnvelope(t, rec)
	if n.ID != id || n.Name != "solo" || n.Health != "unknown" {
		t.Errorf("got %+v, want id=%d name=solo health=unknown", n, id)
	}

	if rec := nodesReq(t, h, http.MethodGet, "/api/admin/nodes/999999", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: expected 404, got %d", rec.Code)
	}
}

// --- PUT /api/admin/nodes/{id} ----------------------------------------------

// The PATCH-like auth_header contract: absent = keep, "" = clear,
// value = replace. The masked value must never round-trip into the store.
func TestAdminNodes_Update_AuthHeaderKeepReplaceClear(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)
	const rawOne = "Bearer sk-raw-original-secret"
	const rawTwo = "Bearer sk-raw-replacement-secret"

	id, err := s.CreateNode(store.Node{
		Name: "authy", BaseURL: "http://authy:11434", AuthHeader: strPtr(rawOne),
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	path := fmt.Sprintf("/api/admin/nodes/%d", id)

	// 1. auth_header absent → keep.
	rec := nodesReq(t, h, http.MethodPut, path, `{"name":"authy-renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("keep: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertNoRawSecret(t, rec.Body.String(), rawOne, rawTwo)
	raw, _ := s.GetNodeWithSecrets(id)
	if raw.AuthHeader == nil || *raw.AuthHeader != rawOne {
		t.Fatalf("after omitted auth_header: stored = %v, want original kept", raw.AuthHeader)
	}
	if raw.Name != "authy-renamed" {
		t.Errorf("name = %q, want authy-renamed", raw.Name)
	}

	// 2. auth_header = new value → replace.
	rec = nodesReq(t, h, http.MethodPut, path, fmt.Sprintf(`{"auth_header":%q}`, rawTwo))
	if rec.Code != http.StatusOK {
		t.Fatalf("replace: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertNoRawSecret(t, rec.Body.String(), rawOne, rawTwo)
	if n := decodeNodeEnvelope(t, rec); n.AuthHeader == nil || !strings.Contains(*n.AuthHeader, "…") {
		t.Errorf("response auth_header = %v, want masked", n.AuthHeader)
	}
	raw, _ = s.GetNodeWithSecrets(id)
	if raw.AuthHeader == nil || *raw.AuthHeader != rawTwo {
		t.Fatalf("after replace: stored = %v, want %q", raw.AuthHeader, rawTwo)
	}

	// 3. auth_header = "" → clear.
	rec = nodesReq(t, h, http.MethodPut, path, `{"auth_header":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := decodeNodeEnvelope(t, rec); n.AuthHeader != nil {
		t.Errorf("response auth_header = %v, want null after clear", *n.AuthHeader)
	}
	raw, _ = s.GetNodeWithSecrets(id)
	if raw.AuthHeader != nil {
		t.Fatalf("after clear: stored = %q, want nil", *raw.AuthHeader)
	}
}

func TestAdminNodes_Update_RefreshesLiveState(t *testing.T) {
	h, s, reg, _ := setupNodesTest(t)
	backend := newNodeBackend(t, "qwen3:32b")

	id, err := s.CreateNode(store.Node{Name: "movable", BaseURL: "http://old-target:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	body := fmt.Sprintf(`{"base_url":%q}`, backend.srv.URL)
	rec := nodesReq(t, h, http.MethodPut, fmt.Sprintf("/api/admin/nodes/%d", id), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	n := decodeNodeEnvelope(t, rec)
	if n.BaseURL != backend.srv.URL {
		t.Errorf("base_url = %q, want %q", n.BaseURL, backend.srv.URL)
	}
	if n.Health != "healthy" {
		t.Errorf("health = %q, want healthy from synchronous re-probe", n.Health)
	}
	if len(n.Models) != 1 || n.Models[0] != "qwen3:32b" {
		t.Errorf("models = %v, want [qwen3:32b]", n.Models)
	}
	if _, err := reg.Resolve("qwen3:32b"); err != nil {
		t.Errorf("model not routable after update: %v", err)
	}
}

func TestAdminNodes_Update_ConfigSourced409(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)

	id, err := s.CreateNode(store.Node{Name: "from-file", BaseURL: "http://file:11434", Source: "config"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	rec := nodesReq(t, h, http.MethodPut, fmt.Sprintf("/api/admin/nodes/%d", id), `{"name":"nope"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 for config-sourced node, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := errMessage(t, rec); !strings.Contains(msg, testNodesFile) {
		t.Errorf("error message %q should point at NODES_FILE %q", msg, testNodesFile)
	}
}

func TestAdminNodes_Update_NameConflict409(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)

	if _, err := s.CreateNode(store.Node{Name: "taken", BaseURL: "http://a:11434"}); err != nil {
		t.Fatalf("CreateNode a: %v", err)
	}
	id, err := s.CreateNode(store.Node{Name: "renamer", BaseURL: "http://b:11434"})
	if err != nil {
		t.Fatalf("CreateNode b: %v", err)
	}

	rec := nodesReq(t, h, http.MethodPut, fmt.Sprintf("/api/admin/nodes/%d", id), `{"name":"taken"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on rename collision, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "node_name_exists" {
		t.Errorf("error code = %q, want node_name_exists", code)
	}
}

func TestAdminNodes_Update_Errors(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)

	id, err := s.CreateNode(store.Node{Name: "target", BaseURL: "http://t:11434"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	if rec := nodesReq(t, h, http.MethodPut, "/api/admin/nodes/999999", `{"name":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: expected 404, got %d", rec.Code)
	}
	if rec := nodesReq(t, h, http.MethodPut, "/api/admin/nodes/abc", `{"name":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: expected 400, got %d", rec.Code)
	}
	rec := nodesReq(t, h, http.MethodPut, fmt.Sprintf("/api/admin/nodes/%d", id), `{"base_url":"ftp://nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid base_url: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- DELETE /api/admin/nodes/{id} --------------------------------------------

func TestAdminNodes_Delete_RemovesFromRouting(t *testing.T) {
	h, s, reg, _ := setupNodesTest(t)
	backend := newNodeBackend(t, "llama3:8b")

	body := fmt.Sprintf(`{"name":"goner","base_url":%q}`, backend.srv.URL)
	rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	n := decodeNodeEnvelope(t, rec)
	if _, err := reg.Resolve("llama3:8b"); err != nil {
		t.Fatalf("precondition: model not routable: %v", err)
	}

	rec = nodesReq(t, h, http.MethodDelete, fmt.Sprintf("/api/admin/nodes/%d", n.ID), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Soft-deleted in the store...
	stored, err := s.GetNode(n.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetNode after delete: %v, %v (row must survive; DELETE is a disable)", stored, err)
	}
	if stored.Enabled {
		t.Error("node still enabled after DELETE")
	}
	// ...and synchronously out of routing.
	snap := reg.Snapshot()
	if len(snap.Nodes) != 0 {
		t.Errorf("registry still has %d nodes after DELETE", len(snap.Nodes))
	}
	if _, err := reg.Resolve("llama3:8b"); err == nil {
		t.Error("model still routable after DELETE")
	}
}

func TestAdminNodes_Delete_Errors(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)

	cfgID, err := s.CreateNode(store.Node{Name: "cfg", BaseURL: "http://cfg:11434", Source: "config"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	rec := nodesReq(t, h, http.MethodDelete, fmt.Sprintf("/api/admin/nodes/%d", cfgID), "")
	if rec.Code != http.StatusConflict {
		t.Errorf("config-sourced: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := errMessage(t, rec); !strings.Contains(msg, testNodesFile) {
		t.Errorf("error message %q should point at NODES_FILE %q", msg, testNodesFile)
	}
	if rec := nodesReq(t, h, http.MethodDelete, "/api/admin/nodes/999999", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: expected 404, got %d", rec.Code)
	}
	if rec := nodesReq(t, h, http.MethodDelete, "/api/admin/nodes/abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid id: expected 400, got %d", rec.Code)
	}
}

// --- POST /api/admin/nodes/{id}/refresh ---------------------------------------

func TestAdminNodes_Refresh_ReturnsFreshLiveState(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)
	backend := newNodeBackend(t, "llama3:8b")

	id, err := s.CreateNode(store.Node{Name: "refreshable", BaseURL: backend.srv.URL, BackendType: "ollama"})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	path := fmt.Sprintf("/api/admin/nodes/%d/refresh", id)

	rec := nodesReq(t, h, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	n := decodeNodeEnvelope(t, rec)
	if n.Health != "healthy" || len(n.Models) != 1 {
		t.Errorf("after refresh: health=%q models=%v, want healthy/[llama3:8b]", n.Health, n.Models)
	}

	// Backend goes down; a second forced refresh must report it immediately.
	backend.fail.Store(true)
	rec = nodesReq(t, h, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	n = decodeNodeEnvelope(t, rec)
	if n.Health != "unhealthy" {
		t.Errorf("after failing refresh: health = %q, want unhealthy", n.Health)
	}
	if n.LastError == "" {
		t.Error("last_error empty, want probe failure detail")
	}
}

func TestAdminNodes_Refresh_NotFound(t *testing.T) {
	h, _, _, _ := setupNodesTest(t)

	if rec := nodesReq(t, h, http.MethodPost, "/api/admin/nodes/999999/refresh", ""); rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// --- GET /api/admin/usage?node_id= --------------------------------------------

func TestAdminUsage_NodeIDFilter(t *testing.T) {
	h, s, _, _ := setupNodesTest(t)

	keyID, err := s.CreateKey("node-usage-key", "hash-node-usage", "sk-nodeusg", 60)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	nodeA, err := s.CreateNode(store.Node{Name: "node-a", BaseURL: "http://a:11434"})
	if err != nil {
		t.Fatalf("CreateNode a: %v", err)
	}
	nodeB, err := s.CreateNode(store.Node{Name: "node-b", BaseURL: "http://b:11434"})
	if err != nil {
		t.Fatalf("CreateNode b: %v", err)
	}

	log := func(model string, nodeID *int64) {
		t.Helper()
		if err := s.LogUsage(store.UsageEntry{
			APIKeyID: keyID, Model: model, PromptTokens: 10, CompletionTokens: 5,
			TotalTokens: 15, DurationMs: 50, Status: "completed", NodeID: nodeID,
		}); err != nil {
			t.Fatalf("LogUsage: %v", err)
		}
	}
	log("model-on-a", &nodeA)
	log("model-on-b", &nodeB)
	log("model-unrouted", nil)

	rec := nodesReq(t, h, http.MethodGet, fmt.Sprintf("/api/admin/usage?node_id=%d&envelope=0", nodeA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var stats []store.UsageStat
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if len(stats) != 1 || stats[0].Model != "model-on-a" {
		t.Errorf("stats = %+v, want exactly the node-a row", stats)
	}

	if rec := nodesReq(t, h, http.MethodGet, "/api/admin/usage?node_id=abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid node_id: expected 400, got %d", rec.Code)
	}
}

// --- GET /api/admin/health node breakdown --------------------------------------

func TestAdminHealth_PerNodeBreakdown(t *testing.T) {
	h, _, reg, _ := setupNodesTest(t)

	u1 := mustParseURL(t, "http://one:11434")
	u2 := mustParseURL(t, "http://two:11434")
	reg.SetNodes([]registry.Node{
		{ID: 1, Name: "one", BaseURL: u1},
		{ID: 2, Name: "two", BaseURL: u2},
	})
	checked := time.Now()
	reg.SetNodeProbe(1, registry.ProbeResult{
		Health: registry.HealthHealthy, Models: []string{"m1", "m2"}, LastCheckedAt: checked,
	})
	reg.SetNodeProbe(2, registry.ProbeResult{
		Health: registry.HealthUnhealthy, LastError: "connection refused", LastCheckedAt: checked,
	})

	rec := nodesReq(t, h, http.MethodGet, "/api/admin/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (one healthy node), got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Status string `json:"status"`
		Nodes  []struct {
			Name          string  `json:"name"`
			Health        string  `json:"health"`
			LastError     string  `json:"last_error"`
			LastCheckedAt *string `json:"last_checked_at"`
			ModelCount    int     `json:"model_count"`
		} `json:"nodes"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want 2 entries", resp.Nodes)
	}
	if resp.Warning != "" {
		t.Errorf("warning = %q, want absent with nodes configured", resp.Warning)
	}
	one, two := resp.Nodes[0], resp.Nodes[1]
	if one.Name != "one" || one.Health != "healthy" || one.ModelCount != 2 || one.LastCheckedAt == nil {
		t.Errorf("node one = %+v, want healthy with 2 models and a timestamp", one)
	}
	if two.Name != "two" || two.Health != "unhealthy" || two.LastError != "connection refused" {
		t.Errorf("node two = %+v, want unhealthy with last_error", two)
	}
}

func TestAdminHealth_ZeroNodesWarning(t *testing.T) {
	h, _, _, _ := setupNodesTest(t)

	rec := nodesReq(t, h, http.MethodGet, "/api/admin/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (zero nodes is not degraded), got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Nodes   []any  `json:"nodes"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if resp.Warning != "no nodes configured" {
		t.Errorf("warning = %q, want 'no nodes configured'", resp.Warning)
	}
	if len(resp.Nodes) != 0 {
		t.Errorf("nodes = %+v, want empty", resp.Nodes)
	}
}

// --- helpers -------------------------------------------------------------------

func strPtr(s string) *string { return &s }

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

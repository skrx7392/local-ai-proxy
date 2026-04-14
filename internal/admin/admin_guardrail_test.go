package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// seedAdmin creates a user row with role='admin', active=true, returning its id.
func seedAdmin(t *testing.T, s *store.Store, email string) int64 {
	t.Helper()
	id, err := s.CreateUser(email, "hash", "Admin")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := s.Pool().Exec(context.Background(), `UPDATE users SET role='admin' WHERE id = $1`, id); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	return id
}

func TestAdmin_Deactivate_LastActiveAdmin_Returns409(t *testing.T) {
	h, s := setupAdminTest(t)

	onlyAdmin := seedAdmin(t, s, "only-admin@example.com")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(onlyAdmin, 10)+"/deactivate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 when deactivating only active admin, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	errObj, _ := errResp["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "last_admin" {
		t.Errorf("expected error code 'last_admin', got %v", errObj["code"])
	}

	// And the user must still be active.
	u, _ := s.GetUserByID(onlyAdmin)
	if u == nil || !u.IsActive {
		t.Error("user should remain active after rejected deactivate")
	}
}

func TestAdmin_Deactivate_SecondAdminAllowed(t *testing.T) {
	h, s := setupAdminTest(t)

	_ = seedAdmin(t, s, "keeper@example.com")
	victim := seedAdmin(t, s, "victim@example.com")

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(victim, 10)+"/deactivate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when another admin remains, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := s.GetUserByID(victim)
	if u == nil || u.IsActive {
		t.Error("victim should have been deactivated")
	}
}

func TestAdmin_Deactivate_ConcurrentRaceSerializes(t *testing.T) {
	// Two concurrent deactivations targeting two different admin rows must
	// not both succeed when only two active admins exist. The advisory lock
	// forces serialization; exactly one should win.
	h, s := setupAdminTest(t)

	adminA := seedAdmin(t, s, "race-a@example.com")
	adminB := seedAdmin(t, s, "race-b@example.com")

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes []int
	)
	for _, id := range []int64{adminA, adminB} {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(uid, 10)+"/deactivate", nil)
			// Each concurrent goroutine needs its own X-Admin-Key request, but
			// the 10-req/min X-Admin-Key bucket is shared per-handler. That's
			// fine for two calls.
			req.Header.Set("X-Admin-Key", testAdminKey)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			mu.Lock()
			codes = append(codes, rec.Code)
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	var successes, conflicts int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		}
	}

	if successes != 1 {
		t.Errorf("expected exactly 1 successful deactivation, got %d (codes=%v)", successes, codes)
	}
	if conflicts != 1 {
		t.Errorf("expected exactly 1 conflict (last_admin), got %d (codes=%v)", conflicts, codes)
	}

	// Exactly one of the two admins must still be active.
	uA, _ := s.GetUserByID(adminA)
	uB, _ := s.GetUserByID(adminB)
	activeCount := 0
	if uA != nil && uA.IsActive {
		activeCount++
	}
	if uB != nil && uB.IsActive {
		activeCount++
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active admin after race, got %d", activeCount)
	}
}

func TestAdmin_Activate_DoesNotNeedGuardrail(t *testing.T) {
	// Activation should always succeed — it can only increase the admin
	// count, so the guardrail doesn't apply.
	h, s := setupAdminTest(t)

	id := seedAdmin(t, s, "reactivate@example.com")
	if err := s.SetUserActive(id, false); err != nil {
		t.Fatalf("SetUserActive: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+strconv.FormatInt(id, 10)+"/activate", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on activate, got %d: %s", rec.Code, rec.Body.String())
	}

	u, _ := s.GetUserByID(id)
	if u == nil || !u.IsActive {
		t.Error("expected user active after activate")
	}
}

package creditrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

func setupRecorderTest(t *testing.T) *store.Store {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping recorder integration test")
	}

	s, err := store.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	wipe := func() {
		c := context.Background()
		p := s.Pool()
		_, _ = p.Exec(c, "DELETE FROM registration_events")
		_, _ = p.Exec(c, "DELETE FROM credit_holds")
		_, _ = p.Exec(c, "DELETE FROM credit_transactions")
		_, _ = p.Exec(c, "DELETE FROM account_usage_stats")
		_, _ = p.Exec(c, "DELETE FROM credit_balances")
		_, _ = p.Exec(c, "DELETE FROM credit_pricing")
		_, _ = p.Exec(c, "DELETE FROM registration_tokens")
		_, _ = p.Exec(c, "DELETE FROM usage_logs")
		_, _ = p.Exec(c, "DELETE FROM nodes")
		_, _ = p.Exec(c, "DELETE FROM user_sessions")
		_, _ = p.Exec(c, "DELETE FROM api_keys")
		_, _ = p.Exec(c, "DELETE FROM users")
		_, _ = p.Exec(c, "DELETE FROM federated_identities")
		_, _ = p.Exec(c, "DELETE FROM credit_requests")
		_, _ = p.Exec(c, "DELETE FROM accounts")
	}
	wipe()
	t.Cleanup(func() {
		wipe()
		s.Close()
	})
	return s
}

func provisionCappedEndUser(t *testing.T, s *store.Store, extID, email string) int64 {
	t.Helper()
	res, err := s.ResolveEndUserAccount(store.FederatedIdentity{
		Source: "openwebui", ExternalID: extID, Email: email, DisplayName: "Cap Hit",
	}, 5.0, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	// Burn the whole allowance: the account is now in the capped state.
	if err := s.AddCredits(res.AccountID, -5, "test burn"); err != nil {
		t.Fatalf("burn allowance: %v", err)
	}
	return res.AccountID
}

func pendingRequests(t *testing.T, s *store.Store, accountID int64) int {
	t.Helper()
	rows, err := s.ListCreditRequests("pending", time.Now())
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.AccountID == accountID {
			n++
		}
	}
	return n
}

// captureWebhook records every POST body it receives.
type captureWebhook struct {
	mu     sync.Mutex
	bodies []Notification
	status int
}

func (c *captureWebhook) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		_ = json.NewDecoder(r.Body).Decode(&n)
		c.mu.Lock()
		c.bodies = append(c.bodies, n)
		c.mu.Unlock()
		status := c.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	})
}

func (c *captureWebhook) received() []Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Notification, len(c.bodies))
	copy(out, c.bodies)
	return out
}

func TestRecorder_CapHitFilesAndNotifies(t *testing.T) {
	s := setupRecorderTest(t)
	acc := provisionCappedEndUser(t, s, "rec-1", "dave@example.com")

	hook := &captureWebhook{}
	srv := httptest.NewServer(hook.handler())
	defer srv.Close()

	rec := New(s, srv.URL, 5.0)
	rec.RecordCapHit(acc)
	rec.Wait()

	if n := pendingRequests(t, s, acc); n != 1 {
		t.Fatalf("expected 1 pending request, got %d", n)
	}
	got := hook.received()
	if len(got) != 1 {
		t.Fatalf("expected 1 webhook POST, got %d", len(got))
	}
	n := got[0]
	if n.RequestID == 0 || n.AccountID != acc {
		t.Errorf("ids: got request_id=%d account_id=%d", n.RequestID, n.AccountID)
	}
	if n.Email != "dave@example.com" || n.DisplayName != "Cap Hit" {
		t.Errorf("identity: got email=%q name=%q", n.Email, n.DisplayName)
	}
	if n.MonthlyGrant != 5.0 {
		t.Errorf("expected effective grant 5.0, got %v", n.MonthlyGrant)
	}
	if n.Spent != 5.0 {
		t.Errorf("expected spent 5.0 (grant minus zero balance), got %v", n.Spent)
	}
	if n.Period == "" {
		t.Error("expected a period")
	}
}

func TestRecorder_UsesAccountGrantOverride(t *testing.T) {
	s := setupRecorderTest(t)
	acc := provisionCappedEndUser(t, s, "rec-2", "erin@example.com")
	override := 12.5
	if err := s.SetMonthlyGrant(acc, &override); err != nil {
		t.Fatalf("SetMonthlyGrant: %v", err)
	}

	hook := &captureWebhook{}
	srv := httptest.NewServer(hook.handler())
	defer srv.Close()

	rec := New(s, srv.URL, 5.0)
	rec.RecordCapHit(acc)
	rec.Wait()

	got := hook.received()
	if len(got) != 1 {
		t.Fatalf("expected 1 webhook POST, got %d", len(got))
	}
	if got[0].MonthlyGrant != 12.5 {
		t.Errorf("expected override grant 12.5 in payload, got %v", got[0].MonthlyGrant)
	}
}

func TestRecorder_SecondHitDoesNotRenotify(t *testing.T) {
	s := setupRecorderTest(t)
	acc := provisionCappedEndUser(t, s, "rec-3", "frank@example.com")

	hook := &captureWebhook{}
	srv := httptest.NewServer(hook.handler())
	defer srv.Close()

	rec := New(s, srv.URL, 5.0)
	rec.RecordCapHit(acc)
	rec.Wait()
	rec.RecordCapHit(acc)
	rec.Wait()

	if n := pendingRequests(t, s, acc); n != 1 {
		t.Errorf("expected 1 pending request, got %d", n)
	}
	if got := hook.received(); len(got) != 1 {
		t.Errorf("expected 1 webhook POST, got %d", len(got))
	}
}

func TestRecorder_NoWebhookStillRecords(t *testing.T) {
	s := setupRecorderTest(t)
	acc := provisionCappedEndUser(t, s, "rec-4", "grace@example.com")

	rec := New(s, "", 5.0)
	rec.RecordCapHit(acc)
	rec.Wait()

	if n := pendingRequests(t, s, acc); n != 1 {
		t.Errorf("expected request recorded without webhook, got %d", n)
	}
}

func TestRecorder_WebhookFailureStillRecords(t *testing.T) {
	s := setupRecorderTest(t)
	acc := provisionCappedEndUser(t, s, "rec-5", "heidi@example.com")

	hook := &captureWebhook{status: http.StatusInternalServerError}
	srv := httptest.NewServer(hook.handler())
	defer srv.Close()

	rec := New(s, srv.URL, 5.0)
	rec.RecordCapHit(acc)
	rec.Wait()

	if n := pendingRequests(t, s, acc); n != 1 {
		t.Errorf("expected request recorded despite webhook failure, got %d", n)
	}
	// Delivery is attempted, then retried once on failure.
	if got := hook.received(); len(got) != 2 {
		t.Errorf("expected 2 delivery attempts (1 retry), got %d", len(got))
	}
}

// Slow/failing deliveries must never cause a cap-hit to go unrecorded: the
// filing always runs; only webhook delivery queues behind the concurrency
// cap. (With the old whole-recording semaphore, accounts beyond the cap were
// silently dropped.)
func TestRecorder_SlowWebhookNeverBlocksFiling(t *testing.T) {
	s := setupRecorderTest(t)

	hook := &captureWebhook{status: http.StatusInternalServerError}
	srv := httptest.NewServer(hook.handler())
	defer srv.Close()

	rec := New(s, srv.URL, 5.0)
	const accounts = 10 // more than the delivery semaphore's 8 slots
	for i := 0; i < accounts; i++ {
		acc := provisionCappedEndUser(t, s,
			fmt.Sprintf("sat-%d", i), fmt.Sprintf("sat%d@example.com", i))
		rec.RecordCapHit(acc)
	}
	rec.Wait()

	rows, err := s.ListCreditRequests("pending", time.Now())
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	if len(rows) != accounts {
		t.Errorf("expected all %d cap-hits filed despite failing webhook, got %d", accounts, len(rows))
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	var rec *Recorder
	rec.RecordCapHit(1) // must not panic
	rec.Wait()
}

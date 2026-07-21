package store

import (
	"sync"
	"testing"
	"time"
)

// provisionEndUser creates an allowance-managed end-user account the way
// production does: through federated identity resolution.
func provisionEndUser(t *testing.T, s *Store, extID, email string, grant float64) int64 {
	t.Helper()
	res, err := s.ResolveEndUserAccount(FederatedIdentity{
		Source: "openwebui", ExternalID: extID, Email: email, DisplayName: "Test User",
	}, grant, time.Now())
	if err != nil {
		t.Fatalf("ResolveEndUserAccount: %v", err)
	}
	return res.AccountID
}

func countCreditRequests(t *testing.T, s *Store, accountID int64) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM credit_requests WHERE account_id = $1`, accountID,
	).Scan(&n); err != nil {
		t.Fatalf("count credit_requests: %v", err)
	}
	return n
}

func TestFileCreditRequest_FirstCapHitFiles(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-1", "cr1@example.com", 5)

	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	id, filed, err := s.FileCreditRequest(acc, now)
	if err != nil {
		t.Fatalf("FileCreditRequest: %v", err)
	}
	if !filed || id == 0 {
		t.Fatalf("expected a filed request with id, got filed=%v id=%d", filed, id)
	}

	var period time.Time
	var status string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT period, status FROM credit_requests WHERE id = $1`, id,
	).Scan(&period, &status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "pending" {
		t.Errorf("expected status pending, got %q", status)
	}
	if period.Format("2006-01-02") != "2026-07-01" {
		t.Errorf("expected period 2026-07-01, got %s", period.Format("2006-01-02"))
	}
}

func TestFileCreditRequest_PendingDedupes(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-2", "cr2@example.com", 5)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	if _, filed, err := s.FileCreditRequest(acc, now); err != nil || !filed {
		t.Fatalf("first file: filed=%v err=%v", filed, err)
	}
	if _, filed, err := s.FileCreditRequest(acc, now.Add(time.Hour)); err != nil || filed {
		t.Fatalf("second file same month: expected no-op, got filed=%v err=%v", filed, err)
	}
	if n := countCreditRequests(t, s, acc); n != 1 {
		t.Errorf("expected 1 request, got %d", n)
	}
}

func TestFileCreditRequest_RefileAfterGrant(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-3", "cr3@example.com", 5)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	id, _, err := s.FileCreditRequest(acc, now)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := s.ResolveCreditRequest(id, "granted", "+$5 via test"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The user burned the top-up too: a new episode files a new request.
	_, filed, err := s.FileCreditRequest(acc, now.Add(48*time.Hour))
	if err != nil || !filed {
		t.Fatalf("refile after grant: expected filed, got filed=%v err=%v", filed, err)
	}
	if n := countCreditRequests(t, s, acc); n != 2 {
		t.Errorf("expected 2 requests, got %d", n)
	}
}

func TestFileCreditRequest_DismissSilencesMonth(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-4", "cr4@example.com", 5)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	id, _, err := s.FileCreditRequest(acc, now)
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := s.ResolveCreditRequest(id, "dismissed", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, filed, err := s.FileCreditRequest(acc, now.Add(72*time.Hour)); err != nil || filed {
		t.Fatalf("file after dismiss same month: expected no-op, got filed=%v err=%v", filed, err)
	}

	// A new month is a fresh episode even after a dismissal.
	_, filed, err := s.FileCreditRequest(acc, time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC))
	if err != nil || !filed {
		t.Fatalf("file in new month: expected filed, got filed=%v err=%v", filed, err)
	}
}

func TestFileCreditRequest_ConcurrentSingleWinner(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-5", "cr5@example.com", 5)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	const workers = 8
	var wg sync.WaitGroup
	filedCount := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, filed, err := s.FileCreditRequest(acc, now)
			if err != nil {
				t.Errorf("concurrent file: %v", err)
				return
			}
			filedCount <- filed
		}()
	}
	wg.Wait()
	close(filedCount)

	winners := 0
	for filed := range filedCount {
		if filed {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}
	if n := countCreditRequests(t, s, acc); n != 1 {
		t.Errorf("expected 1 request row, got %d", n)
	}
}

func TestResolveCreditRequest_Errors(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-6", "cr6@example.com", 5)

	if err := s.ResolveCreditRequest(999999, "granted", ""); err != ErrCreditRequestNotFound {
		t.Errorf("unknown id: expected ErrCreditRequestNotFound, got %v", err)
	}

	id, _, err := s.FileCreditRequest(acc, time.Now())
	if err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := s.ResolveCreditRequest(id, "granted", "+$1"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := s.ResolveCreditRequest(id, "dismissed", ""); err != ErrCreditRequestResolved {
		t.Errorf("second resolve: expected ErrCreditRequestResolved, got %v", err)
	}
}

func TestListCreditRequests_JoinsAccountDisplay(t *testing.T) {
	s := setupTestStore(t)
	accA := provisionEndUser(t, s, "cr-7a", "alice@example.com", 5)
	accB := provisionEndUser(t, s, "cr-7b", "bob@example.com", 5)
	override := 12.5
	if err := s.SetMonthlyGrant(accB, &override); err != nil {
		t.Fatalf("SetMonthlyGrant: %v", err)
	}

	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	idA, _, _ := s.FileCreditRequest(accA, now)
	if _, _, err := s.FileCreditRequest(accB, now.Add(time.Minute)); err != nil {
		t.Fatalf("file B: %v", err)
	}

	rows, err := s.ListCreditRequests("pending")
	if err != nil {
		t.Fatalf("ListCreditRequests: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 pending rows, got %d", len(rows))
	}
	// Newest first.
	if rows[0].AccountID != accB || rows[1].AccountID != accA {
		t.Errorf("expected newest-first [B, A], got [%d, %d]", rows[0].AccountID, rows[1].AccountID)
	}
	if rows[0].Email == nil || *rows[0].Email != "bob@example.com" {
		t.Errorf("expected bob email, got %v", rows[0].Email)
	}
	if rows[0].MonthlyGrant == nil || *rows[0].MonthlyGrant != 12.5 {
		t.Errorf("expected override grant 12.5, got %v", rows[0].MonthlyGrant)
	}
	if rows[1].MonthlyGrant != nil {
		t.Errorf("expected nil grant override for A, got %v", *rows[1].MonthlyGrant)
	}
	// Provisioned with grant 5 and nothing spent: balance rides along for display.
	if rows[1].Balance != 5 {
		t.Errorf("expected balance 5 for A, got %v", rows[1].Balance)
	}

	if err := s.ResolveCreditRequest(idA, "granted", "+$5"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	granted, err := s.ListCreditRequests("granted")
	if err != nil {
		t.Fatalf("list granted: %v", err)
	}
	if len(granted) != 1 || granted[0].ID != idA {
		t.Errorf("expected [%d] granted, got %+v", idA, granted)
	}
	if granted[0].ResolvedAt == nil || granted[0].ResolvedNote == nil || *granted[0].ResolvedNote != "+$5" {
		t.Errorf("expected resolution metadata, got at=%v note=%v", granted[0].ResolvedAt, granted[0].ResolvedNote)
	}
}

func TestGetCreditRequestInfo(t *testing.T) {
	s := setupTestStore(t)
	acc := provisionEndUser(t, s, "cr-8", "carol@example.com", 5)
	// Burn the whole allowance so the cap-hit state is realistic.
	if err := s.AddCredits(acc, -5, "test burn"); err != nil {
		t.Fatalf("burn: %v", err)
	}

	id, _, err := s.FileCreditRequest(acc, time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("file: %v", err)
	}

	info, err := s.GetCreditRequestInfo(id)
	if err != nil {
		t.Fatalf("GetCreditRequestInfo: %v", err)
	}
	if info.AccountID != acc {
		t.Errorf("account: expected %d, got %d", acc, info.AccountID)
	}
	if info.Email == nil || *info.Email != "carol@example.com" {
		t.Errorf("email: got %v", info.Email)
	}
	if info.DisplayName == nil || *info.DisplayName != "Test User" {
		t.Errorf("display name: got %v", info.DisplayName)
	}
	if info.MonthlyGrant != nil {
		t.Errorf("expected nil grant override, got %v", *info.MonthlyGrant)
	}
	if info.Balance != 0 {
		t.Errorf("expected balance 0, got %v", info.Balance)
	}
	if info.Period.Format("2006-01-02") != "2026-07-01" {
		t.Errorf("period: got %s", info.Period.Format("2006-01-02"))
	}

	if _, err := s.GetCreditRequestInfo(999999); err == nil {
		t.Error("expected error for unknown request id")
	}
}

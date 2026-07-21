// Package creditrequest records "account hit its monthly allowance" events
// and notifies the operator's webhook (docs/design/credit-requests.md).
//
// Both monthly_limit_reached 402 sites (the credit gate's pre-check and the
// reserve failure in the chat handler) call RecordCapHit. The store dedupes
// per account+month, so a capped user retrying in chat produces exactly one
// filed request and one notification per episode.
package creditrequest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/krishna/local-ai-proxy/internal/store"
)

// MonthlyLimitMessage is the user-facing 402 text for allowance-managed
// accounts. It lives here so both 402 sites stay in sync — the wording
// promises the admin was notified, which is exactly what RecordCapHit does.
const MonthlyLimitMessage = "Monthly usage limit reached — your admin has been notified. " +
	"Credits may be added shortly; otherwise your allowance resets next month."

// Notification is the webhook payload for one filed credit request.
type Notification struct {
	RequestID    int64   `json:"request_id"`
	AccountID    int64   `json:"account_id"`
	Email        string  `json:"email"`
	DisplayName  string  `json:"display_name"`
	MonthlyGrant float64 `json:"monthly_grant"` // effective (override or env default)
	Spent        float64 `json:"spent"`         // grant minus remaining balance
	Period       string  `json:"period"`        // YYYY-MM-DD, first day of the month
}

// Recorder files credit requests and delivers webhook notifications
// asynchronously so the 402 response path never waits on it. A nil *Recorder
// is a no-op, letting call sites skip nil checks.
type Recorder struct {
	db           *store.Store
	webhookURL   string // empty = record only, no notification
	defaultGrant float64
	client       *http.Client

	wg  sync.WaitGroup
	sem chan struct{} // caps concurrent webhook deliveries (never the filing)
}

// New builds a Recorder. webhookURL may be empty (requests are still
// recorded and visible in the admin console — the webhook is a consumer,
// not the source of truth).
func New(db *store.Store, webhookURL string, defaultGrant float64) *Recorder {
	return &Recorder{
		db:           db,
		webhookURL:   webhookURL,
		defaultGrant: defaultGrant,
		client:       &http.Client{Timeout: 5 * time.Second},
		sem:          make(chan struct{}, 8),
	}
}

// RecordCapHit files a credit request for the account (deduped per month)
// and fires the webhook when this call was the one that filed it. Async so
// the 402 response never waits. The filing itself always runs — one fast
// indexed insert, the same load profile as the 402 handling that triggered
// it — so slow webhook deliveries can never cause a cap-hit to go
// unrecorded; only delivery is concurrency-capped.
func (r *Recorder) RecordCapHit(accountID int64) {
	if r == nil {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.recordCapHit(accountID)
	}()
}

// Wait blocks until all in-flight recordings finish. Used by tests and
// shutdown; never called on the request path.
func (r *Recorder) Wait() {
	if r == nil {
		return
	}
	r.wg.Wait()
}

func (r *Recorder) recordCapHit(accountID int64) {
	id, filed, err := r.db.FileCreditRequest(accountID, time.Now())
	if err != nil {
		slog.Error("file credit request failed", "error", err, "account_id", accountID)
		return
	}
	if !filed {
		return
	}
	slog.Info("credit request filed", "request_id", id, "account_id", accountID)
	if r.webhookURL == "" {
		return
	}

	info, err := r.db.GetCreditRequestInfo(id)
	if err != nil {
		slog.Error("credit request info lookup failed", "error", err, "request_id", id)
		return
	}
	grant := r.defaultGrant
	if info.MonthlyGrant != nil {
		grant = *info.MonthlyGrant
	}
	n := Notification{
		RequestID:    id,
		AccountID:    info.AccountID,
		MonthlyGrant: grant,
		Spent:        grant - info.Balance,
		Period:       info.Period.Format("2006-01-02"),
	}
	if info.Email != nil {
		n.Email = *info.Email
	}
	if info.DisplayName != nil {
		n.DisplayName = *info.DisplayName
	}
	// Blocking acquire is safe here: only filing winners reach delivery, and
	// there is at most one winner per account per month.
	r.sem <- struct{}{}
	defer func() { <-r.sem }()
	if err := r.deliver(n); err != nil {
		// The bot reconciles pending requests on startup and the admin
		// console lists them, so a lost notification self-heals.
		slog.Error("credit request notification failed", "error", err, "request_id", id)
	}
}

// deliver POSTs the notification, retrying once. Fire-and-forget beyond
// that — durability lives in the credit_requests table, not the webhook.
func (r *Recorder) deliver(n Notification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		resp, err := r.client.Post(r.webhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return lastErr
}

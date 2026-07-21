package credits

import (
	"net/http"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// CreditGate is a fast pre-check middleware that rejects requests from
// inactive accounts or accounts with zero/negative effective balance.
// Every key must be attached to an account — there is no bypass. The
// startup backfill (BackfillAdminKeyAccounts) attaches legacy NULL-account
// keys to the admin service account, so a nil AccountID can only mean a
// misprovisioned key; the gate fails closed on it.
//
// The gate checks the BILLING account: the billing.Resolution attached by
// the billing middleware when present (end-user accounts on trusted keys),
// else the key's own account. Allowance-managed accounts get the
// monthly_limit_reached wording so chat users see an actionable error.
func CreditGate(db *store.Store, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFromContext(r.Context())
			if key == nil {
				// No key in context: nothing to gate — authentication is the
				// auth middleware's responsibility, not the credit gate's.
				next.ServeHTTP(w, r)
				return
			}

			var accountID int64
			var allowanceManaged bool
			if res, ok := billing.FromContext(r.Context()); ok {
				accountID = res.AccountID
				allowanceManaged = res.AllowanceManaged
			} else if key.AccountID != nil {
				accountID = *key.AccountID
			} else {
				m.RecordCreditGateReject()
				apierror.WriteError(w, r, http.StatusForbidden, "account_required", "invalid_request_error", "API key is not attached to an account")
				return
			}

			isActive, balance, reserved, err := db.GetAccountCreditStatus(accountID)
			if err != nil {
				apierror.WriteError(w, r, http.StatusInternalServerError, "internal_error", "server_error", "Failed to check credit status")
				return
			}

			if !isActive {
				m.RecordCreditGateReject()
				apierror.WriteError(w, r, http.StatusForbidden, "account_disabled", "invalid_request_error", "Account is disabled")
				return
			}

			if balance-reserved <= 0 {
				m.RecordCreditGateReject()
				if allowanceManaged {
					apierror.WriteError(w, r, http.StatusPaymentRequired, "monthly_limit_reached", "invalid_request_error", "Monthly usage limit reached — resets next month")
					return
				}
				apierror.WriteError(w, r, http.StatusPaymentRequired, "insufficient_credits", "invalid_request_error", "Insufficient credits")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

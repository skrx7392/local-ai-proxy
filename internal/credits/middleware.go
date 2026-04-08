package credits

import (
	"net/http"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/metrics"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// CreditGate is a fast pre-check middleware that rejects requests from
// inactive accounts or accounts with zero/negative effective balance.
// Keys without AccountID (legacy admin keys) pass through.
func CreditGate(db *store.Store, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFromContext(r.Context())
			if key == nil || key.AccountID == nil {
				next.ServeHTTP(w, r)
				return
			}

			isActive, balance, reserved, err := db.GetAccountCreditStatus(*key.AccountID)
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
				apierror.WriteError(w, r, http.StatusPaymentRequired, "insufficient_credits", "invalid_request_error", "Insufficient credits")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

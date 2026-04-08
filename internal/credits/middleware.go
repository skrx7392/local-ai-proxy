package credits

import (
	"encoding/json"
	"net/http"

	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

func writeJSONError(w http.ResponseWriter, statusCode int, code, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
}

// CreditGate is a fast pre-check middleware that rejects requests from
// inactive accounts or accounts with zero/negative effective balance.
// Keys without AccountID (legacy admin keys) pass through.
func CreditGate(db *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFromContext(r.Context())
			if key == nil || key.AccountID == nil {
				next.ServeHTTP(w, r)
				return
			}

			isActive, balance, reserved, err := db.GetAccountCreditStatus(*key.AccountID)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "internal_error", "server_error", "Failed to check credit status")
				return
			}

			if !isActive {
				writeJSONError(w, http.StatusForbidden, "account_disabled", "invalid_request_error", "Account is disabled")
				return
			}

			if balance-reserved <= 0 {
				writeJSONError(w, http.StatusPaymentRequired, "insufficient_credits", "invalid_request_error", "Insufficient credits")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

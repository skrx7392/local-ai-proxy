// Package billing resolves which account a proxied request is billed to.
//
// It sits between auth and the credit gate (docs/design/end-user-accounts.md):
// for a key with trust_user_headers, a forwarded OpenWebUI identity is mapped
// to its own auto-provisioned end-user account (with monthly allowance);
// every other request bills the key's account. Downstream consumers
// (CreditGate, the chat handler) read the Resolution from the request context
// instead of key.AccountID so the pre-check, reserve, settle, and usage row
// all follow the same billing account.
package billing

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/store"
)

// Headers forwarded by OpenWebUI when ENABLE_FORWARD_USER_INFO_HEADERS is on.
// User-Id is the identity authority; the others are display metadata.
const (
	HeaderUserID    = "X-OpenWebUI-User-Id"
	HeaderUserEmail = "X-OpenWebUI-User-Email"
	HeaderUserName  = "X-OpenWebUI-User-Name"
)

// SourceOpenWebUI is the federated_identities.source for OpenWebUI users.
const SourceOpenWebUI = "openwebui"

// Resolution is the billing decision for one request.
type Resolution struct {
	AccountID        int64
	AllowanceManaged bool // true for auto-provisioned end-user accounts (drives 402 wording)
	// RateLimitPerMin is the billing account's rate_limit_per_min override;
	// nil = class env default. Carried here so the rate limiter (which runs
	// right after billing) needs no extra DB read.
	RateLimitPerMin *int
}

type ctxKey struct{}

// WithResolution returns a context carrying the billing resolution.
func WithResolution(ctx context.Context, res Resolution) context.Context {
	return context.WithValue(ctx, ctxKey{}, res)
}

// FromContext returns the billing resolution, if one was attached.
func FromContext(ctx context.Context) (Resolution, bool) {
	res, ok := ctx.Value(ctxKey{}).(Resolution)
	return res, ok
}

// Middleware attaches a billing Resolution to every authenticated request.
// Identity headers are honored ONLY on keys with trust_user_headers —
// spoofed headers on any other key are inert (billing unchanged).
// Resolution failure for a trusted identity fails closed: billing an end
// user against the shared account instead would breach account isolation.
func Middleware(db *store.Store, defaultGrant float64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFromContext(r.Context())
			if key == nil || db == nil {
				next.ServeHTTP(w, r)
				return
			}

			if key.TrustUserHeaders {
				if extID := strings.TrimSpace(r.Header.Get(HeaderUserID)); extID != "" {
					res, err := db.ResolveEndUserAccount(store.FederatedIdentity{
						Source:      SourceOpenWebUI,
						ExternalID:  extID,
						Email:       strings.TrimSpace(r.Header.Get(HeaderUserEmail)),
						DisplayName: strings.TrimSpace(r.Header.Get(HeaderUserName)),
					}, defaultGrant, time.Now())
					if err != nil {
						slog.ErrorContext(r.Context(), "end-user billing resolution failed",
							"error", err, "external_id", extID)
						apierror.WriteError(w, r, http.StatusInternalServerError,
							"internal_error", "server_error", "Failed to resolve billing account")
						return
					}
					next.ServeHTTP(w, r.WithContext(WithResolution(r.Context(), Resolution{
						AccountID:        res.AccountID,
						AllowanceManaged: true,
						RateLimitPerMin:  res.RateLimitPerMin,
					})))
					return
				}
			}

			if key.AccountID != nil {
				// AllowanceManaged comes from the ACCOUNT, not the identity
				// path: a key created directly on an end-user account keeps
				// that account's limiter class and 402 semantics.
				r = r.WithContext(WithResolution(r.Context(), Resolution{
					AccountID:        *key.AccountID,
					AllowanceManaged: key.AccountAllowanceManaged,
					RateLimitPerMin:  key.AccountRateLimitPerMin,
				}))
			}
			next.ServeHTTP(w, r)
		})
	}
}

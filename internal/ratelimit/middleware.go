package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/krishna/local-ai-proxy/internal/apierror"
	"github.com/krishna/local-ai-proxy/internal/auth"
	"github.com/krishna/local-ai-proxy/internal/billing"
	"github.com/krishna/local-ai-proxy/internal/metrics"
)

// Limits carries the class-default account rate limits (requests/minute)
// and concurrency caps (in-flight non-GET requests). A per-account override
// (accounts.rate_limit_per_min) takes precedence over the per-minute
// defaults via EffectiveLimit; concurrency caps are env-only. Zero-value
// caps mean "no cap" (boot validation keeps prod values >= 1).
type Limits struct {
	EndUserPerMin int // accounts with AllowanceManaged=true (END_USER_RATELIMIT_PER_MIN)
	ServicePerMin int // every other account (ACCOUNT_RATELIMIT_PER_MIN)

	EndUserMaxConcurrent int // END_USER_MAX_CONCURRENT
	ServiceMaxConcurrent int // ACCOUNT_MAX_CONCURRENT
}

func (l Limits) maxConcurrent(allowanceManaged bool) int {
	if allowanceManaged {
		return l.EndUserMaxConcurrent
	}
	return l.ServiceMaxConcurrent
}

// EffectiveLimit resolves one account's rate limit: the per-account override
// when set, else the class default. Single source of truth — the middleware
// and the admin accounts listing both go through it.
func EffectiveLimit(override *int, allowanceManaged bool, limits Limits) int {
	if override != nil {
		return *override
	}
	if allowanceManaged {
		return limits.EndUserPerMin
	}
	return limits.ServicePerMin
}

// Middleware enforces the account-level and key-level rate limits and the
// per-account concurrency cap, in that order
// (docs/design/per-account-rate-limiting.md §3.2):
//
//  1. Account bucket, keyed by the billing resolution's account ID. Checked
//     first so a throttled user's rejects never drain the shared key's
//     aggregate bucket.
//  2. Key bucket, unchanged semantics. A key-level reject refunds the
//     account token — different owners on the shared trusted key (see
//     Limiter.Return).
//  3. Concurrency slot (non-GET only — GETs like /api/v1/models don't
//     occupy GPU). A concurrency reject deliberately does NOT refund the
//     account token: a free retry would let a client busy-poll the
//     semaphore at zero cost, and it is the polling client's own budget.
//     The slot is released in this middleware's own defer, which covers
//     full stream lifetime, client disconnects, and handler panics
//     (handleStreaming runs synchronously inside the handler; no Hijack).
//
// Requests without a billing resolution (legacy nil-account keys — a
// reachable prod path that 403s at the credit gate) see only the key
// bucket; requests without a key pass through untouched.
func Middleware(keys, accounts *Limiter, conc *ConcurrencyLimiter, limits Limits, m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFromContext(r.Context())
			if key == nil {
				// No key in context means auth middleware didn't run (shouldn't happen)
				next.ServeHTTP(w, r)
				return
			}

			res, hasRes := billing.FromContext(r.Context())
			if hasRes {
				limit := EffectiveLimit(res.RateLimitPerMin, res.AllowanceManaged, limits)
				if allowed, retryAfter := accounts.Allow(res.AccountID, limit); !allowed {
					m.RecordAccountRateLimitReject("rate", classLabel(res.AllowanceManaged))
					writeRateLimited(w, r, retryAfter,
						fmt.Sprintf("Rate limit exceeded for your account (%d req/min)", limit))
					return
				}
			}

			if allowed, retryAfter := keys.Allow(key.ID, key.RateLimit); !allowed {
				if hasRes {
					accounts.Return(res.AccountID)
				}
				m.RecordRateLimitReject()
				writeRateLimited(w, r, retryAfter,
					fmt.Sprintf("Rate limit exceeded for this API key (%d req/min)", key.RateLimit))
				return
			}

			if hasRes && r.Method != http.MethodGet {
				max := limits.maxConcurrent(res.AllowanceManaged)
				if !conc.TryAcquire(res.AccountID, max) {
					m.RecordAccountRateLimitReject("concurrency", classLabel(res.AllowanceManaged))
					// Retry-After is advisory — stream end time is unknowable.
					w.Header().Set("Retry-After", "5")
					apierror.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded",
						fmt.Sprintf("Too many concurrent requests for your account (max %d); retry shortly", max))
					return
				}
				m.RecordStreamStart()
				defer func() {
					conc.Release(res.AccountID)
					m.RecordStreamEnd()
				}()
			}

			next.ServeHTTP(w, r)
		})
	}
}

func classLabel(allowanceManaged bool) string {
	if allowanceManaged {
		return "enduser"
	}
	return "service"
}

// writeRateLimited emits the OpenAI-envelope 429. The message is
// scope-differentiated (account vs key) because Open WebUI surfaces the raw
// string — identical bodies would make background title/tag failures
// undiagnosable. Retry-After is clamped to [1s, 1h]: OpenAI SDKs auto-retry
// honoring it, so it must never be 0 (retry storm) or infinite (a zero-limit
// bucket's refill rate is 0).
func writeRateLimited(w http.ResponseWriter, r *http.Request, retryAfter float64, scope string) {
	secs := math.Ceil(retryAfter)
	if secs < 1 || math.IsNaN(secs) {
		secs = 1
	}
	if secs > 3600 || math.IsInf(secs, 1) {
		secs = 3600
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(secs)))
	apierror.WriteError(w, r, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_exceeded",
		fmt.Sprintf("%s; retry in %ds", scope, int(secs)))
}

# Per-Account Rate Limiting

Status: accepted (2026-07-21 — Krishna approved the §8 recommendations as written:
30/min end-user default, shared-key ceiling stays 120/min with a runbook note,
/models keeps consuming rate tokens, chain reorder accepted with regression test,
grandfathering audit pre-deploy, concurrency defaults 5/8 env-only)
Author: Claude, synthesized from three design explorations + cross-review

## 1. Problem & abuse model

Rate limiting today is keyed on the **API key**, but the billing/fairness entity is the
**account**. That mismatch produces two inverse holes, plus a gap no req/min limit covers:

- **Scenario 1 — key multiplication.** A service account with N keys gets N× the per-key
  limit. Per-key buckets can never fix this; the ceiling must live on the account.
- **Scenario 2 — shared-key starvation.** All Open WebUI end users arrive on ONE trusted
  service key (prod: 120 req/min). One user hammering the key 429s everyone; there is no
  per-user isolation at all. End users are auto-provisioned accounts, so keying on the
  account gives each user a private bucket for free.
- **Scenario 3 — streaming occupancy.** The scarce resource is GPU inference capacity, and
  a req/min limit does not bound it: one account can open its whole minute budget as
  simultaneous long-lived SSE streams (`WriteTimeout=0`, per-node timeout default 5 min).
  Requests/min bounds arrival rate, not occupancy. A per-account concurrent-stream cap does.
- **Scenario 4 — NOT fixed (say so honestly).** A scripted client grinding steadily inside
  its own limits is not stopped by any rate limiter. Credits bound its spend; account
  deactivation (existing creditGate 403) is the kill-switch; metrics make it visible.
  Do not oversell this feature as abuse-proof.

Constraint that shapes every default: Open WebUI fires **2–4 upstream completions per
visible chat message** (title/tag/follow-up generation) plus `/models` fetches. Limits
sized for "one request per message" break normal UX opaquely — background tasks fail with
raw error strings surfaced in the UI.

## 2. Current state

- **Chat chain** (`cmd/proxy/main.go:289`): `cors(auth(billingResolver(creditGate(rateLimit(proxy)))))`.
  Rate limiting already runs after billing resolution, so `billing.Resolution
  {AccountID, AllowanceManaged}` (`internal/billing/billing.go`) is in context at
  rate-limit time. **The keying change needs zero new per-request DB reads.**
- **Per-key limiter** (`internal/ratelimit/ratelimit.go`): in-memory token bucket keyed on
  `api_keys.id`, per-key `rate_limit` column, capacity = limit, refill = limit/60
  tokens/sec, new bucket seeds capacity−1, 1-min prune ticker / 10-min idle cutoff, 429
  with Retry-After. Nil key passes through (`ratelimit.go:100-104`). Tests use
  `time.Sleep` (no injectable clock yet).
- **`internal/authlimit`**: keyed limiters (per-IP / per-email) guarding the public auth
  surface; documents the single-replica in-memory assumption (`authlimit.go:8-9`) and has
  the two patterns we reuse: injectable clock (`NewWithClock`) and the bcrypt counting
  semaphore (`authlimit.go:192-213`).
- **No concurrency bound anywhere** on `/api/v1/`.
- House style for per-account settings: `accounts.monthly_grant` nullable override +
  `PUT /api/admin/accounts/{id}/allowance` + admin-UI AllowanceDialog
  (`internal/store/federated.go:217-229`, schema guard at `schema.sql:281-285`).

## 3. Recommended design

Three explorations (minimal / robust / abuse-model) were cross-reviewed by three judges who
split 1-1-1. The synthesis below takes **minimal's** phasing, plumbing, and refund
mechanics, **robust's** concurrency semaphore and correctness details, and
**abuse-model's** scenario framing and 429 message UX. Judge disagreements are resolved
inline, with reasons.

### 3.1 Keying and classes

Add a **second, separate `Limiter` instance** inside `internal/ratelimit` (not the same
map — key IDs and account IDs share the int64 space; one map would collide key #7 with
account #7). Same lazy-refill token bucket as today; capacity/refill rewritten on every
`Allow` call so limit changes apply on the next request. While touching the package,
retrofit injectable `nowFn` (`NewWithClock`, the authlimit pattern) into both limiters to
kill the `time.Sleep` tests.

- Bucket key: `billing.Resolution.AccountID` via `billing.FromContext` — never `key.ID` or
  `key.AccountID`, because the shared trusted key resolves a *different* end-user account
  per request.
- Class discriminator: `Resolution.AllowanceManaged` — **not** `key.TrustUserHeaders` (a
  trusted key without forwarded headers bills its own service account).
- Effective limit = per-account override if non-NULL, else `END_USER_RATELIMIT_PER_MIN`
  when AllowanceManaged, else `ACCOUNT_RATELIMIT_PER_MIN`. This override-else-class-default
  resolution lives in **one exported helper in `ratelimit`** that the limiter, admin
  `listAccounts`, and any future site all call — the logic is already duplicated twice for
  monthly_grant; do not ship a third copy.
- Fallbacks, stated precisely: `Resolution.AccountID` is a non-pointer int64; the only
  guard is `FromContext`'s ok bool. Missing Resolution → per-key bucket only. **This path
  is reachable in prod**, not test-only: billing attaches no Resolution for legacy
  nil-account keys, which under the new chain order consume a key token and then 403 at
  creditGate. Nil key → pass through, unchanged.

### 3.2 Check order and refund semantics (judges disagreed; resolved)

Every `/api/v1/` request passes gates in this order, centralized in one exported
`CheckAll`-style function so ordering and unwinding are tested in one place:

1. **Account bucket** `Allow(accountID, effectiveLimit)` — reject → 429. Account-first is
   load-bearing: a throttled user's rejects must never drain the shared key's aggregate
   bucket.
2. **Key bucket** `Allow(key.ID, key.RateLimit)` — unchanged semantics. Reject → 429 **and
   refund the account token** (`Limiter.Return(id)`: `tokens = min(capacity, tokens+1)`,
   ~10 lines, clamped at capacity, property-tested `tokens ≤ capacity`). The refund is not
   optional polish: on the shared Open WebUI key the key bucket belongs to the *service
   key* and the account bucket to the *individual end user* — different owners. Without
   the refund, aggregate saturation burns every innocent user's personal budget on
   requests that never ran, and their Retry-After is a lie. (abuse-model's "both budgets
   belong to the same account, skip refunds" rests on a false premise; two judges
   confirmed. Do not delete `Return()` as needless complexity — this paragraph is why it
   exists.)
3. **Concurrency semaphore** (non-GET only) `TryAcquire(accountID, classCap)` — a mutexed
   `map[int64]int` (keyed generalization of the bcrypt semaphore), entries deleted at
   zero. Reject → 429. **No refund on concurrency reject** — resolved deliberately: a
   full refund would let a client busy-poll the semaphore at zero cost; consuming one
   account rate token bounds the poll rate, and it is the polling client's own budget.
   `Release` in the middleware's **outermost defer** after `next.ServeHTTP` returns —
   `handleStreaming` runs synchronously inside the handler (no Hijack path), so this
   correctly covers full stream lifetime, client disconnects, and panics. GETs
   (`/api/v1/models`) consume rate tokens (abuse bound) but never a concurrency slot
   (they don't occupy GPU).

Net effect: multi-key service accounts are bounded by one account bucket (Scenario 1
closed); each Open WebUI user gets a private bucket and stream cap (Scenarios 2 and 3
closed); the shared key's per-key bucket remains as the deliberate **aggregate ceiling**.

**Keep the per-key check for end-user traffic — it is load-bearing, not legacy.** New
buckets seed capacity−1, so an attacker rotating forged `X-OpenWebUI-User-Id` values mints
a fresh near-full 30-token bucket per identity. The shared key's per-key bucket is the only
aggregate bound that makes identity-rotation unprofitable. Raise it via the existing
`PUT /api/admin/keys/{id}/rate-limit` as headcount grows (runbook: >4 simultaneously
active users at 30/min each will collectively starve at 120/min).

### 3.3 Middleware placement

One-line chain swap: `cors(auth(billingResolver(rateLimit(creditGate(proxy)))))`. The
limiter needs Resolution so it cannot move earlier than billingResolver; moving it before
creditGate shields the per-request credit-status query from unthrottled 402-spam.

**Known interaction, decided deliberately:** `RecordCapHit` — the trigger for the
just-shipped Discord credit-request flow — fires only inside creditGate's 402 branch
(`internal/credits/middleware.go:65`). Under the new order, an over-cap end user who is
*also* over-rate sees 429s and never files a top-up request. **Accepted**: cap-hit
recording fires on the first request that passes the rate gates, which a capped-but-human
user will produce within seconds; the alternative (recording cap-hits from the limiter
path) couples the limiter to billing state for no real gain. A regression test must pin
that the credit-request auto-trigger still fires for rate-passing over-cap requests.

### 3.4 429 + Retry-After semantics

- Emit via `apierror.WriteError` — it produces the identical OpenAI envelope
  `{"error":{message, type, code}}` that creditGate and billing already use on `/api/v1/`
  (minimal's "SDKs will mis-parse apierror" rationale was checked and is wrong).
- `type`/`code` stay `rate_limit_exceeded` for **all three** gates — SDK parsers key on
  these; a novel code buys nothing (resolves robust's separate `concurrency_limit_exceeded`
  code against abuse-model's keep-identical graft).
- **Human messages are scope-differentiated** — Open WebUI surfaces the raw message string,
  and identical bodies make background title/tag failures undiagnosable:
  - `"Rate limit exceeded for your account (30 req/min); retry in 4s"`
  - `"Rate limit exceeded for this API key (120 req/min); retry in 2s"`
  - `"Too many concurrent requests for your account (max 5); retry shortly"`
- Retry-After = `ceil((1−tokens)/refillRate)` from whichever bucket rejected, floor 1s.
  Truthful because of the refund rule. Concurrency rejects send fixed `Retry-After: 5`
  (advisory — stream end time is unknowable). OpenAI SDKs auto-retry 429s honoring
  Retry-After, so accuracy matters to avoid retry storms.

### 3.5 Metrics

- New nil-safe counter `aiproxy_account_ratelimit_rejects_total{kind="rate"|"concurrency",
  class="enduser"|"service"}` — bounded vocabulary, **no account_id label** (cardinality
  rule). The class label answers "are the defaults too tight for end users?" straight from
  Prometheus.
- Legacy `aiproxy_ratelimit_rejects_total` keeps counting **key-level** rejects only — the
  two counters partition rejects by gate, no double-counting; the new counter is
  authoritative for account-level throttling.
- Unlabeled gauge `aiproxy_streams_inflight` — answers "is GPU occupancy saturated right
  now" for free once the semaphore exists.

### 3.6 State and deploys

In-memory, single-replica — the documented house assumption (`authlimit.go:8-9`),
restated here. Buckets reset to near-full on every deploy and 10-min idle prune (a free
burst; accepted). RollingUpdate maxSurge briefly runs two pods with independent buckets —
a seconds-long 2× window. **Accepted rather than set `maxSurge: 0`**, which implies
`maxUnavailable: 1`, i.e. brief downtime on every deploy — the wrong trade for a homelab.
Concurrency counters reset *correctly* by construction: in-flight streams die with the pod
(10s graceful shutdown) — a genuine advantage of the semaphore over any persisted design.

Deferred multi-replica path: keep the state behind thin seams
(`Allow(id, limit) (ok, retryAfter)`, `Return(id)`, `TryAcquire(id, cap)`/`Release(id)`);
a v2 Postgres fixed-window counter (`INSERT … ON CONFLICT DO UPDATE … RETURNING`) exists
as a sketch only — note its caveats before anyone adopts it blind: 2× burst at window
boundaries and one DB write per request. Distributed *concurrency* limiting is deliberately
not promised (crash-leaked slots); scaling out requires revisiting this doc, not flipping
`replicas: 2`.

Bucket `capacity` and `refillRate` are already independent fields, so a future
`ACCOUNT_RATELIMIT_BURST` knob is a config addition, not a redesign.

## 4. Config & schema

### 4.1 Env (parsed in `internal/config/config.go`, boot-fail on invalid per the `intEnvOrDefault` house style — silently disabling a security limit is worse than failing to boot)

| Var | Default | Applies to |
|---|---|---|
| `ACCOUNT_RATELIMIT_PER_MIN` | **300** | service accounts (`AllowanceManaged=false`) |
| `END_USER_RATELIMIT_PER_MIN` | **30** | end-user accounts (`AllowanceManaged=true`) |
| `ACCOUNT_MAX_CONCURRENT` | **8** | service accounts, non-GET in-flight |
| `END_USER_MAX_CONCURRENT` | **5** | end-user accounts, non-GET in-flight |

- Range 1..10000 (concurrency 1..100); zero/negative/non-numeric refuse to boot. **No
  `0=disabled` semantic** (that is authlimit's convention, rejected here) — to effectively
  disable, set the max.
- 300 (not robust's 120) for services: generous so existing multi-key accounts mostly
  don't break on day one, but finally bounded. 30 (not the earlier 20) for end users:
  Open WebUI's 2–4 upstream calls per message plus `/models` fetches make 30/min ≈ 7–10
  visible messages/min; 20 ≈ 5–7 and risks breaking background tasks.
- `END_USER_MAX_CONCURRENT=5`, not robust's 3: one visible message = 1 visible stream +
  2–4 parallel background completions; 3 can trip on a *single send*.
- All four surfaced read-only in the admin config view — which requires **all three**
  wiring points or the value stays invisible: `config.go` parse, the ConfigSnapshot
  whitelist (`internal/admin/config_health.go:15-43`), and the literal assignment in
  `cmd/proxy/main.go:239-262`.

### 4.2 Schema (Phase B)

One nullable column, monthly_grant recipe, with minimal's name to avoid the
`api_keys.rate_limit` collision inside the very JOINs we add:

```sql
DO $$ BEGIN
    ALTER TABLE accounts ADD COLUMN rate_limit_per_min INTEGER;
EXCEPTION WHEN duplicate_column THEN NULL; END $$;
```

NULL = class env default. No backfill (NULL is correct initially). No `max_concurrent`
column in v1 — concurrency is env-only until a concrete account needs a bespoke cap.

Override plumbing rides existing queries — no new per-request query paths: join
`accounts.rate_limit_per_min` into `GetKeyByHash` (`store.go:307-322`, service path) and
into the accounts SELECT inside `ResolveEndUserAccount` (`federated.go:163-179`, end-user
path); carry it as `Resolution.RateLimitPerMin *int`. Honest caveat (a judge caught the
"zero plumbing" overstatement): the end-user SELECT lives inside `applyMonthlyAllowance`,
whose signature/return must widen to carry the value out — small but real work, counted in
the estimate. Unlike monthly_grant's next-reset latency, overrides apply on the **next
request** (both carrier queries run per-request; `Allow` rewrites capacity/refill).

### 4.3 Admin API/UI (monthly_grant recipe, cloned end-to-end)

- `PUT /api/admin/accounts/{id}/rate-limit`, body `{"rate_limit_per_min": number|null}`
  via `json.RawMessage`: absent field → 400; explicit null → clear override; value
  validated 1..10000. **Explicit 0 rejected with 400** — resolved against abuse-model's
  `0=blocked` kill-switch: blocking is the job of credits and account deactivation
  (creditGate already 403s inactive accounts), and a 429-based block invites SDK
  auto-retry hammering forever; a third zero-semantic (column-0=blocked vs
  monthly_grant-0=blocked vs validate.go-0=default) is a maintenance trap.
  `RowsAffected==0` → 404. `Store.SetAccountRateLimit` mirrors `SetMonthlyGrant`
  (`federated.go:217-229`).
- `listAccounts` DTO gains `rate_limit_per_min` + `effective_rate_limit_per_min` (handler
  already selects `allowance_managed`; class defaults threaded via admin Options like
  `endUserMonthlyGrant`), both computed through the shared resolver helper.
- FE: `RateLimitDialog` cloned from AllowanceDialog (Apply = override / Use default = null
  / Cancel; "(default)" vs "(override)" marker), rate-limit column in the accounts table,
  `useSetRateLimit` mutation invalidating `qk.accounts.all`, Zod fields, MSW
  handlers + fixtures. Per-key `PUT /api/admin/keys/{id}/rate-limit` stays untouched.

## 5. Alternatives considered & rejected

- **Env-only, defer concurrency entirely (minimal's cut).** Cheapest diff, but it defers
  the only control that bounds GPU occupancy — the stated scarce resource — to an
  appetite question. Rejected; concurrency ships in v1 as its own severable phase, in the
  cheap form (env-only, no column, no FE).
- **Override=0 as abuse kill-switch (abuse-model).** Duplicates the existing account-
  deactivation 403 with the wrong status code; 429 + Retry-After: 60 means SDKs politely
  hammer a blocked account forever, each attempt still paying auth + billing queries
  including the per-request `federated_identities.last_seen_at` write. Rejected.
- **Full `GET /api/v1/models` exemption (abuse-model).** GPU-wise sound (serves from the
  in-memory node snapshot + a pricing SELECT), but it reopens an unthrottled authed path
  doing per-request billing DB work. Rejected for v1; kept token-consuming, slot-exempt
  (§8 Q3).
- **No refunds ("don't engineer refund complexity", abuse-model).** Rests on the false
  premise that key and account buckets share an owner; false exactly for the shared-key
  end-user case this feature exists for. Rejected — see §3.2.
- **`GET /api/admin/ratelimit/hot` top-throttled panel (abuse-model).** Unimplementable
  as specified: reject counts are pruned with the 10-min-*idle* bucket, so the counter is
  cumulative for hot buckets and the abuser vanishes 10 minutes after stopping — exactly
  when you go look. The class-labeled Prometheus counter covers the tuning question;
  per-account visibility deferred until a real need defines retention.
- **Per-account `max_concurrent` column + combined Limits dialog (robust).** Admin
  surface no concrete account has earned. Env-only per-class caps in v1.
- **Postgres fixed-window limiter in v1 (robust's reserved path).** Speculative
  scaffolding for a single-replica deployment. Deferred with caveats labeled (§3.6).
- **Hand-written 429 body instead of apierror (minimal).** Rationale ("SDKs mis-parse
  apierror") is factually wrong — apierror emits the same envelope creditGate already
  uses on `/api/v1/`. Unified on apierror.
- **Skipping the per-key check for AllowanceManaged traffic.** No — it is the only bound
  making forged-identity rotation unprofitable (§3.2).

## 6. Rollout plan

Phases are independently shippable; A+B deliberately land as **one backend PR** so the
override lever exists before the new ceiling bites anyone (resolving minimal's
self-flagged Phase-A gap). Per house rules: branch + PR, images via CI→GHCR, never
scp+build.

- **Phase A+B — backend core + override (M, ~2–2.5d, one PR).** `nowFn` retrofit +
  account Limiter + `Return` + `CheckAll` + chain reorder + env config + ConfigSnapshot
  wiring + metrics; schema column, `GetKeyByHash`/`ResolveEndUserAccount` joins
  (incl. `applyMonthlyAllowance` widening), `Resolution.RateLimitPerMin`, admin PUT +
  `listAccounts` fields, shared resolver helper.
- **Phase C — admin FE (S, ~0.5–1d, one PR).** Dialog, column, hook, Zod, MSW. Can lag A+B.
- **Phase D — concurrency cap (S, ~0.5–1d, one PR).** Keyed semaphore, class env caps,
  outermost-defer Release, slot-exempt GETs, `aiproxy_streams_inflight`. Severable but
  committed to v1.
- **Deploy (S).** Env vars in `deploy/k8s/deployment.yaml`; schema change idempotent-
  guarded, no rollback hazard; buckets reset on deploy (accepted). **Pre-deploy
  grandfathering pass:** enumerate service accounts whose multi-key aggregate exceeds
  300/min and set overrides *before* the ceiling lands, rather than tuning off day-one
  429s. One manual decision: the shared Open WebUI key's per-key limit (§8 Q2). Watch
  `aiproxy_account_ratelimit_rejects_total` for a few days before tightening any default.
  Grafana panel + (later) Discord alerting via the credit-request bot hook — follow-up,
  not v1.

## 7. Test plan

TDD per house rule — tests first, all deterministic via the injected clock (no
`time.Sleep`):

- **Unit (ratelimit):** two-bucket ordering; refund on key-reject; `Return` clamped at
  capacity with a property test (`tokens ≤ capacity` under any interleaving — a wrong
  refund ordering silently doubles effective limits); no refund on concurrency reject;
  Retry-After computed from the rejecting bucket; resolver helper
  (override > class default, both classes).
- **Middleware:** context seeded via `auth.WithKey` + `billing.WithResolution` covering
  end-user vs service defaults, override precedence, missing-Resolution → per-key-only
  fallback (the reachable prod path), nil-key pass-through, both metrics with correct
  `kind`/`class` labels, scope-differentiated messages.
- **Concurrency:** slot released on normal return, client disconnect, and a **panicking
  handler** (a leaked slot silently strangles an account until pod restart); GETs never
  take slots; gauge tracks in-flight.
- **Regression (chain reorder):** over-cap + rate-passing end user still records a
  cap-hit and files the credit request (Discord flow); over-cap + over-rate sees 429;
  audit existing middleware/integration tests assuming 402-before-429 order.
- **Admin:** `setAccountRateLimit` handler tests beside `eua_admin_test.go` (absent/null/
  0/out-of-range/404); `listAccounts` effective values.
- **FE:** dialog + mutation + MSW per the AllowanceDialog test suite; e2e via GitHub
  Actions (local Playwright is unreliable on this machine).

## 8. Open decisions for Krishna

1. **End-user default: 30/min (recommended) vs your earlier 20/min.** 20 ≈ 5–7 visible
   messages/min after Open WebUI's 2–4× amplification, before subtracting `/models`
   polling — tight for real chat. Recommend 30, tune down later from the class-labeled
   reject metric, not before shipping.
2. **Shared Open WebUI key ceiling.** Keep 120/min as the deliberate aggregate backstop,
   or raise now? Recommend raising to ~expected concurrent users × 30 via the existing
   per-key admin endpoint, and a runbook note to scale it with headcount. Do not remove
   it — it is the anti-identity-rotation bound.
3. **`GET /api/v1/models`:** keep consuming rate tokens (recommended — it is an authed
   path doing per-request billing DB work; exemption reopens an unbounded path) vs exempt
   so polling never erodes chat budget. Revisit only if reject metrics show models
   polling causing user-visible 429s.
4. **Chain reorder sign-off:** rateLimit before creditGate changes precedence — an
   over-cap AND over-rate account sees 429 where it saw 402, and cap-hit/Discord
   recording waits for the first rate-passing request. Recommend accepting with the
   regression test; severable if you object.
5. **Grandfathering:** approve the pre-deploy audit + overrides for service accounts
   currently exceeding 300/min aggregate (recommended), vs letting them hit the ceiling
   and tuning reactively.
6. **Concurrency defaults:** 5 end-user / 8 service (recommended; 3 end-user can trip on
   a single Open WebUI send). Also confirm concurrency stays env-only (no column, no FE)
   until an account actually needs a bespoke cap.

# Credit Requests: cap-hit detection + one-click top-ups

Status: accepted (Krishna 2026-07-21: auto-trigger on cap-hit; Discord card offers
top-ups only; v1 includes the admin UI phase)
Author: Claude, reviewed by Krishna

## Problem

When an end-user account exhausts its monthly allowance, every request 402s with
`monthly_limit_reached` and the message "Monthly usage limit reached — resets next
month". That error text rendered in OpenWebUI chat is the user's entire experience,
and it is a dead end: there is no way to ask for more credits, and nothing records
or notifies on cap-hits — the admin never learns someone is blocked unless they
complain out-of-band.

Both remediation levers already exist as admin endpoints, so approval needs no new
billing machinery:

- `POST /api/admin/accounts/{id}/credits` — one-time grant. Allowance accounts are
  reset-to-grant, so a mid-month grant naturally expires at the next monthly reset:
  "extra credits this month only" semantics for free.
- `PUT /api/admin/accounts/{id}/allowance` — permanent monthly grant change
  (deliberately NOT exposed on the Discord card; it stays an admin-console action).

## Decision summary

The **first 402 of a cap-hit episode automatically files a `credit_requests` row**
and fires a webhook to the existing Discord bot, which posts a card with one-click
top-up buttons. The 402 message tells the user the admin has been notified. The
admin console lists requests and gains the (previously missing) allowance editor.

No user action is required or possible: once capped, every API call is rejected, and
there is no portal — so the "request" is filed by the proxy on the user's behalf.
An explicit user-clickable signed link in the 402 message is deferred to v2.

## 1. credit_requests table

```sql
CREATE TABLE IF NOT EXISTS credit_requests (
    id            BIGSERIAL PRIMARY KEY,
    account_id    BIGINT NOT NULL REFERENCES accounts(id),
    period        DATE NOT NULL,        -- first day of the UTC month (allowance_period convention)
    status        TEXT NOT NULL DEFAULT 'pending',   -- pending | granted | dismissed
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at   TIMESTAMPTZ,
    resolved_note TEXT                  -- e.g. '+$5 via discord by myrwin7'
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_requests_pending
    ON credit_requests(account_id, period) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_credit_requests_status ON credit_requests(status, created_at);
```

**Filing policy** (per account, per UTC month):

- No row, or only `granted` rows → cap-hit files a new request. A user who burned
  through a top-up may legitimately need to ask again; each episode gets one card.
- A `pending` row exists → no-op (the partial unique index makes concurrent 402s
  race-safe: one winner inserts, losers hit the index and do nothing).
- A `dismissed` row exists → no-op for the rest of the month. Dismiss means "leave
  me alone about this account this month". Insert uses
  `WHERE NOT EXISTS (... status IN ('pending','dismissed') ...)`; the partial index
  backstops the pending race, and a dismissed row is only ever written by resolution
  (never concurrently with itself), so the NOT EXISTS check suffices for it.

Multiple `granted` rows per month are allowed and form the audit trail.

## 2. Cap-hit detection

Both `monthly_limit_reached` sites call a shared recorder when the billing account
is allowance-managed:

1. `CreditGate` pre-check (`internal/credits/middleware.go`).
2. The reserve-failure path in `handleChatCompletions` (`internal/proxy/proxy.go`).

The recorder runs **async** (goroutine, own context with timeout, errors logged) so
the 402 path stays fast — a capped user retrying in chat produces one indexed
NOT-EXISTS probe per attempt and zero writes after the first.

On a successful insert (the winner), the recorder resolves display metadata
(email/name via `federated_identities`, effective grant, spent ≈ grant − balance)
and fires the notification.

**402 message** for allowance-managed accounts becomes:
"Monthly usage limit reached — your admin has been notified. Credits may be added
shortly; otherwise your allowance resets next month."
(Code stays `monthly_limit_reached`; only the human text changes. The text is
static — it does not re-check request state per 402.)

## 3. Notification webhook

- Config: `CREDIT_ALERT_WEBHOOK_URL` (empty = disabled; requests are still recorded
  and visible in the admin console — Discord is a consumer, not the source of truth).
- Fire-and-forget POST with a short timeout, one retry, then give up and log. A lost
  notification self-heals: the bot reconciles pending requests on startup, and the
  admin console always shows them.
- Payload: `{request_id, account_id, email, display_name, monthly_grant, spent, period}`.
- Prod value: `http://openwebui-discord-bot.openwebui.svc.cluster.local:8080/credit-request`.

## 4. Admin API

- `GET /api/admin/credit-requests?status=pending|granted|dismissed&limit&offset` —
  list envelope; rows join account name, federated email, effective monthly grant,
  current balance. Default filter: `pending`.
- `PUT /api/admin/credit-requests/{id}` body `{"status": "granted"|"dismissed", "note"?: string}` —
  valid only from `pending` (409 `already_resolved` otherwise). Resolution does NOT
  move money itself; the caller grants credits first (existing endpoint), then marks
  the request. Keeping the two steps separate reuses the audited grant path and
  keeps this endpoint trivial.
- `GET /api/admin/accounts` rows gain `effective_monthly_grant` (the env default
  resolved server-side) so the UI never hardcodes the default.

All responses keep the `{data: ...}` envelope convention.

## 5. Discord bot (BOT-2)

Extends `local-ai-chat/discord-approval-bot/` (same deployment):

- New explicit route `POST /credit-request` (registered before the catch-all
  signup-webhook route) → card in the existing channel:
  title "💳 Credit request", fields Name / Email / Grant / Spent this month,
  footer marker `credit-req:<request_id>` (restart-dedupe via channel history scan,
  same pattern as signups).
- Buttons: **+$1 this month**, **+$5 this month**, **Dismiss**. Allow-list enforced
  exactly like signup buttons. No permanent-raise button (decision above).
- Action flow: top-up → `POST /api/admin/accounts/{account_id}/credits`
  `{amount, description: "credit-request top-up via discord"}` →
  `PUT /api/admin/credit-requests/{id}` `{status:"granted", note:"+$N via discord by <user>"}` →
  edit card to "✅ +$N by <name>". Dismiss → PUT dismissed → "⛔ Dismissed by <name>".
  If the request was already resolved elsewhere (admin console), the PUT 409s and
  the card is edited to reflect that instead of erroring.
- Startup reconcile: `GET /api/admin/credit-requests?status=pending` → announce any
  request whose marker is absent from recent channel history.
- New env/secrets: `PROXY_ADMIN_URL` (`http://ai-proxy.local-ai.svc.cluster.local`),
  `PROXY_ADMIN_KEY` (a dedicated admin key minted for the bot, stored in a k8s
  secret + the repo-external secrets dir, per convention). Admin rate limit
  (10 req/min) is far above bot traffic.

## 6. Admin console (FE-3, folds in the FE-4 allowance-editor remainder)

Accounts page:

- Allowance-managed rows show monthly grant (marking the env default, via
  `effective_monthly_grant`) and current balance.
- Inline allowance editor dialog → `PUT /api/admin/accounts/{id}/allowance`
  (endpoint has had no UI since EUA shipped).
- Pending credit requests surface as a highlighted strip above the table when any
  exist: email, spent/grant, per-row **Top up** (opens the existing
  GrantCreditsDialog, then marks the request granted) and **Dismiss**.

## 7. Rollout (order matters)

1. Merge + deploy backend (schema idempotent-on-boot). `CREDIT_ALERT_WEBHOOK_URL`
   unset → detection + admin API live, no Discord traffic.
2. Mint the bot's admin key; add secret; deploy the extended bot.
3. Set `CREDIT_ALERT_WEBHOOK_URL` on the proxy deployment and roll it.
4. Verify end-to-end with a low-grant test account: chat past the cap → card
   appears → +$1 → chat works again → request marked granted.
5. Deploy admin-frontend (FE-3) independently.

Rollback: unset the webhook URL (detection keeps recording silently); the table and
endpoints are inert without traffic.

## Non-goals / v2

- Explicit user-clickable request link (signed token in the 402 message).
- Auto-approval policy (e.g., first +$1/month granted automatically).
- Email notifications; user-visible balance (needs the portal).
- Changing allowance semantics — reset-to-grant stands.

# End-User Accounts: per-user attribution + monthly allowance

Status: accepted (Krishna 2026-07-21: full per-user accounts, monthly $ cap, backend first)
Author: Claude, reviewed by Krishna (pending)

## Problem

All chat.ai.skpodduturi.dev traffic reaches the proxy on one shared OpenWebUI API key
billed to the `admin-service` account. Usage is a single undifferentiated blob: no
per-user visibility in analytics, and no way to stop one end user from consuming the
entire shared balance.

## Decision summary

Each OpenWebUI user gets a **real proxy account**, auto-provisioned on first request,
with its **own credit balance** funded by a **monthly allowance** (reset, no rollover).
The existing reserve/settle ledger enforces the cap with no new enforcement machinery:
`balance` can never go below zero, and the balance is re-granted monthly — so
spend-per-month ≤ allowance, structurally.

## 1. Trusted identity forwarding

- `api_keys.trust_user_headers BOOLEAN NOT NULL DEFAULT FALSE` — settable only via the
  admin API (`PUT /api/admin/keys/{id}`). Exactly one key (OpenWebUI's) gets it in prod.
- On `POST /api/v1/chat/completions`, when the authenticated key has
  `trust_user_headers` and the request carries `X-OpenWebUI-User-Id`, the proxy bills
  the **end-user account** resolved from that identity instead of the key's account.
  `X-OpenWebUI-User-Email` / `X-OpenWebUI-User-Name` are captured as display metadata.
- Headers on a **non-trusted** key are ignored entirely (billing unchanged, debug log).
  A trusted key with **no headers** bills its own account, unchanged — this also covers
  OpenWebUI's internal task requests and any direct curl use of the shared key.

Security invariant: attribution can never be *claimed* by a client; it is only
*granted* by the admin marking a specific key as trusted. A spoofed header on any other
key is inert.

## 2. Federated identities + auto-provisioning

New table:

```sql
CREATE TABLE IF NOT EXISTS federated_identities (
    id           BIGSERIAL PRIMARY KEY,
    source       TEXT NOT NULL,              -- 'openwebui' (TEXT, not enum: future sources)
    external_id  TEXT NOT NULL,              -- OpenWebUI user id (authority for identity)
    account_id   BIGINT NOT NULL REFERENCES accounts(id),
    email        TEXT,                       -- display metadata, refreshed when it changes
    display_name TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source, external_id)
);
```

First sight of an identity — all four inserts in ONE transaction. Concurrency: the
identity insert uses `ON CONFLICT (source, external_id) DO NOTHING RETURNING id`; a
loser (no row returned) **rolls back its whole transaction** — discarding its
provisional account, balance, and registration event — and adopts the winner's
account via re-select. No advisory lock needed; the UNIQUE constraint elects the
winner and the rollback guarantees no orphans (race-tested, including
no-orphan-accounts assertions):

1. `accounts` row: `type='end_user'`, `name` = email (fallback: display name, then
   `openwebui:<external_id>`), `allowance_managed=TRUE`.
2. `credit_balances` row initialized to 0 (the allowance top-up below funds it in the
   same request).
3. `federated_identities` row.
4. `registration_events` row: `kind='user'`, `source='trusted_header'`,
   metadata `{source: 'openwebui', external_id, email}`.

Identity authority is `external_id`, not email: an email change in OpenWebUI updates
the metadata but keeps the same account (and its history/balance).

## 3. Monthly allowance

New columns:

```sql
-- accounts
allowance_managed BOOLEAN NOT NULL DEFAULT FALSE,  -- only auto-provisioned end-user accounts
monthly_grant     DECIMAL(15,6),                   -- NULL = use env default

-- credit_balances
allowance_period  DATE,                            -- first day of the last granted month (UTC)
```

- Config: `END_USER_MONTHLY_GRANT` (credits ≡ USD, default **5.00**). Per-account
  override via `accounts.monthly_grant` (explicit `0` = blocked; no "unlimited"
  sentinel — set a large value instead).
- **Lazy top-up, no cron**: on the billing path, before reserving, if the billing
  account is `allowance_managed` and `allowance_period` ≠ current UTC month:

```sql
UPDATE credit_balances
SET balance = $grant, allowance_period = $month, updated_at = NOW()
WHERE account_id = $id
  AND (allowance_period IS NULL OR allowance_period < $month);
```

  The `WHERE` guard makes concurrent first-requests-of-the-month race-safe (single
  winner; losers see rows-affected 0 and proceed). A `credit_transactions` row
  (`type='monthly_allowance'`, `balance_after=$grant`) keeps the audit trail honest.
- Reset semantics (not `+=`): unspent allowance does **not** roll over, so monthly
  spend is bounded by the grant. `reserved` (in-flight holds) is intentionally left
  untouched; holds settle against the fresh balance as usual.
- Non-allowance accounts (`allowance_managed=FALSE`, i.e. every account that exists
  today) are completely unaffected.

### Cap softness (accepted, documented)

The cap is enforced at reserve time; settlement subtracts the **actual** cost even
when it exceeds the estimate, so a balance can go slightly negative on the last
request of the month. The overrun is bounded by a single response (~cents at current
pricing: a full 100k-token completion on gemma4:e4b is $0.04). The next monthly reset
normalizes it. Revisit with hard mid-stream enforcement only if per-request costs grow
orders of magnitude.

Two related edges, same acceptance rationale:
- A pending hold that crosses the month boundary settles against the fresh grant
  (seconds-long window; stale holds are already swept).
- Manual `AddCredits` to an allowance-managed account does **not** survive the next
  monthly reset (reset-to-grant semantics). To give someone more headroom, raise
  `monthly_grant` — don't hand-grant credits.

## 4. Proxy billing resolution

The live chain is `authMiddleware(creditGate(proxyHandler))`, and `CreditGate`
pre-checks the **key's** account — so resolution cannot live inside the handler or
the gate would reject end users based on the shared account's state (and end-user
402s would never reference their own balance).

Instead, a dedicated **billing resolution middleware** sits between auth and the
gate:

```
authMiddleware( billingResolver( creditGate( proxyHandler )))
```

`billingResolver`: when the context key has `trust_user_headers` and
`X-OpenWebUI-User-Id` is present, call `ResolveEndUserAccount` (provision + allowance
top-up) and stash `{AccountID, AllowanceManaged}` in the request context; otherwise
stash the key's own account. `CreditGate` and `handleChatCompletions` consume the
stashed resolution instead of `key.AccountID` — the gate's active/balance pre-check,
usage-stats estimation, `ReserveCredits`, settle attribution, and the usage-log row
all follow the billing account. The per-key session token limit stays per-KEY (it
protects the credential; the allowance protects the person).

402 semantics: when the billing account is allowance-managed, both the gate's
pre-check and the reserve failure return `monthly_limit_reached` ("Monthly usage
limit reached — resets next month") instead of `insufficient_credits`.

## 5. usage_logs attribution

```sql
-- schema.sql re-runs in full on every boot: the ADD COLUMN is wrapped in the
-- repo's duplicate_column guard and the index uses IF NOT EXISTS.
DO $$ BEGIN
    ALTER TABLE usage_logs ADD COLUMN account_id BIGINT REFERENCES accounts(id);  -- NULL = historical
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;
CREATE INDEX IF NOT EXISTS idx_usage_logs_account_created ON usage_logs(account_id, created_at);
```

Written on every insert with the billing account. Analytics read
`COALESCE(usage_logs.account_id, api_keys.account_id)` (join fallback for pre-feature
rows). All four analytics endpoints gain an `account_id` filter, mirroring the
`node_id` filter added in BE-9.

## 6. Admin API surface

- `PUT /api/admin/keys/{id}`: new `trust_user_headers` field (absent=keep, bool=set).
- `GET /api/admin/accounts`: rows gain `type`, `allowance_managed`, `monthly_grant`,
  and the federated `email` when present; optional `?type=end_user` filter.
- `PUT /api/admin/accounts/{id}/allowance`: body `{"monthly_grant": 12.5 | null}`
  (null → revert to env default).
- Usage analytics endpoints: `account_id` query filter.

All responses keep the `{data: ...}` envelope convention.

## 7. Rollout (order matters)

1. Merge + deploy backend (schema is idempotent-on-boot per repo convention).
2. Mark the OpenWebUI key trusted via admin API. Nothing changes yet (no headers).
3. Set `ENABLE_FORWARD_USER_INFO_HEADERS=true` on the OpenWebUI deployment
   (`local-ai-chat/openwebui.yaml`) and roll it.
4. Verify: chat once per test user → accounts auto-created, usage attributed, allowance
   transactions present; direct-key curl still bills admin-service.

Rollback: unset the env var (headers stop, everything bills the key account again);
schema additions are inert without headers.

## Non-goals

- No OpenWebUI⇄proxy credential/account sync (rejected 2026-07-09, stands).
- No per-end-user proxy API keys.
- Admin-console UI for end-user usage/allowances: follow-up FE task.
- `/v1/embeddings` routing: unchanged, separate task.

CREATE TABLE IF NOT EXISTS api_keys (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    key_hash    TEXT NOT NULL UNIQUE,
    key_prefix  TEXT NOT NULL,
    rate_limit  INTEGER NOT NULL DEFAULT 60,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked     BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS usage_logs (
    id                BIGSERIAL PRIMARY KEY,
    api_key_id        BIGINT NOT NULL REFERENCES api_keys(id),
    model             TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    duration_ms       BIGINT NOT NULL DEFAULT 0,
    status            TEXT NOT NULL DEFAULT 'completed',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_usage_logs_key_id ON usage_logs(api_key_id);
CREATE INDEX IF NOT EXISTS idx_usage_logs_created ON usage_logs(created_at);

CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    name          TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user',
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS user_sessions (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id),
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Add user_id to api_keys (NULL = legacy admin-created key)
DO $$ BEGIN
    ALTER TABLE api_keys ADD COLUMN user_id BIGINT REFERENCES users(id);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- Accounts: universal tenant for credits
CREATE TABLE IF NOT EXISTS accounts (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    type       TEXT NOT NULL DEFAULT 'personal',
    is_active  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Link users to accounts
DO $$ BEGIN
    ALTER TABLE users ADD COLUMN account_id BIGINT REFERENCES accounts(id);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- Link API keys to accounts (billing ownership)
DO $$ BEGIN
    ALTER TABLE api_keys ADD COLUMN account_id BIGINT REFERENCES accounts(id);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- Per-key session token limit (6hr sliding window)
DO $$ BEGIN
    ALTER TABLE api_keys ADD COLUMN session_token_limit INTEGER;
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- Credit balances per account
CREATE TABLE IF NOT EXISTS credit_balances (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id),
    balance    DECIMAL(15,6) NOT NULL DEFAULT 0,
    reserved   DECIMAL(15,6) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Audit trail for all credit changes
CREATE TABLE IF NOT EXISTS credit_transactions (
    id            BIGSERIAL PRIMARY KEY,
    account_id    BIGINT NOT NULL REFERENCES accounts(id),
    amount        DECIMAL(15,6) NOT NULL,
    balance_after DECIMAL(15,6) NOT NULL,
    type          TEXT NOT NULL,
    reference_id  BIGINT,
    description   TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credit_transactions_account
    ON credit_transactions(account_id, created_at);

-- Per-model pricing
CREATE TABLE IF NOT EXISTS credit_pricing (
    id                 BIGSERIAL PRIMARY KEY,
    model_id           TEXT NOT NULL UNIQUE,
    prompt_rate        DECIMAL(15,10) NOT NULL,
    completion_rate    DECIMAL(15,10) NOT NULL,
    typical_completion INTEGER NOT NULL DEFAULT 500,
    effective_from     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    active             BOOLEAN NOT NULL DEFAULT TRUE
);

-- Credit holds for reserve/settle flow
CREATE TABLE IF NOT EXISTS credit_holds (
    id          BIGSERIAL PRIMARY KEY,
    account_id  BIGINT NOT NULL REFERENCES accounts(id),
    amount      DECIMAL(15,6) NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_credit_holds_pending
    ON credit_holds(status, created_at) WHERE status = 'pending';

-- Historical usage averages for reserve estimation
CREATE TABLE IF NOT EXISTS account_usage_stats (
    account_id            BIGINT NOT NULL,
    model                 TEXT NOT NULL,
    avg_completion_tokens INTEGER NOT NULL DEFAULT 0,
    request_count         INTEGER NOT NULL DEFAULT 0,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (account_id, model)
);

-- Registration tokens for service self-provisioning
CREATE TABLE IF NOT EXISTS registration_tokens (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    credit_grant DECIMAL(15,6) NOT NULL DEFAULT 0,
    max_uses     INTEGER NOT NULL DEFAULT 1,
    uses         INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ,
    revoked      BOOLEAN NOT NULL DEFAULT FALSE
);

-- Composite index for session limit sliding window queries
CREATE INDEX IF NOT EXISTS idx_usage_logs_key_created
    ON usage_logs(api_key_id, created_at);

-- Registration audit trail. Source values (at time of writing):
--   'public_signup'        — new user via POST /api/auth/register
--   'registration_token'   — service account via POST /api/accounts/register
--   'admin_bootstrap'      — first admin or DR admin via /api/admin/bootstrap
--   'admin_create'         — admin-created user via POST /api/admin/users (future)
--   'admin_service'        — the auto-created "admin-service" account (OSS-2)
--   'backfill'             — rows inserted by the PR 1 backfill for historical data
-- The column is TEXT (not an enum) so new sources don't require a migration.
CREATE TABLE IF NOT EXISTS registration_events (
    id                      BIGSERIAL PRIMARY KEY,
    kind                    TEXT NOT NULL,               -- 'user' | 'service'
    account_id              BIGINT REFERENCES accounts(id),
    user_id                 BIGINT REFERENCES users(id),
    registration_token_id   BIGINT REFERENCES registration_tokens(id),
    source                  TEXT NOT NULL,
    metadata                JSONB,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_registration_events_created
    ON registration_events(created_at);

-- Analytics indexes (PR 1)
CREATE INDEX IF NOT EXISTS idx_usage_logs_model_created
    ON usage_logs(model, created_at);
CREATE INDEX IF NOT EXISTS idx_api_keys_account_id
    ON api_keys(account_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_user_id
    ON api_keys(user_id);

-- Per-request credit cost on usage_logs (historical rows default 0).
DO $$ BEGIN
    ALTER TABLE usage_logs ADD COLUMN credits_charged DECIMAL(15,6) NOT NULL DEFAULT 0;
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- Distributed backend nodes (inference backends the gateway routes to).
-- Rows are never hard-deleted: usage_logs references them; DELETE on the
-- admin API maps to enabled=FALSE (repo soft-delete convention).
CREATE TABLE IF NOT EXISTS nodes (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    base_url      TEXT NOT NULL,
    backend_type  TEXT NOT NULL DEFAULT 'ollama'
                  CHECK (backend_type IN ('ollama', 'openai_compat')),
    auth_header   TEXT,            -- optional Authorization value sent to the node
    static_models TEXT[],          -- non-NULL disables model discovery; exact list
    health_path   TEXT,            -- optional liveness-probe path override
    timeout_seconds INTEGER        -- optional per-node request timeout (NULL = default)
                  CHECK (timeout_seconds IS NULL OR timeout_seconds > 0),
    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    source        TEXT NOT NULL DEFAULT 'api'
                  CHECK (source IN ('api', 'config')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Node attribution on usage logs (historical rows stay NULL)
DO $$ BEGIN
    ALTER TABLE usage_logs ADD COLUMN node_id BIGINT REFERENCES nodes(id);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_usage_logs_node_created
    ON usage_logs(node_id, created_at);

-- ---------------------------------------------------------------------------
-- Pricing re-denomination (OSS-5): canonical rates are credits per MILLION
-- tokens (per-MTok, the industry convention). Charge math everywhere is
--   cost = tokens * rate_mtok / 1_000_000
-- and application code reads/writes ONLY the *_mtok columns below.
--
-- The old per-token columns (prompt_rate, completion_rate) are DEPRECATED:
-- application writes keep them in sync (= rate_mtok / 1e6, rounded to their
-- 10-decimal scale) purely so a rolled-back binary still reads correct
-- prices during the transition. They will be DROPPED in a later release.
--
-- Idempotency (production-critical): this file is re-executed in full on
-- EVERY boot. The duplicate_column guards make the ADD COLUMNs no-ops after
-- the first run, and the backfill only touches rows whose *_mtok value is
-- still NULL — so re-running the schema can never multiply an
-- already-converted rate again, and never clobbers a rate later re-priced
-- through the per-MTok admin API. The x1e6 backfill itself is lossless:
-- DECIMAL(15,10) * 1e6 needs at most 4 fractional digits, which
-- DECIMAL(15,6) stores exactly. Pinned by TestRedenomination_MigrationIdempotent
-- and TestRedenomination_EqualCostProof.
DO $$ BEGIN
    ALTER TABLE credit_pricing ADD COLUMN prompt_rate_mtok DECIMAL(15,6);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE credit_pricing ADD COLUMN completion_rate_mtok DECIMAL(15,6);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

UPDATE credit_pricing SET prompt_rate_mtok = prompt_rate * 1000000
    WHERE prompt_rate_mtok IS NULL;
UPDATE credit_pricing SET completion_rate_mtok = completion_rate * 1000000
    WHERE completion_rate_mtok IS NULL;

-- ---------------------------------------------------------------------------
-- End-user accounts (docs/design/end-user-accounts.md): per-user attribution
-- and monthly allowance for identities forwarded on trusted keys.

-- Only the admin API may set this; a key with the flag bills the end-user
-- account resolved from X-OpenWebUI-User-Id instead of its own account.
DO $$ BEGIN
    ALTER TABLE api_keys ADD COLUMN trust_user_headers BOOLEAN NOT NULL DEFAULT FALSE;
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- allowance_managed marks auto-provisioned end-user accounts: only these get
-- the monthly balance reset. monthly_grant NULL = use the env default
-- (END_USER_MONTHLY_GRANT); explicit 0 = blocked.
DO $$ BEGIN
    ALTER TABLE accounts ADD COLUMN allowance_managed BOOLEAN NOT NULL DEFAULT FALSE;
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

DO $$ BEGIN
    ALTER TABLE accounts ADD COLUMN monthly_grant DECIMAL(15,6);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- First day (UTC) of the month the allowance was last granted for.
DO $$ BEGIN
    ALTER TABLE credit_balances ADD COLUMN allowance_period DATE;
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

-- Identity authority is (source, external_id) — email is display metadata and
-- may change without moving the account.
CREATE TABLE IF NOT EXISTS federated_identities (
    id           BIGSERIAL PRIMARY KEY,
    source       TEXT NOT NULL,
    external_id  TEXT NOT NULL,
    account_id   BIGINT NOT NULL REFERENCES accounts(id),
    email        TEXT,
    display_name TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source, external_id)
);

-- Billing-account attribution on usage rows (NULL = pre-feature history;
-- analytics COALESCE through the api_keys join for those).
DO $$ BEGIN
    ALTER TABLE usage_logs ADD COLUMN account_id BIGINT REFERENCES accounts(id);
EXCEPTION WHEN duplicate_column THEN
    NULL;
END $$;

CREATE INDEX IF NOT EXISTS idx_usage_logs_account_created
    ON usage_logs(account_id, created_at);

-- The admin accounts listing resolves each account's display email via a
-- correlated per-account lookup; this index keeps that O(log n) per row.
CREATE INDEX IF NOT EXISTS idx_federated_identities_account
    ON federated_identities(account_id, id);

-- Same shape for the personal-account fallback in usage-by-account: the
-- per-account owning-user email lookup filters users on account_id.
CREATE INDEX IF NOT EXISTS idx_users_account
    ON users(account_id, id);

-- ---------------------------------------------------------------------------
-- Credit requests (docs/design/credit-requests.md): auto-filed on the first
-- monthly_limit_reached 402 of a cap-hit episode. Filing policy per
-- (account, UTC month): a pending row dedupes, a dismissed row silences the
-- rest of the month, granted rows allow re-filing (each episode gets one).

CREATE TABLE IF NOT EXISTS credit_requests (
    id            BIGSERIAL PRIMARY KEY,
    account_id    BIGINT NOT NULL REFERENCES accounts(id),
    period        DATE NOT NULL,      -- first day of the UTC month, like allowance_period
    status        TEXT NOT NULL DEFAULT 'pending',  -- pending | granted | dismissed
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at   TIMESTAMPTZ,
    resolved_note TEXT
);

-- Race-safety for concurrent 402s: only one pending row can exist per
-- account+month, so the losing inserter's ON CONFLICT no-ops.
CREATE UNIQUE INDEX IF NOT EXISTS idx_credit_requests_pending
    ON credit_requests(account_id, period) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_credit_requests_status
    ON credit_requests(status, created_at);

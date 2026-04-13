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

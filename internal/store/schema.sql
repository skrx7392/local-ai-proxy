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

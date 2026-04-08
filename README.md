# local-ai-proxy

An OpenAI-compatible reverse proxy to Ollama with API key authentication, per-key rate limiting, usage tracking, and user management. Deployed to k3s at `ai.kinvee.in/api`. All routes are under the `/api` prefix, leaving `ai.kinvee.in/` free for the frontend.

## Architecture

```
Client → RequestID → CORS → Auth → CreditGate → RateLimit → Proxy → Ollama
                                                                ↓
                                                    Async Usage Logger → PostgreSQL
```

Internal packages: `config`, `auth`, `store`, `ratelimit`, `proxy`, `middleware`, `admin`, `user`, `credits`, `logging`, `requestid`, `health`, `metrics`, `apierror` — all using stdlib `net/http`, no frameworks.

## Endpoints

### Proxy (authenticated via `Authorization: Bearer <api-key>`)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/chat/completions` | Proxied to Ollama with usage tracking (streaming + non-streaming) |
| `GET` | `/api/v1/models` | Lists models with active pricing |

### Health & Observability

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/healthz/live` | Liveness probe — always 200 |
| `GET` | `/api/healthz/ready` | Readiness probe — checks DB, Ollama, usage writer |
| `GET` | `/api/healthz` | Alias for `/api/healthz/live` (backward compat) |
| `GET` | `/metrics` | Prometheus metrics endpoint |

### Auth API (public + session-authenticated)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/auth/register` | Register a new user account |
| `POST` | `/api/auth/login` | Login and receive a session token |
| `POST` | `/api/auth/logout` | Invalidate session (session-authenticated) |

### User API (session-authenticated via `Authorization: Bearer <session-token>` or `X-Session-Token`)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/users/profile` | Get user profile |
| `PUT` | `/api/users/profile` | Update user profile |
| `PUT` | `/api/users/password` | Change password |
| `POST` | `/api/users/keys` | Create user API key |
| `GET` | `/api/users/keys` | List user's API keys |
| `DELETE` | `/api/users/keys/{id}` | Revoke user's API key |
| `GET` | `/api/users/usage` | Get usage stats for user's keys |

### Admin (authenticated via `X-Admin-Key` header, rate limited to 10 req/min)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/keys` | Create API key (`{name, rate_limit}`) — returns full key once, never retrievable again |
| `GET` | `/api/admin/keys` | List all keys (id, name, key_prefix, rate_limit, created_at, revoked) |
| `DELETE` | `/api/admin/keys/{id}` | Revoke (soft-delete) a key |
| `GET` | `/api/admin/usage` | Aggregated usage stats (filterable by `key_id` and `since`) |
| `GET` | `/api/admin/users` | List all users |
| `PUT` | `/api/admin/users/{id}/activate` | Activate a user account |
| `PUT` | `/api/admin/users/{id}/deactivate` | Deactivate a user account |

## Request Flow

### Non-Streaming (`stream=false` or omitted)

1. Request passes through CORS, auth, and rate limit middleware
2. Request body read into memory (capped by `MAX_REQUEST_BODY`)
3. Body peeked to extract model name and stream flag
4. Reverse proxy forwards to Ollama
5. `ModifyResponse` hook intercepts response, parses JSON for token usage
6. Usage entry sent to async channel (non-blocking; drops if buffer full)
7. Response written to client unchanged

### Streaming (`stream=true`)

1. Request passes through middleware, body peeked for model name
2. Direct HTTP connection established to Ollama
3. Response streamed line-by-line via `bufio.Reader`
4. Each `data: {...}` line observed for usage object (non-destructive parsing)
5. Lines flushed to client immediately for SSE delivery
6. On EOF/error, status logged as completed, partial, or error
7. Usage entry sent to async channel

### Async Usage Logging

All requests write to a buffered channel (capacity 1000). A dedicated goroutine drains the channel and calls `store.LogUsage()`. On shutdown, the channel is closed and remaining entries are drained.

## Database

PostgreSQL via pgx/v5 connection pool. Schema auto-migrated on startup via embedded SQL.

### Schema

```sql
api_keys (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL UNIQUE,
  key_prefix  TEXT NOT NULL,
  rate_limit  INTEGER NOT NULL DEFAULT 60,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  revoked     BOOLEAN NOT NULL DEFAULT FALSE,
  user_id     BIGINT REFERENCES users(id)  -- NULL = legacy admin-created key
)

usage_logs (
  id                BIGSERIAL PRIMARY KEY,
  api_key_id        BIGINT NOT NULL REFERENCES api_keys(id),
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  duration_ms       BIGINT NOT NULL DEFAULT 0,
  status            TEXT NOT NULL DEFAULT 'completed',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
)

users (
  id            BIGSERIAL PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  name          TEXT NOT NULL,
  role          TEXT NOT NULL DEFAULT 'user',
  is_active     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
)

user_sessions (
  id         BIGSERIAL PRIMARY KEY,
  user_id    BIGINT NOT NULL REFERENCES users(id),
  token_hash TEXT NOT NULL UNIQUE,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
```

## Configuration

All configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `ADMIN_KEY` | *(required)* | Admin API authentication key |
| `DATABASE_URL` | *(required)* | PostgreSQL connection string |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama backend URL |
| `PORT` | `8080` | Server listen port |
| `CORS_ORIGINS` | `*` | Allowed CORS origins |
| `MAX_REQUEST_BODY` | `52428800` (50MB) | Max request body size in bytes |
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |

## Deployment

Multi-stage Docker build (`deploy/Dockerfile`) with k8s manifests in `deploy/k8s/`. CI/CD pipeline: GitHub Actions → Tailscale → SSH to dev server → Docker build → k3s rollout.

```bash
# Build
CGO_ENABLED=0 go build -ldflags="-s -w" -o proxy ./cmd/proxy

# Run
ADMIN_KEY=your-key DATABASE_URL=postgres://... ./proxy

# Docker
docker build -f deploy/Dockerfile -t ai-proxy .
```

## Strengths

- Clean package separation with proper middleware chaining
- Async usage logging via buffered channel — non-blocking to requests
- Streaming SSE support with line-by-line token extraction
- Auth strips Bearer token before forwarding to Ollama (no key leakage)
- User registration and session management
- Full k3s deployment pipeline
- Test coverage across all packages

## Known Gaps

| Area | Issue |
|------|-------|
| Endpoints | Only chat completions + models; no embeddings, completions, or images |
| Rate limiting | In-memory only — resets on restart |
| Validation | Request body forwarded as-is, no schema checks |
| Scale | Single replica, no multi-backend support |
| Storage | No backup strategy, soft-delete only (unbounded growth) |
| Streaming | Token extraction is fragile — silently fails if Ollama changes SSE format |

## Observability

### Structured Logging

All logs are JSON to stdout via `slog`, collected by Alloy and shipped to Loki. Every log entry within an HTTP request includes `request_id` automatically via a context-aware slog handler.

### Request IDs

Every request gets an `X-Request-ID` header (generated as `req_` + 32 hex chars, or reuses a valid incoming header). The ID appears in response headers and error response JSON bodies:

```json
{"error":{"message":"...","type":"...","code":"..."},"request_id":"req_abc123..."}
```

### Prometheus Metrics

Available at `GET /metrics`. Key metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `aiproxy_request_duration_seconds` | Histogram | HTTP request latency |
| `aiproxy_requests_total` | Counter | Total HTTP requests |
| `aiproxy_tokens_total` | Counter | Tokens processed (by model, prompt/completion) |
| `aiproxy_credit_gate_rejects_total` | Counter | Requests rejected by credit gate |
| `aiproxy_ratelimit_rejects_total` | Counter | Requests rejected by rate limiter |
| `aiproxy_usage_channel_depth` | Gauge | Async usage channel depth |
| `aiproxy_ollama_up` | Gauge | Ollama reachability (updated on readiness check) |

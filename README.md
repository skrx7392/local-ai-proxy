# local-ai-proxy

An OpenAI-compatible reverse proxy to Ollama with API key authentication, per-key rate limiting, usage tracking, and user management. Deployed to k3s at `ai.kinvee.in/api`.

## Architecture

```
Client → CORS → Auth → Rate Limit → Proxy → Ollama
                                       ↓
                              Async Usage Logger → PostgreSQL
```

8 internal packages (`config`, `auth`, `store`, `ratelimit`, `proxy`, `middleware`, `admin`, `user`) — all using stdlib `net/http`, no frameworks.

## Endpoints

### Proxy (authenticated via `Authorization: Bearer <api-key>`)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/chat/completions` | Proxied to Ollama with usage tracking (streaming + non-streaming) |
| `GET` | `/v1/models` | Passthrough to Ollama |
| `GET` | `/healthz` | Liveness/readiness probe |

### User API (session-authenticated via `Authorization: Bearer <session-token>` or `X-Session-Token`)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/register` | Register a new user account |
| `POST` | `/api/login` | Login and receive a session token |

### Admin (authenticated via `X-Admin-Key` header, rate limited to 10 req/min)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/admin/keys` | Create API key (`{name, rate_limit}`) — returns full key once, never retrievable again |
| `GET` | `/admin/keys` | List all keys (id, name, key_prefix, rate_limit, created_at, revoked) |
| `DELETE` | `/admin/keys/{id}` | Revoke (soft-delete) a key |
| `GET` | `/admin/usage` | Aggregated usage stats (filterable by `key_id` and `since`) |
| `GET` | `/admin/users` | List all users |
| `PUT` | `/admin/users/{id}/activate` | Activate a user account |
| `PUT` | `/admin/users/{id}/deactivate` | Deactivate a user account |

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
| Observability | No structured logging, no metrics, no request IDs |
| Rate limiting | In-memory only — resets on restart |
| Validation | Request body forwarded as-is, no schema checks |
| Scale | Single replica, no multi-backend support |
| Storage | No backup strategy, soft-delete only (unbounded growth) |
| Streaming | Token extraction is fragile — silently fails if Ollama changes SSE format |

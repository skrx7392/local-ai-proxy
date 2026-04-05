# local-ai-proxy

An OpenAI-compatible reverse proxy to Ollama with API key authentication, per-key rate limiting, and usage tracking. Deployed to k3s at `ai.kinvee.in`.

## Architecture

```
Client → CORS → Auth → Rate Limit → Proxy → Ollama
                                       ↓
                              Async Usage Logger → SQLite
```

6 internal packages (`config`, `auth`, `store`, `ratelimit`, `proxy`, `middleware`, `admin`) — all using stdlib `net/http`, no frameworks.

## Endpoints

### Proxy

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/chat/completions` | Proxied to Ollama with usage tracking (streaming + non-streaming) |
| `GET` | `/v1/models` | Passthrough to Ollama |
| `GET` | `/healthz` | Liveness/readiness probe |

### Admin (authenticated via `X-Admin-Key` header, rate limited to 10 req/min)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/admin/keys` | Create API key (`{name, rate_limit}`) — returns full key once, never retrievable again |
| `GET` | `/admin/keys` | List all keys (id, name, key_prefix, rate_limit, created_at, revoked) |
| `DELETE` | `/admin/keys/{id}` | Revoke (soft-delete) a key |
| `GET` | `/admin/usage?key_id=&since=` | Aggregated usage stats (requests, tokens by model/key/status) |

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

SQLite with WAL mode, dual-connection architecture (read pool of 4, write singleton of 1).

### Schema

```sql
api_keys (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL UNIQUE,
  key_prefix  TEXT NOT NULL,
  rate_limit  INTEGER NOT NULL DEFAULT 60,
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  revoked     INTEGER NOT NULL DEFAULT 0
)

usage_logs (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  api_key_id        INTEGER NOT NULL REFERENCES api_keys(id),
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  duration_ms       INTEGER NOT NULL DEFAULT 0,
  status            TEXT NOT NULL DEFAULT 'completed',
  created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)
```

## Configuration

All configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `ADMIN_KEY` | *(required)* | Admin API authentication key |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama backend URL |
| `PORT` | `8080` | Server listen port |
| `DB_PATH` | `./proxy.db` | SQLite database file path |
| `CORS_ORIGINS` | `*` | Allowed CORS origins |
| `MAX_REQUEST_BODY` | `52428800` (50MB) | Max request body size in bytes |

## Deployment

Multi-stage Docker build (`deploy/Dockerfile`) with k8s manifests in `deploy/k8s/`. CI/CD pipeline: GitHub Actions → Tailscale → SSH to dev server → Docker build → k3s rollout.

```bash
# Build
CGO_ENABLED=0 go build -ldflags="-s -w" -o proxy ./cmd/proxy

# Run
ADMIN_KEY=your-key ./proxy

# Docker
docker build -f deploy/Dockerfile -t ai-proxy .
```

## Strengths

- Clean package separation with proper middleware chaining
- Dual read/write SQLite connections avoids lock contention
- Async usage logging via buffered channel — non-blocking to requests
- Streaming SSE support with line-by-line token extraction
- Auth strips Bearer token before forwarding to Ollama (no key leakage)
- Full k3s deployment pipeline

## Known Gaps

| Area | Issue |
|------|-------|
| Testing | Zero test files — CI runs `go test` but finds nothing |
| Endpoints | Only chat completions + models; no embeddings, completions, or images |
| Observability | No structured logging, no metrics, no request IDs |
| Rate limiting | In-memory only — resets on restart |
| Validation | Request body forwarded as-is, no schema checks |
| Scale | Single replica, no multi-backend support |
| Storage | 100Mi PVC, no backup strategy, soft-delete only (unbounded growth) |
| Streaming | Token extraction is fragile — silently fails if Ollama changes SSE format |

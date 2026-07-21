# local-ai-proxy

An OpenAI-compatible gateway for self-hosted inference: API key authentication, per-key rate limiting, credit-based usage accounting, user management, and model-aware routing across multiple backend nodes (Ollama or any OpenAI-compatible server). All routes are under the `/api` prefix, leaving the root path free for a frontend.

## Architecture

```
Client → RequestID → CORS → Auth → CreditGate → RateLimit → Proxy
                                                              │ Resolve(model)
                                                              ▼
                                                        Node Registry ──► Node "workstation" (ollama)
                                                              │      ──► Node "gpu-box"     (ollama)
                                                              │      ──► Node "cloud"       (openai_compat)
                                                              ▼
                                                  Async Usage Logger → PostgreSQL
```

The gateway routes each chat request **by model name** to a healthy backend node:

- **Node registry** (`internal/registry`) — an in-memory model→nodes routing map, published as an immutable snapshot (`atomic.Pointer`, copy-on-write). Multiple healthy nodes serving the same model are round-robined.
- **Health poller** (`internal/poller`) — probes every enabled node on a ~15s interval (±20% deterministic per-node jitter, 5s per-probe timeout, 1MB response cap, redirects refused). For `ollama` nodes one `GET {base_url}/api/tags` request is both liveness check and model discovery; for `openai_compat` nodes it is `GET {base_url}/v1/models`. Nodes with `static_models` are only health-checked (2xx = alive) and their configured list stays authoritative.
- **Startup discovery sweep** — before the HTTP listener opens, all enabled nodes are probed in parallel under a bounded budget (5s per probe, ~6s overall), so restarts route deterministically. There is no optimistic routing: a node is only routable once a probe (or its static list) has established what it serves.
- **Health hysteresis** — in the running poller a node goes `healthy → unhealthy` only after **2 consecutive probe failures** (no flapping on one timeout); any success is immediately healthy. The startup sweep and admin-triggered probes are decisive on a single failure.
- **Immediate probe on admin writes** — creating, updating, or refreshing a node via the admin API probes it synchronously before the response returns; deleting a node removes it from routing before the response returns.

Everything else — auth, credits, rate limiting, usage accounting — stays in the gateway against the single shared Postgres. There is exactly one gateway process by design (see Known Gaps).

Internal packages: `config`, `auth`, `authlimit`, `store`, `ratelimit`, `proxy`, `registry`, `poller`, `nodesource`, `middleware`, `admin`, `user`, `credits`, `bootstrap`, `logging`, `requestid`, `health`, `metrics`, `apierror` — all using stdlib `net/http`, no frameworks.

## Endpoints

### Proxy (authenticated via `Authorization: Bearer <api-key>`)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/chat/completions` | Routed to a healthy node serving the requested model (streaming + non-streaming), with usage tracking |
| `GET` | `/api/v1/models` | Lists models with active pricing **and** at least one healthy node serving them (set `MODELS_LIST_ALL=true` to list the full priced catalog); `owned_by` is the node name, or `multiple` |

### Health & Observability

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/healthz/live` | Liveness probe — always 200 |
| `GET` | `/api/healthz/ready` | Readiness probe — checks DB, node registry, usage writer (see below) |
| `GET` | `/api/healthz` | Alias for `/api/healthz/live` (backward compat) |
| `GET` | `/metrics` | Prometheus metrics endpoint |

Readiness = DB ok **and** usage writer ok **and** (*zero enabled nodes* **or** *at least one healthy node*). Zero enabled nodes is deliberately *ready* — a fresh install must be able to serve the admin API to register its first node (the `nodes` check reports `"detail": "no nodes configured"`). Readiness reads the registry snapshot only; it never probes backends synchronously.

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

### Admin (authenticated via `X-Admin-Key` header — rate limited to 10 req/min — or an admin Bearer session)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/keys` | Create API key (`{name, rate_limit, account_id?}`) — attaches to the given account (404 if unknown), or to the auto-created `admin-service` account when `account_id` is omitted; response includes `account_id`; returns full key once, never retrievable again |
| `GET` | `/api/admin/keys` | List all keys (id, name, key_prefix, rate_limit, created_at, revoked) |
| `GET` | `/api/admin/keys/{id}` | Key detail |
| `PUT` | `/api/admin/keys/{id}/rate-limit` | Update a key's rate limit |
| `PUT` | `/api/admin/keys/{id}/session-limit` | Set/clear a key's session token limit |
| `PUT` | `/api/admin/keys/{id}/trust-user-headers` | Trust forwarded end-user identity headers on this key (`{trust_user_headers: bool}`) — requests then bill auto-provisioned per-user accounts (see docs/design/end-user-accounts.md) |
| `DELETE` | `/api/admin/keys/{id}` | Revoke (soft-delete) a key |
| `GET` | `/api/admin/usage` | Aggregated usage stats (filterable by `key_id`, `since`, and `node_id`) |
| `GET` | `/api/admin/usage/summary` \| `/by-model` \| `/by-user` \| `/timeseries` | Usage analytics |
| `GET` | `/api/admin/users` | List all users |
| `GET` | `/api/admin/users/{id}` | User detail |
| `PUT` | `/api/admin/users/{id}/activate` \| `/deactivate` \| `/role` | User mutations |
| `GET` | `/api/admin/accounts` | List accounts (credit balances) |
| `POST` | `/api/admin/accounts/{id}/credits` | Grant credits |
| `POST` | `/api/admin/accounts/{id}/keys` | Create a key bound to an account |
| `PUT` | `/api/admin/accounts/{id}/allowance` | Set (`{monthly_grant: 12.5}`) or clear (`{monthly_grant: null}` → env default) an end-user account's monthly allowance override |
| `GET`/`POST` | `/api/admin/pricing` | List / upsert model pricing (`{model_id, prompt_rate_per_mtok, completion_rate_per_mtok, typical_completion}`) |
| `DELETE` | `/api/admin/pricing/{id}` | Deactivate pricing |
| `GET`/`POST`/`DELETE` | `/api/admin/registration-tokens` | Manage service-registration tokens |
| `GET` | `/api/admin/registrations` | Registration audit feed |
| `GET` | `/api/admin/config` | Effective config snapshot (includes `models_list_all`, `nodes_file`) |
| `GET` | `/api/admin/health` | Component health: db, **nodes** (total/healthy counts), usage writer, uptime |
| `POST` | `/api/admin/bootstrap` | One-time first-admin bootstrap (404 unless `ADMIN_BOOTSTRAP_TOKEN` is set) |

#### Admin keys and the `admin-service` account

Every API key is **account-backed and credit-gated** — there is no bypass. Admin-minted keys attach to an account like any other key:

- `POST /api/admin/keys` with `account_id` binds the key to that existing account (same effect as `POST /api/admin/accounts/{id}/keys`).
- Without `account_id`, the key attaches to the designated **`admin-service`** account, auto-created at startup with a one-time starting balance (`ADMIN_SERVICE_CREDIT_GRANT`, default 1,000,000 credits). Top it up any time via `POST /api/admin/accounts/{id}/credits`.
- On upgrade, a startup backfill attaches all pre-existing NULL-account keys to their owner's account (user keys) or to `admin-service` (admin keys), so live keys keep working. Because those keys are now credit-gated, requests for **unpriced models return `400 unknown_model`** — add pricing for the models you serve (`POST /api/admin/pricing`).

#### Node management

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/nodes` | Register a node `{name, base_url, backend_type?, auth_header?, static_models?, health_path?, timeout_seconds?}` — probed synchronously; the 201 response includes initial health |
| `GET` | `/api/admin/nodes` | List nodes: stored config joined with live state |
| `GET` | `/api/admin/nodes/{id}` | Node detail |
| `PUT` | `/api/admin/nodes/{id}` | Update mutable fields (API-sourced nodes only), then re-probe synchronously |
| `DELETE` | `/api/admin/nodes/{id}` | Disable the node (soft-delete) and remove it from routing before returning (204) |
| `POST` | `/api/admin/nodes/{id}/refresh` | Force an immediate probe + model rediscovery |

Response conventions:

- List and detail responses use the `{"data": ...}` envelope (lists add `"pagination": {limit, offset, total}`). `?envelope=0` opts out for one deprecation release.
- `auth_header` is always **masked** in responses (e.g. `"Bearer sk-…abcd"`) — the raw value is write-only.
- Every node response carries live state alongside stored config: `health` (`healthy` / `unhealthy` / `unknown`), `models` (discovered or static list), `last_error`, `last_checked_at`.
- `PUT` is PATCH-like — omitted fields keep their current value:
  - `auth_header`: absent = keep, `""` = clear, value = replace
  - `health_path`: absent = keep, `""` = clear, value = replace
  - `static_models`: absent/null = keep, `[]` = clear (switch back to probe discovery), non-empty = replace
  - `timeout_seconds`: absent = keep, `0` = clear (use the 5-minute default), positive = replace
  - `enabled`: absent = keep; `true` re-enables a previously deleted node
- Config-sourced nodes (from `NODES_FILE` / `OLLAMA_URL`) are **read-only via the API**: `PUT`/`DELETE` return `409 config_sourced_node` pointing at the file.

## Node registration

Nodes come from two coexisting sources: a static JSON file (`NODES_FILE`, the compose/self-hoster path) and the admin API (the dynamic path).

### `NODES_FILE`

```json
{
  "nodes": [
    { "name": "workstation", "base_url": "http://192.0.2.10:11434", "backend_type": "ollama",
      "timeout_seconds": 900 },
    { "name": "gpu-box", "base_url": "http://ollama.example.internal:11434", "backend_type": "ollama" },
    { "name": "cloud", "base_url": "https://api.example.com", "backend_type": "openai_compat",
      "auth_header": "Bearer ${CLOUD_KEY}", "static_models": ["gpt-4o-mini"] }
  ]
}
```

- `${VAR}` references (braced form only) in any string value are expanded from the environment at load time, so secrets stay out of the file. Referencing an **undefined** variable fails startup with an error naming the variable.
- The file is parsed strictly: unknown fields, duplicate node names, and trailing data after the JSON document all fail startup fast.
- `backend_type` defaults to `"ollama"`. `static_models` (non-empty) disables discovery — the list is authoritative and the node is only health-checked (`health_path` if set, else the backend type's discovery endpoint; only the status code is used).
- `base_url` is the backend's **API root, excluding the `/v1` segment** — the gateway path-joins `/v1/chat/completions`, `/v1/models`, or `/api/tags` onto it (a prefix like `https://host/openai` is preserved). Scheme must be `http`/`https`; userinfo, query, and fragment are rejected; a `base_url` ending in `/v1` is rejected with a hint.

**Merge semantics at startup** (idempotent):

- Declared nodes are upserted by `name` with `source="config"`.
- Config-sourced nodes in the DB that are no longer declared are **disabled** (never hard-deleted; usage rows reference them).
- API-sourced nodes are never touched by file loading — but a `name` collision between the file and an API-sourced node **fails startup** with a clear error.

### `OLLAMA_URL` (single-node shortcut) — breaking change

> **BREAKING**: the implicit `http://localhost:11434` default is **gone**. Older releases assumed a localhost Ollama when `OLLAMA_URL` was unset; now an unset `OLLAMA_URL` means **zero nodes**. If you relied on the implicit default, set `OLLAMA_URL=http://localhost:11434` explicitly — one line.

- `OLLAMA_URL` explicitly set → a config-sourced node named `default` (`backend_type: "ollama"`) is synthesized from it at startup and merges exactly like a file node. Existing single-node deployments that set the variable upgrade with zero config changes. (Declaring another node named `default` in `NODES_FILE` at the same time is a startup error.)
- `OLLAMA_URL` unset → no synthesis, ever. A `NODES_FILE`-only install gets exactly the nodes it declared; a fresh install starts with zero nodes — which is *ready*, so the admin API is available to register the first node via `POST /api/admin/nodes`. Chat requests return `503 model_unavailable` until a node serves the requested model.

### Price your models (required setup step)

The pricing catalog starts **empty** — nothing is seeded. `GET /api/v1/models` lists the intersection of *actively priced* models and models *served by a healthy node*, so after registering nodes you must price each model you want to expose (the gateway logs a warn-level reminder at startup while the catalog is empty):

```bash
# Rates are credits per MILLION tokens (2000/MTok below = 0.002 credits per token).
curl -s -X POST -H "X-Admin-Key: $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"model_id":"llama3.1:8b","prompt_rate_per_mtok":2000,"completion_rate_per_mtok":2000,"typical_completion":500}' \
  http://localhost:8080/api/admin/pricing
```

Pricing is also **required** for any credit-backed key (keys created for user or service accounts): requests for unpriced models are rejected with `400 unknown_model`. Rates are in credits per **million** tokens (per-MTok, the industry convention; the old per-token field names `prompt_rate`/`completion_rate` are rejected with `400 unknown_field`); `typical_completion` feeds the completion-token estimate that sizes per-request credit holds when the client sends no `max_tokens` and the account has little usage history.

## Request Flow

The middleware chain (auth → credit gate → rate limit) runs before routing, so `401`/`402`/`429` always precede `503 model_unavailable`. Within the proxy handler, order matters:

1. Body read into memory (capped by `MAX_REQUEST_BODY`) and validated: malformed JSON or a missing/empty `model` → `400` (OpenAI-shaped `invalid_request_error`).
2. Pricing check: unpriced model → `400 unknown_model`; session token limit checked. (All keys are credit-backed — the startup backfill attaches legacy NULL-account keys to the `admin-service` account.)
3. **`Resolve(model)`** against the registry snapshot: no healthy node serving the model → `503 model_unavailable`. Resolution happens **before** any credit hold, so node outages cause zero hold churn.
4. Credits reserved.
5. Forward to `{node.base_url}/v1/chat/completions` with the node's `auth_header` (the client's own `Authorization` is stripped and never forwarded) under the node's timeout budget (`timeout_seconds`, default 5 minutes, applied as a per-request context deadline).

The upstream client never follows redirects (a redirect with `auth_header` attached could exfiltrate node credentials — a redirecting upstream fails the request), and non-streaming/error response bodies are read through the `MAX_REQUEST_BODY` cap.

### Non-Streaming (`stream=false` or omitted)

Response body read (capped), token usage parsed from the JSON, credits settled against the hold, usage entry queued, response written to the client unchanged.

### Streaming (`stream=true`)

Response streamed line-by-line via `bufio.Reader`; each `data: {...}` line is observed for a usage object (non-destructive) and flushed to the client immediately for SSE delivery. On EOF/error, status is logged as completed, partial, or error; credits settled from observed (or byte-estimated) usage.

### Async Usage Logging

All requests write to a buffered channel (capacity 1000). A dedicated goroutine drains the channel and calls `store.LogUsage()`. Every entry records the `node_id` of the node that served it (NULL when no node was resolved). On shutdown, the channel is closed and remaining entries are drained.

## Database

PostgreSQL via pgx/v5 connection pool. Schema auto-migrated on startup via embedded SQL.

### Schema (excerpt — see `internal/store/schema.sql` for the full schema, including accounts, credits, pricing, and sessions)

```sql
api_keys (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL UNIQUE,
  key_prefix  TEXT NOT NULL,
  rate_limit  INTEGER NOT NULL DEFAULT 60,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  revoked     BOOLEAN NOT NULL DEFAULT FALSE,
  user_id     BIGINT REFERENCES users(id),  -- NULL = admin-created key
  account_id  BIGINT REFERENCES accounts(id) -- billing account; backfilled at startup, never NULL afterwards
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
  node_id           BIGINT REFERENCES nodes(id), -- node attribution; NULL = pre-routing rows
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
)

nodes (
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
```

Discovered model lists are deliberately **not** persisted — they are runtime state, re-derived by the startup sweep and the poller (`static_models` covers nodes where discovery is undesired).

## Configuration

All configuration via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `ADMIN_KEY` | *(required)* | Admin API authentication key |
| `DATABASE_URL` | *(required)* | PostgreSQL connection string |
| `OLLAMA_URL` | *(none)* | Optional single-node shortcut: when set, a config-sourced node named `default` is synthesized from it at startup; when unset there is **no** synthesized node — a fresh install starts with zero nodes (see Node registration; the old localhost default is gone) |
| `NODES_FILE` | *(none)* | Path to a JSON node-declaration file (see Node registration) |
| `MODELS_LIST_ALL` | `false` | `GET /v1/models` lists every actively priced model instead of the priced-AND-served intersection |
| `ADMIN_BOOTSTRAP_TOKEN` | *(none)* | Enables `POST /api/admin/bootstrap` (one-time first-admin creation) when set |
| `DEFAULT_CREDIT_GRANT` | `0` | Credits granted to newly registered accounts |
| `ADMIN_SERVICE_CREDIT_GRANT` | `1000000` | One-time starting balance for the auto-created `admin-service` account (applied at creation only — top up later via `POST /api/admin/accounts/{id}/credits`; must be >= 0) |
| `END_USER_MONTHLY_GRANT` | `5.0` | Monthly allowance (reset-to-grant, no rollover) for auto-provisioned end-user accounts on trusted keys; per-account override via `PUT /api/admin/accounts/{id}/allowance`; must be >= 0 |
| `PORT` | `8080` | Server listen port |
| `CORS_ORIGINS` | `*` | Allowed CORS origins |
| `MAX_REQUEST_BODY` | `52428800` (50MB) | Max request body size in bytes (chat proxy path); also caps upstream non-streaming/error response reads |
| `MAX_JSON_REQUEST_BODY` | `1048576` (1MB) | Max body size for JSON API endpoints (auth/users/accounts/admin) |
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `AUTH_RATELIMIT_LOGIN_PER_MIN` | `5` | Login attempts per minute per client IP |
| `AUTH_RATELIMIT_LOGIN_EMAIL_PER_MIN` | `5` | Login attempts per minute per target email |
| `AUTH_RATELIMIT_REGISTER_PER_MIN` | `3` | Registrations per minute per client IP (user + service) |
| `AUTH_RATELIMIT_GENERAL_PER_MIN` | `120` | Other `/api/auth|users|accounts` requests per minute per IP |
| `AUTH_BCRYPT_MAX_CONCURRENT` | `8` | Global cap on simultaneous bcrypt operations |

## Deployment

Multi-stage Docker build (`deploy/Dockerfile`); a complete self-hosting example (gateway + Postgres + two Ollama nodes) lives in [`deploy/examples/`](deploy/examples/), and [`docs/deployment.md`](docs/deployment.md) covers multi-machine topologies (k3s, native macOS, overlay networks) and Ollama tuning. Reference k8s manifests are in `deploy/k8s/`.

```bash
# Build
CGO_ENABLED=0 go build -ldflags="-s -w" -o proxy ./cmd/proxy

# Run (single-node shortcut)
ADMIN_KEY=your-key DATABASE_URL=postgres://... OLLAMA_URL=http://localhost:11434 ./proxy

# Docker
docker build -f deploy/Dockerfile -t local-ai-proxy .
```

> Backends are unauthenticated by default (Ollama has no auth). Never expose a node on an untrusted network without a reverse proxy + token (`auth_header` supports this) or a private overlay network — see `docs/deployment.md`.

## Strengths

- Clean package separation with proper middleware chaining
- Model-aware routing across N backend nodes with health checking, discovery, and round-robin
- Async usage logging via buffered channel — non-blocking to requests
- Streaming SSE support with line-by-line token extraction
- Upstream hardening: redirects refused, response caps, per-node timeouts, header-injection validation
- Auth strips the client's Bearer token before forwarding (no key leakage); per-node `auth_header` for protected backends
- User registration, session management, credit accounting
- Test coverage across all packages (including `-race` on the registry)

## Known Gaps

| Area | Issue |
|------|-------|
| Endpoints | Only chat completions + models; no embeddings, completions, or images |
| Scale | Exactly one gateway replica by design — rate limiting is in-memory (correct with a single gateway, resets on restart); multi-gateway needs shared rate-limit state |
| Routing | Round-robin only; no weighted/least-loaded balancing, model aliases, or fallback chains yet |
| Nodes | Gateway must reach nodes over the network (inbound HTTP); no push-mode agents for NAT'd machines |
| Validation | Model name and JSON shape are validated; the rest of the request body is forwarded as-is, no schema checks |
| Storage | No backup strategy, soft-delete only (unbounded growth) |
| Streaming | Token extraction is fragile — silently fails if a backend changes its SSE format |

## Observability

### Structured Logging

All logs are JSON to stdout via `slog`, ready for any log collector. Every log entry within an HTTP request includes `request_id` automatically via a context-aware slog handler. Routing decisions are logged at debug level with `request_id`, `model`, and `node`; node health transitions are logged at info level.

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
| `aiproxy_tokens_total` | Counter | Tokens processed (by model, node, prompt/completion) |
| `aiproxy_node_up` | Gauge | Per-node health (`{node="name"}`), updated on every probe |
| `aiproxy_ollama_up` | Gauge | Legacy: OR of all node states (kept for one release, then removed) |
| `aiproxy_credit_gate_rejects_total` | Counter | Requests rejected by credit gate |
| `aiproxy_ratelimit_rejects_total` | Counter | Requests rejected by rate limiter |
| `aiproxy_usage_channel_depth` | Gauge | Async usage channel depth |

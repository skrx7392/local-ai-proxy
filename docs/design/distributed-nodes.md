# Design: Distributed Backend Nodes

**Status**: v4 — final; two Codex review rounds incorporated, open questions resolved (2026-07-07)
**Scope**: backend (`local-ai-proxy`); admin frontend changes noted but designed separately

## Summary

Today the proxy forwards every chat request to a single backend (`OLLAMA_URL`). This
design adds a **node registry**: multiple inference backends on multiple machines, each
serving one or more models, with the gateway routing each request by model to a healthy
node. All auth, credits, rate limiting, and usage accounting stay in the gateway against
the single shared Postgres — nothing about the data plane's source of truth changes.

The design also prepares the project for open-sourcing: backends are generic
OpenAI-compatible servers (Ollama is one flavor), node connectivity makes no network
assumptions (Tailscale, LAN, cluster DNS, or public TLS all work), and nodes can be
declared in a static config file or managed dynamically via the admin API.

## Goals

- Route `/api/v1/chat/completions` requests to the correct backend node by model name.
- Support N machines each running one Ollama (or other OpenAI-compatible) instance.
- Auto-discover which models each node serves; re-discover periodically.
- Health-check nodes and route around dead ones.
- Attribute every usage log row to the node that served it.
- Keep existing single-node deployments working with zero config changes.
- Registration via static config file (compose/self-hoster path) **and** admin API
  (dynamic path), coexisting.

## Non-goals (v1)

- **Gateway replication.** Exactly one gateway process. Rate limiting stays in-memory
  and correct. Multi-gateway (and the DB/Redis rate-limit rework it requires) is future work.
- **Push-mode node agents.** Nodes must be reachable by the gateway (inbound HTTP).
  NAT-traversal / reverse-tunnel agents are future work.
- **Load balancing policies.** If multiple healthy nodes serve the same model, v1 does
  round-robin. Weighted/least-loaded routing is future work.
- **Model aliases and fallback chains.** Roadmap items, designed later.
- **Non-Postgres databases.** SQLite's single-writer model conflicts with the project's
  transactional credit system; explicitly out of scope.
- **Embeddings / completions endpoints.** Tracked separately in Known Gaps.

## Terminology

- **Gateway** — the existing proxy process (auth, credits, rate limit, usage, admin API).
- **Node** — an inference backend the gateway forwards to, identified by a **base URL**.
  A node is *not* a machine: one machine may host multiple nodes (rare, but supported).
- **Backend type** — the API flavor of a node: `ollama` or `openai_compat`.

## Architecture

```
                          ┌─────────────────────────────┐
Client ──► Gateway        │  Node "m5-max"  (ollama)     │──► qwen3-coder:30b
           ├─ Auth        │  http://100.x.y.z:11434      │
           ├─ Router      └─────────────────────────────┘
           ├─ CreditGate  ┌─────────────────────────────┐
           ├─ RateLimit   │  Node "k3s-gpu" (ollama)     │──► llama3.2:3b, phi4
           │              │  http://ollama.models.svc    │
           ▼              └─────────────────────────────┘
        PostgreSQL        ┌─────────────────────────────┐
        (usage, credits,  │  Node "cloud"  (openai_compat)│──► gpt-4o-mini
         keys, nodes)     │  https://api.example.com     │
                          └─────────────────────────────┘
```

The gateway keeps an **in-memory registry**: node configs (from DB + config file) plus
runtime state (health, discovered models). Runtime state is not persisted — with a
single gateway there is no second reader — but startup runs a **synchronous initial
discovery sweep** (see below) so the model→node map is deterministic before traffic is
accepted. Admin endpoints expose the in-memory state.

### Registry concurrency

The poller and request path share the registry, so its routing state is published as an
**immutable snapshot** swapped via `atomic.Pointer` (copy-on-write): the poller builds a
new snapshot (node health + model→nodes map) and publishes it atomically; `Resolve`
only ever reads one snapshot. Round-robin counters live **outside** the snapshot, in a
mutex-guarded map owned by the registry, so they survive snapshot republishes (a
counter rebuilt every 15s poll cycle would reset balancing constantly). The counter map
is pruned to routable models on each republish — bounded by the discovered catalog, no
per-request allocation from arbitrary client-supplied model names. Race coverage via
`go test -race` is part of the registry PR's acceptance criteria.

## Data model

Additions to `internal/store/schema.sql`, following the existing idempotent style:

```sql
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
```

Notes:

- **No `node_models` table.** Discovered model sets are runtime state, not config; they
  are re-derived by the startup sweep and the poller. `static_models` covers nodes
  where discovery is undesired or the discovery endpoint is unreliable.
- `auth_header` is a secret. Admin reads return it masked (`"Bearer sk-…abcd"`), same
  convention as API keys (`key_prefix`). Values are validated at registration to reject
  control characters (CR/LF header-injection guard). It is never logged.
- Store update helpers set `updated_at` explicitly (no trigger; matches repo style).
- `health_path` is **path-only**: must start with `/`; scheme, host, userinfo, query,
  and fragment are rejected at registration. It is path-joined onto the node's
  `base_url`, so it can never become a second URL/SSRF surface.
- Nodes referenced by `usage_logs` can never be hard-deleted; `DELETE` on the admin API
  sets `enabled=false` (consistent with the repo's soft-delete convention).

### `base_url` semantics

`base_url` is the backend's **API root, excluding the `/v1` segment**. The gateway
builds upstream URLs by **path-joining** (never overwriting the path, unlike today's
`upstreamURL.Path = "/v1/chat/completions"`):

- chat: `{base_url}/v1/chat/completions`
- discovery (`openai_compat`): `{base_url}/v1/models`
- discovery (`ollama`): `{base_url}/api/tags`

So `https://host/openai` → `https://host/openai/v1/chat/completions`, and a bare origin
like `http://100.x.y.z:11434` works for Ollama. Registration validates and
canonicalizes: scheme must be `http`/`https`, no query/fragment/userinfo, trailing
slash trimmed, and a `base_url` ending in `/v1` is rejected with a hint (a common
mistake; we append `/v1` ourselves). vLLM, llama.cpp server, LM Studio, and TGI all
serve under `/v1`, so no per-node path override is needed in v1.

## Node registration

### Static config file (`NODES_FILE`)

JSON (stdlib-parseable; no new dependency), mounted into the container or placed on disk:

```json
{
  "nodes": [
    { "name": "m5-max",  "base_url": "http://100.101.2.3:11434", "backend_type": "ollama",
      "timeout_seconds": 900 },
    { "name": "k3s-gpu", "base_url": "http://ollama.models.svc.cluster.local:11434", "backend_type": "ollama" },
    { "name": "cloud",   "base_url": "https://api.example.com",  "backend_type": "openai_compat",
      "auth_header": "Bearer ${CLOUD_KEY}", "static_models": ["gpt-4o-mini"] }
  ]
}
```

`${VAR}` references are expanded from the environment at load time so secrets stay out
of the file. An invalid file fails startup fast (matches `config.Load` behavior).

**Merge semantics at startup**: config-file nodes are upserted by `name` with
`source='config'`; config-sourced nodes absent from the file are disabled. API-sourced
nodes are untouched by file loading. Config-sourced nodes are read-only via the admin
API (mutations return 409 with a pointer to the file). A `name` collision between the
file and an API-sourced node fails startup with a clear error.

### Admin API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/admin/nodes` | Register a node `{name, base_url, backend_type, auth_header?, static_models?, health_path?, timeout_seconds?}` |
| `GET` | `/api/admin/nodes` | List nodes: config + live state (health, models, last_error, last_checked_at) |
| `PUT` | `/api/admin/nodes/{id}` | Update mutable fields (API-sourced nodes only) |
| `DELETE` | `/api/admin/nodes/{id}` | Disable the node and remove it from routing |
| `POST` | `/api/admin/nodes/{id}/refresh` | Force an immediate probe + rediscovery |

Nodes created or updated via the API are probed immediately (synchronously, with the
probe timeout) so the response can include initial health, and the registry snapshot is
republished.

### Backward compatibility with `OLLAMA_URL`

`config.Load` currently defaults `OLLAMA_URL` to `http://localhost:11434` even when
unset, so synthesis keys off **explicit presence** (`os.LookupEnv`), not the value:

- `OLLAMA_URL` explicitly set → synthesize config-sourced node
  `{name: "default", base_url: $OLLAMA_URL, backend_type: "ollama"}` (merges like any
  config node). Existing deployments upgrade with zero config changes.
- `OLLAMA_URL` unset → **no synthesis, ever.** A `NODES_FILE`-only install gets exactly
  the nodes it declared, and a fresh install starts with zero nodes (which is *ready* —
  see Readiness — so the admin API is available to register the first node).

This deliberately drops the implicit localhost fallback: today a bare `./proxy` run
with no `OLLAMA_URL` silently assumes `http://localhost:11434`; after this change it
starts with zero nodes and chat requests 503 until a node is set. Silent-implicit and
zero-node-ready cannot coexist (an implicit node that isn't running would fail
readiness on every fresh install). Local dev sets `OLLAMA_URL` explicitly — one line,
documented in the README.

## Discovery and health checking

A single poller goroutine (started from `main.go` alongside the sweeper) probes every
enabled node on an interval.

**Startup sweep**: before the HTTP listener starts, the registry probes all enabled
nodes **in parallel** with a bounded budget (per-probe timeout 5s; overall sweep capped
at ~6s). Nodes that respond are `healthy` with a populated model list; nodes that don't
are `unhealthy` and receive no traffic until the poller reaches them. There is **no
optimistic routing to unprobed nodes**: `Resolve` only considers nodes whose model list
is known (discovered or `static_models`). This keeps restarts deterministic at the cost
of a few seconds of startup latency, and means a temporarily-down node's models 503
until it recovers — the correct signal.

**Ongoing polling**:

- **Interval**: 15s with ±20% jitter per node; per-probe timeout 5s; response body
  capped at 1MB; HTTP redirects disabled.
- **Probe = discovery** (one request does both):
  - `ollama` → `GET {base_url}/api/tags` — liveness + model list (tag names).
  - `openai_compat` → `GET {base_url}/v1/models` — liveness + model list (`data[].id`).
  - Nodes with `static_models`: probe `health_path` if set, else the type's discovery
    endpoint; only the status code is used (2xx = alive), body ignored. `health_path`
    exists precisely for backends whose discovery endpoint is absent or gated.
- **State machine**: `healthy` → `unhealthy` after **2 consecutive failures**
  (hysteresis avoids flapping on one timeout); `unhealthy` → `healthy` on first
  success. Transitions are logged and update `aiproxy_node_up{node}`.
- The per-node `auth_header` is sent on probes (cloud backends require it).
- Each successful or failed probe cycle republishes the registry snapshot.

## Request routing

`proxy.NewHandler` changes its first parameter from `ollamaURL *url.URL` to a registry
dependency. The registry is needed beyond `Resolve` (models listing, admin health), so
the interface is:

```go
type Registry interface {
    // Resolve returns a healthy node serving model, or ErrModelUnavailable.
    // Successive calls for the same model round-robin across healthy nodes.
    Resolve(model string) (Node, error)
    // Snapshot returns the current node states and model→node map
    // (for /v1/models, /api/admin/nodes, /api/admin/health, readiness).
    Snapshot() RegistrySnapshot
}
type Node struct {
    ID         int64
    Name       string
    BaseURL    *url.URL
    AuthHeader string // "" = none
}
```

### Request flow (order matters — this changes the credit path)

The middleware chain is unchanged: auth → credit gate (including the per-key
session-token-limit check) → rate limit all run **before** the proxy handler, so
`401`/`402`/`429` continue to precede routing, and `503 model_unavailable` is only
ever returned to requests that already passed those gates. Within the handler:

1. Peek body; **validate**: malformed JSON or missing/empty `model` → `400`
   (OpenAI-shaped `invalid_request_error`). Today parse errors are silently ignored
   and delegated upstream; with routing that is no longer viable.
2. Pricing check (existing credit-gate behavior): unknown model → `400`.
3. **`Resolve(model)`**: no healthy node → `503 model_unavailable`. **No credit hold
   exists yet** — resolution happens *before* reservation, so outages cause zero
   hold churn and cannot strand credits behind the 10-minute sweeper.
4. Reserve credits (existing).
5. Forward to `{node.base_url}/v1/chat/completions`; settle/release as today.

A node can die between steps 3 and 5; that surfaces as an upstream error on the
existing failure path (release hold, log `error` status) — same as today's Ollama-down
behavior, no new handling needed.

Other routing behavior:

- **Multiple healthy nodes for one model**: round-robin from the snapshot (see
  Registry concurrency).
- **Auth forwarding**: the client's `Authorization` header is stripped today; when the
  resolved node has `auth_header`, it is set on the upstream request.
- **Usage attribution**: `store.UsageEntry` gains `NodeID *int64`; `LogUsage`'s insert
  column list is extended accordingly. Entries from requests that never resolved a
  node keep `NULL`.

### Upstream HTTP client hardening

Applies to **both** the poller's client and the chat-forwarding client (today's chat
client follows redirects and `io.ReadAll`s the non-streaming body unboundedly):

- `CheckRedirect` returns an error — redirects are never followed. With `auth_header`
  attached, following a redirect could exfiltrate backend credentials to an attacker-
  influenced location; this is the single most important client change.
- Non-streaming responses and upstream error bodies are read through a size cap
  (`io.LimitReader`, configurable, default 50MB to match `MAX_REQUEST_BODY`; probe
  bodies 1MB).
- Both properties get explicit acceptance tests (redirect returns 502; oversized
  body truncates/errors cleanly).
- **Per-node timeout**: the chat client's fixed 5-minute `http.Client.Timeout` becomes
  the *default*; a node with `timeout_seconds` set gets that value instead. Implemented
  as a per-request `context.WithTimeout` deadline (one shared client, no client Timeout
  field) so each request carries its node's budget. This exists for slow CPU-only
  nodes, where a long non-streaming completion on a large model can legitimately exceed
  5 minutes.

### `/api/v1/models`

By default, returns models that have **active pricing AND at least one healthy node
serving them** (intersection). Listing a model a client cannot actually use violates
the OpenAI client contract more than omitting a temporarily-down one. This is
configurable: `MODELS_LIST_ALL=true` lists every actively priced model regardless of
node availability (for deployments that want the full catalog visible even when
machines are asleep). Default `false`. The flag is reported in the admin config
snapshot like other env config.

Each entry keeps the OpenAI shape; `owned_by` becomes the node name for single-node
models, `"multiple"` otherwise (and the priced-model's pricing row's existence is the
only requirement when `MODELS_LIST_ALL=true`). Built from `Registry.Snapshot()` +
`ListActivePricing` (replacing the pricing-only listing).

### Readiness

`/api/healthz/ready` currently makes a synchronous `HEAD` to Ollama. New semantics,
all derived from the registry snapshot (no synchronous probes on the readiness path):

- ready = DB ok + usage writer ok + (**zero enabled nodes** OR **≥1 healthy node**).
- Zero enabled nodes is *ready*: a fresh OSS install must be able to serve the admin
  API/UI to register its first node. A "no nodes configured" warning appears in
  `/api/admin/health`.
- Because the startup sweep completes before the listener starts, there is no
  "unready until first poll" window; single-node deployments keep today's semantics
  (backend down → not ready).
- `unhealthy`/unprobed nodes are treated identically everywhere: not routable, absent
  from `/v1/models`, not counted by readiness.

## Observability

- `aiproxy_node_up{node}` gauge. `aiproxy_ollama_up` is kept for one release as the OR
  of all node states, then removed.
- `aiproxy_tokens_total` gains a `node` label (recorded in the proxy handler, where the
  resolved node is in scope; cardinality is a handful of nodes). Request-level metrics
  (`aiproxy_requests_total`, duration histogram) are recorded in middleware without
  node context and stay node-free in v1 — threading routing results into middleware
  isn't worth the coupling yet.
- Admin usage endpoints (`/api/admin/usage`) accept a `node_id` filter; per-node
  aggregates power a future admin-UI Nodes page.
- Routing decisions are logged at debug with `request_id`, `model`, `node`.

## Security considerations

1. **The gateway fetches admin-supplied URLs (SSRF surface).** Mitigations: node
   registration is admin-key-gated; schemes restricted to `http`/`https`; URL
   canonicalization at registration; redirects disabled on *all* upstream clients;
   probe/proxy timeouts and response caps enforced. For OSS deployments where
   admin ≠ infra owner, a future `NODES_DENY_CIDRS` option is noted in the roadmap;
   v1 documents the trust assumption (admins are trusted to register internal URLs).
2. **`auth_header` at rest** is stored plaintext in Postgres, like other server-side
   secrets in this deployment class. It is masked on all read paths, validated against
   header injection (control characters rejected), and never logged. At-rest encryption
   is deliberately out of scope (the DB already holds credential hashes and the key
   would live next to the data).
3. **Backends are unauthenticated by default** (Ollama has no auth). The docs must be
   explicit: never expose a node on an untrusted network without a reverse proxy +
   token (`auth_header` supports this) or a private overlay (Tailscale/WireGuard/VPC).
4. **No network assumptions**: nodes are plain URLs. TLS works out of the box
   (`https://` base URLs); private CAs via standard `SSL_CERT_FILE` mechanisms.

## Failure modes

| Failure | Behavior |
|---------|----------|
| Node dies mid-stream | Stream ends; usage logged as `partial`/`error` (existing paths); hold settled/released as today |
| Node dies between Resolve and forward | Upstream error path: hold released, `error` status logged (same as today's Ollama-down) |
| Node unreachable at probe | Marked unhealthy after 2 failures; routed around; `aiproxy_node_up` drops |
| All nodes for a model down | `503 model_unavailable` **before any credit hold**; model disappears from `/v1/models` |
| All nodes down | Readiness fails (when ≥1 node enabled); chat requests 503; admin API keeps serving |
| Zero nodes configured | Ready; admin API serves; chat requests 503; warning in admin health |
| Gateway restart | Startup sweep (≤ ~6s, parallel) rebuilds health + model map before the listener opens; nodes that missed the sweep stay unroutable until first successful poll |
| DB down | Unchanged from today (readiness fails; auth/credit paths error) |
| Config file invalid | Startup fails fast with a parse error |

## What deliberately does not change

- **Rate limiting** stays in-memory — still correct with exactly one gateway.
- **Credits** (reserve/settle/holds), **auth**, **sessions**, **admin mutations** — all
  already DB-transactional and untouched. (Only the *ordering* of resolve-vs-reserve
  in the request flow changes, per above.)
- **Sweeper** unchanged (single process).
- **Usage writer** unchanged (channel → Postgres), only the entry struct and insert
  column list grow.

## Rollout plan

PR-sized, tests-first, in dependency order:

1. **Store layer**: `nodes` schema (CHECK constraints, `health_path`,
   `timeout_seconds`) + CRUD with `updated_at` maintenance + masked-secret handling +
   `usage_logs.node_id`. Pure store tests.
2. **Registry**: snapshot-based registry (`atomic.Pointer`, race tests), startup sweep,
   discovery/health poller with fake HTTP backends, state-machine tests, `OLLAMA_URL`
   explicit-presence synthesis rules, `NODES_FILE` loading + merge semantics.
3. **Routing**: proxy handler takes `Registry`; model validation (400s); resolve-before-
   reserve ordering; round-robin; upstream client hardening (redirect denial + body
   caps, both clients); per-node timeout deadlines; usage attribution;
   `tokens_total{node}`; `/v1/models` intersection + `MODELS_LIST_ALL`; readiness
   change.
4. **Admin API**: nodes CRUD + refresh + immediate-probe-on-write; admin health
   integration; `node_id` filter on usage endpoints; config snapshot additions.
5. **Docs & packaging**: README rewrite for multi-node, docker-compose example
   (gateway + Postgres + two Ollama nodes), deployment guide (k3s pod with `Recreate`
   strategy + native macOS pattern).

Admin frontend (Nodes page) follows as a separate design in the frontend repo.

## Resolved questions (2026-07-07)

1. `/v1/models` availability filter → **configurable** via `MODELS_LIST_ALL`
   (default `false` = intersection). See the `/api/v1/models` section.
2. Config file format → **JSON** (zero-dependency, stdlib-parsed). Revisit YAML only
   if OSS feedback demands it.
3. Per-node request timeout → **in v1**: `timeout_seconds` on nodes, applied as a
   per-request context deadline; default remains 5 minutes. See client hardening.

## Future roadmap (explicitly deferred)

Gateway replication (requires shared rate-limit state) · weighted/least-loaded routing ·
model aliases (`gpt-4o` → local model) · fallback chains (node down → cloud) · push-mode
agents for NAT'd nodes · embeddings endpoint · `NODES_DENY_CIDRS` SSRF hardening ·
per-node chat-path override if a non-`/v1` backend appears.

## Review log

- **2026-07-07 Codex review (11 findings: 2 blockers, 7 major, 2 minor)** — all
  addressed in v2: startup sweep replaces optimistic routing (B1); resolve-before-
  reserve (B2); redirect/body-cap hardening on the chat client, not just probes (M3);
  `OLLAMA_URL` explicit-presence synthesis (M4); `health_path` for static-model nodes
  (M5); `Registry.Snapshot()` + explicit `LogUsage`/metrics threading, requests_total
  kept node-free (M6); copy-on-write snapshot + bounded counters (M7); readiness
  derived from snapshot with zero-node = ready (M8); path-joining `base_url` semantics
  + registration validation (M9); model validation 400s (M10); CHECK constraints,
  header-injection validation, `updated_at` in helpers (M11).
- **2026-07-07 Codex verification pass** — confirmed v2 closed the original findings;
  raised 4 follow-ups, all addressed in v3: dropped implicit localhost synthesis (it
  contradicted zero-node-ready readiness — explicit `OLLAMA_URL` is now required for
  the single-node path); `health_path` restricted to path-only with registration
  validation; round-robin counters moved outside the snapshot so republishes don't
  reset balancing; middleware-vs-routing ordering (session-token limit before
  `Resolve`) stated explicitly.

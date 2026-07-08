# Self-hosting example

A complete local stack: the gateway, Postgres, and two Ollama backend nodes
declared via `NODES_FILE` (`nodes.json`).

- **ollama-a** — plain auto-discovery node: the gateway probes `GET /api/tags`
  and routes to whatever models you pull into it.
- **ollama-b** — demonstrates `static_models`: discovery is disabled, the
  declared list (`llama3.2:1b`) is authoritative, and the gateway only
  health-checks the node.

Host ports are non-standard on purpose (gateway on **18080**) so the stack
never collides with services already running on your machine.

## 1. Start the stack

```bash
cd deploy/examples
export ADMIN_KEY=$(openssl rand -hex 24)   # or any secret you like
docker compose build
docker compose up -d
```

Wait for readiness (DB ok + usage writer ok + at least one healthy node):

```bash
curl -s http://localhost:18080/api/healthz/ready | jq
```

Check what the gateway discovered — both nodes should be `healthy`
(ollama-a with an empty `models` list until you pull something; ollama-b
pinned to its static list):

```bash
curl -s -H "X-Admin-Key: $ADMIN_KEY" http://localhost:18080/api/admin/nodes | jq
```

## 2. Pull a model

Pull a small model into the auto-discovery node:

```bash
docker compose exec ollama-a ollama pull smollm2:135m
```

The poller re-discovers models within ~15 seconds, or force it immediately
(use the node `id` from the listing above):

```bash
curl -s -X POST -H "X-Admin-Key: $ADMIN_KEY" \
  http://localhost:18080/api/admin/nodes/1/refresh | jq '.data.health, .data.models'
```

If you want to route to **ollama-b**, pull its declared model there first —
with `static_models` the gateway trusts the list, it cannot verify it:

```bash
docker compose exec ollama-b ollama pull llama3.2:1b
```

## 3. Create an API key and chat

```bash
KEY=$(curl -s -X POST -H "X-Admin-Key: $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"demo","rate_limit":60}' http://localhost:18080/api/admin/keys | jq -r .key)

curl -s http://localhost:18080/api/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"smollm2:135m","messages":[{"role":"user","content":"Say hi in five words."}]}' | jq
```

Admin-created keys are not bound to a credit account, so they bypass the
credit gate and the pricing allowlist — handy for smoke tests.

## 4. Pricing and `/v1/models`

`GET /api/v1/models` lists the intersection of *actively priced* models and
models *served by a healthy node*. The gateway seeds a default pricing
catalog, but your model probably isn't in it — add pricing so it shows up:

```bash
curl -s -X POST -H "X-Admin-Key: $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"model_id":"smollm2:135m","prompt_rate":0.001,"completion_rate":0.001,"typical_completion":300}' \
  http://localhost:18080/api/admin/pricing

curl -s -H "Authorization: Bearer $KEY" http://localhost:18080/api/v1/models | jq
```

Pricing is also **required** for any credit-backed key (keys created for user
or service accounts): requests for unpriced models are rejected with `400
unknown_model`, and the account needs a positive credit balance
(`POST /api/admin/accounts/{id}/credits`, or set `DEFAULT_CREDIT_GRANT`).

## 5. Per-node usage attribution

Every usage row records which node served it:

```bash
curl -s -H "X-Admin-Key: $ADMIN_KEY" \
  "http://localhost:18080/api/admin/usage?node_id=1" | jq
```

## Adding a cloud (OpenAI-compatible) node

JSON does not allow comments, so the third-node example lives here instead of
in `nodes.json`. Append this entry to the `nodes` array, set `CLOUD_KEY` in
the gateway's environment (uncomment it in `docker-compose.yml`), and restart —
`${VAR}` references are expanded from the environment at load time so the
secret never lives in the file:

```json
{
  "name": "cloud",
  "base_url": "https://api.example.com",
  "backend_type": "openai_compat",
  "auth_header": "Bearer ${CLOUD_KEY}",
  "static_models": ["gpt-4o-mini"]
}
```

Note `base_url` excludes the `/v1` segment — the gateway appends it.

## Cleanup

```bash
docker compose down -v   # -v also removes the Postgres and model volumes
```

## Going multi-machine

See [`docs/deployment.md`](../../docs/deployment.md) for running nodes on
separate machines: k3s pod patterns, native macOS (Apple Silicon) nodes,
network topologies, and Ollama memory tuning.

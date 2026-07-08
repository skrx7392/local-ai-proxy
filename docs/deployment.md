# Deployment guide: multi-node operation

How to run the gateway and its backend nodes across real machines. For a
single-machine quickstart, use [`deploy/examples/`](../deploy/examples/).

A **node** is anything the gateway can reach over HTTP that speaks the Ollama
API (`backend_type: "ollama"`) or the OpenAI API (`backend_type:
"openai_compat"`). The gateway needs *inbound* HTTP reachability to every
node — there are no push-mode agents (yet), so NAT'd machines must be reached
via an overlay network or a reverse proxy.

All hostnames and addresses below are placeholders (`example.internal`,
`192.0.2.x`, `100.64.x.x`) — substitute your own.

## The hard rule

> **Never expose a bare Ollama (or any unauthenticated backend) to the public
> internet.** Ollama has no authentication: anyone who can reach the port can
> run inference, pull models, and delete models. Put every node either on a
> private network (LAN, VPN, overlay) or behind a reverse proxy that enforces
> a token — the gateway sends a per-node `auth_header` for exactly this.

## Network patterns

### Same host

Gateway and node on one machine. Use `OLLAMA_URL=http://localhost:11434`
(the single-node shortcut) or a `NODES_FILE` entry pointing at localhost.
Inside Docker Compose, use service names (`http://ollama-a:11434`). From a
container to a natively-running host process, use
`http://host.docker.internal:11434` (Docker Desktop) or the host's LAN/bridge
address (Linux).

### LAN

Nodes on a trusted private network:

```json
{ "name": "gpu-box", "base_url": "http://192.0.2.10:11434", "backend_type": "ollama" }
```

Bind Ollama to the LAN interface (`OLLAMA_HOST=0.0.0.0` on the node) and
firewall the port to the gateway's address. "Trusted LAN" means *you* trust
every device on it; on shared networks prefer an overlay.

### WireGuard / Tailscale overlay

The recommended pattern for machines in different locations (home
workstation + cloud VM + laptop). Every machine joins the overlay and gets a
stable private address; the gateway reaches nodes by that address, and
nothing is exposed publicly:

```json
{ "name": "workstation", "base_url": "http://100.64.0.10:11434", "backend_type": "ollama",
  "timeout_seconds": 900 }
```

Tailscale MagicDNS names (`http://workstation.example.ts.net:11434`) work
too — the gateway resolves them like any hostname.

### Reverse proxy with token (`auth_header`)

When a node must be reachable over an untrusted network, front it with a
reverse proxy (Caddy, nginx, Traefik) that terminates TLS and requires a
bearer token, and give the gateway that token:

```json
{ "name": "remote-gpu", "base_url": "https://ollama.example.com",
  "backend_type": "ollama", "auth_header": "Bearer ${REMOTE_GPU_TOKEN}" }
```

The gateway sends `auth_header` as the `Authorization` header on every probe
and every forwarded request, never logs it, masks it on all admin reads, and
never follows upstream redirects (so the credential cannot be exfiltrated by
a redirecting endpoint). TLS with a private CA works via the standard
`SSL_CERT_FILE` mechanism on the gateway.

## Pattern: Ollama node on k3s/Kubernetes

Suitable for Linux boxes with (or without) GPUs that are already cluster
members. The important, non-obvious parts:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama-gpu-box
spec:
  replicas: 1
  # Recreate, NOT RollingUpdate: a rolling update briefly runs two pods, and
  # two Ollama processes loading the same model will OOM a machine sized for
  # one copy. Take the brief downtime instead.
  strategy:
    type: Recreate
  selector:
    matchLabels: { app: ollama-gpu-box }
  template:
    metadata:
      labels: { app: ollama-gpu-box }
    spec:
      # Pin the pod to the machine that owns the model storage and the
      # memory/GPU budget. Models are multi-GB; you do not want the scheduler
      # moving this pod and re-pulling everything.
      nodeSelector:
        kubernetes.io/hostname: gpu-box.example.internal
      containers:
        - name: ollama
          image: ollama/ollama:latest
          ports: [{ containerPort: 11434 }]
          env:
            - name: OLLAMA_KEEP_ALIVE
              value: "-1"          # single-model node: keep it resident
          resources:
            # Be GENEROUS with memory limits. Ollama mmaps model files, and
            # page-cache pages for mmap'd files are charged to the container's
            # cgroup — so the limit must fit the model weights PLUS KV cache
            # and runtime overhead, not just the process heap. A limit sized
            # to "the process" gets the pod OOM-killed mid-load.
            requests: { memory: "6Gi" }
            limits:   { memory: "10Gi" }   # ~2x the model size is a sane start
          volumeMounts:
            - { name: models, mountPath: /root/.ollama }
      volumes:
        - name: models
          persistentVolumeClaim:
            claimName: ollama-gpu-box-models
---
apiVersion: v1
kind: Service
metadata:
  name: ollama-gpu-box
spec:
  selector: { app: ollama-gpu-box }
  ports: [{ port: 11434, targetPort: 11434 }]
```

Model storage: use a node-local PV (`local-path` on k3s, or an explicit
`hostPath`) so model blobs survive pod restarts and never traverse the
network. With `nodeSelector` pinning, a node-local volume is exactly right —
the pod can't move anyway.

Register the node with the cluster-internal DNS name:

```json
{ "name": "gpu-box", "base_url": "http://ollama-gpu-box.models.svc.cluster.local:11434",
  "backend_type": "ollama" }
```

(gateway inside the same cluster), or via the overlay/LAN address of the
machine if the gateway runs elsewhere.

## Pattern: native macOS (Apple Silicon)

**Metal GPU acceleration is not available inside Linux containers or VMs.**
Docker on macOS runs a Linux VM, so a containerized Ollama on a Mac is
CPU-only — a waste of exactly the hardware that makes Apple Silicon machines
good inference nodes. Run Ollama **natively** (`brew install ollama`, or the
app) and register the machine as a node over your overlay/LAN:

```bash
# On the Mac: listen beyond localhost (choose the interface deliberately)
launchctl setenv OLLAMA_HOST 0.0.0.0
launchctl setenv OLLAMA_KEEP_ALIVE -1
# restart the Ollama app / brew service after changing these
```

```json
{ "name": "macbook", "base_url": "http://100.64.0.20:11434", "backend_type": "ollama",
  "timeout_seconds": 600 }
```

Use the tailnet/VPN address, not a public one (see the hard rule). Laptops
sleep and roam: expect the node to go `unhealthy` and come back — the gateway
routes around it automatically and re-lists its models when it returns. Give
big models on battery-throttled hardware a generous `timeout_seconds`.

## Ollama tuning for gateway duty

- **`OLLAMA_KEEP_ALIVE=-1` on single-model nodes.** The default unloads a
  model after 5 idle minutes, so the first request after a quiet period eats
  a full model load (tens of seconds). A node dedicated to one model should
  keep it resident permanently.
- **`OLLAMA_NUM_PARALLEL=1` (or 2) on low-memory multi-model nodes.** Each
  parallel slot multiplies KV-cache memory. On a machine juggling several
  models, cap parallelism so a burst of concurrent requests can't OOM it.
- **Placement rule: never co-locate two hot models on a machine that cannot
  hold both in memory simultaneously.** Two frequently-used models on one
  box that fits only one means constant load/unload thrash — every switch
  costs a full model load. Split hot models across nodes (the gateway routes
  by model, so this is the natural topology) and reserve multi-model nodes
  for machines with headroom or for cold/rarely-used models.
- **`timeout_seconds`** (per node, in the registration): raise it well above
  the default 5 minutes for slow CPU-only nodes running large models —
  a long non-streaming completion can legitimately exceed the default.

## Operational notes

- **Health cadence**: every enabled node is probed roughly every 15s (±20%
  per-node jitter). A node is marked unhealthy after 2 consecutive failures
  and healthy again on the first success. `POST /api/admin/nodes/{id}/refresh`
  forces an immediate probe.
- **Gateway restarts** run a parallel discovery sweep (~6s budget) before the
  listener opens, so routing is deterministic immediately; a node that misses
  the sweep gets no traffic until its first successful poll.
- **Zero nodes is a valid state**: the gateway reports ready and serves the
  admin API so you can register the first node. Chat requests return
  `503 model_unavailable` until a healthy node serves the model.
- **Monitoring**: watch `aiproxy_node_up{node="..."}` (per-node gauge) and
  the `nodes` section of `GET /api/admin/health`. Per-node usage attribution
  is available via `GET /api/admin/usage?node_id=...`.

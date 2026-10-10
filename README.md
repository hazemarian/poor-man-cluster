<p align="center">
  <img src="docs/assets/logo-lockup.svg" alt="Poor Man's Cluster" width="680">
</p>

<p align="center"><strong>SIMPLE • FLEXIBLE • POWERFUL</strong></p>

# Poor Man's Cluster

A simple, cost-effective, production-ready deployment stack on open-source tools. No Kubernetes, no managed cloud services, no expensive licensing — just Docker Swarm, a small Go control plane (`pmcluster`), and a handful of well-chosen tools that get the job done.

This project gives you a full production cluster with HTTPS ingress, observability, programmatic deployments, automated backups, and a single CLI to manage it.

The control plane is a single static Go 1.25 binary (`pmcluster`, ~25 MB, no cgo) that brings the cluster up, deploys applications via a small DSL with versioned rollbacks, accepts HMAC-verified webhooks from CI, manages registry credentials and bootstrap passwords, and ships its own JSON audit logs.

In front of it sits **`pmcluster-edge`** — a small Go service deployed as a Swarm service that publishes `pmcluster.<domain>` as the single public origin for the **operator console** (a web UI for API keys, TLS, webhooks, stacks, and more), the REST API, and webhook receivers — all shielded by per-IP rate limiting, a connection shield, and automatic IP blocklisting.

- **Design + trade-offs:** [RFC v2 — issue #1](https://github.com/hazemarian/poor-man-cluster/issues/1) (what actually shipped)
- **Current release:** [v0.2.179](https://github.com/hazemarian/poor-man-cluster/releases)

---

## How It Works

The stack runs on Docker Swarm. One machine acts as the **manager node** — it controls the cluster, hosts the management UIs, and runs `pmcluster`. Any number of **worker nodes** can join with `docker swarm join`; the manager automatically schedules global services on them.

Two overlay networks connect everything:

- **`traefik-net`** — application traffic between Traefik and your services
- **`monitoring-net`** — telemetry (logs, metrics, traces) between services and OpenObserve

All sensitive credentials are stored **encrypted (AES-256-GCM) in `pmcluster`'s SQLite** — the DB is the source of truth — and mirrored into Docker Swarm secrets as **content-addressed objects** (`<name>_<sha256-first-8>`; the same value always reuses the same object, a rotation mints a new one and the old in-use secret stays mounted). Every `cluster update` rebuilds any swarm object that went missing from the DB index. Platform configs (Traefik dynamic, OTel collector, observability stack, etc.) are stored **only in the DB** — no `~/.pmcluster/config/*.yml` disk files. On each `cluster update`, the five platform stacks (themselves DSL manifests with `platform: true`) are rendered through the same pipeline as app stacks, hashed, compared against the stored `rendered_hash` **and** against the live swarm — only changed/missing stacks are re-deployed. Drift-prune removes services that were dropped from a compose. The bootstrap admin passwords for Traefik/OpenObserve/edge are generated randomly on first `cluster up` — no `.env` editing required.

```
Internet
   │
   ▼
Traefik (HTTPS ingress; global on every MANAGER — the swarm provider needs the
        manager socket — fronted by the ingress routing mesh from every node)
   ├──▶ pmcluster.<domain>
   │        ├── /web/*  ──▶ pmcluster-edge ──▶ operator console (gin + HTMX)
   │        │             gated by admin-auth (htpasswd) or sso-auth (forwardAuth)
   │        └── /api/*  ──▶ pmcluster-edge ──▶ pmcluster daemon (host.docker.internal:9090)
   ├──▶ observ.<domain> ──▶ OpenObserve
   │        gated by admin-auth + openobserve-auto-auth (no login prompt)
   ├──▶ traefik.<domain>/dashboard/ ──▶ Traefik dashboard
   │        gated by admin-auth or sso-auth
   ├──▶ sso.<domain> ──▶ oauth2-proxy (SSO, optional)
   └──▶ Your App(s)

Your App(s) ──OTLP──▶ OTel Collector ──▶ OpenObserve
Traefik     ──OTLP──▶ OTel Collector ──▶ OpenObserve
pmcluster-edge shields: per-real-IP rate limits, in-flight shield, timeouts,
                        body caps (413), auto-ban of abusive IPs (403)

Every node:    OTel Collector (global)
Storage nodes: offen backup agent (hourly; double-write to the in-cluster
               SeaweedFS store + the offsite S3/R2 target)
One non-storage node: the SeaweedFS backup store (S3 :8333 on the routing
               mesh, WebDAV :7333 cluster-internal)
```

---

## Stack Components

### Traefik — Ingress & API Gateway
Sits at the edge and routes HTTPS traffic to the right service based on Docker labels (for swarm services) and a file provider (for the pmcluster route + TLS certificates). TLS certs are loaded from Swarm secrets (content-addressed, Raft-replicated). Built-in OpenTelemetry support sends traces and metrics to the collector. Deployed `mode: global` **pinned to manager nodes** — the swarm provider lists services through the local Docker socket, which only a manager can read — while the **ingress routing mesh** fronts it from every node: 80/443 are published in ingress mode, so any node that receives traffic forwards it to a manager's Traefik task.

### pmcluster operator console — Service & Platform UI
Web UI (served by `pmcluster-edge` on the `pmcluster.<domain>` origin) for day-to-day operations: view stacks and **services** (live replica health, task/crash history, tailed logs), **restart** services, run one-off `exec` commands or a full browser **terminal**, manage webhooks/API keys/TLS/backups, manage **users** with **RBAC roles** (admin > operator > viewer), and edit cluster settings. Platform services (edge/traefik/observability/backup/sso) have their own read-only **Platform** page (`/web/platform`) and a separated panel on the Services page — customer apps are never mixed in with them. The console lives under `/web` and is gated by Traefik's `admin-auth` (htpasswd) middleware — or by `sso-auth` (oauth2-proxy forwardAuth) when GitHub SSO is enabled. When behind the auth gate, the console's own login page is disabled (`EDGE_LOGIN_DISABLED=true`) and the Users CRUD is hidden from the nav. No Portainer — its role is fully covered here plus the `pmcluster service` CLI. The console carries the Poor Man's Cluster brand identity: a six-palette accent system with a palette picker (persisted before first paint) and the inlined combination-mark logo.

### oauth2-proxy (SSO, optional)
When GitHub SSO is enabled via `pmcluster setup --sso-enabled`, an `oauth2-proxy` sidecar runs in its own stack (`sso.<domain>`) on the manager. It performs GitHub OAuth (with optional org restriction) and exposes `/oauth2/*` endpoints. Traefik's `sso-auth` forwardAuth middleware replaces `admin-auth` on the console (`/web/*`), OpenObserve (`observ.<domain>`), and the Traefik dashboard (`traefik.<domain>`). The `/oauth2/*` path resolves on every gated host so the OAuth redirect loop works correctly. The cookie is shared across all `<domain>` subdomains (`OAUTH2_PROXY_COOKIE_DOMAINS`). When SSO is on, the edge console's own login is disabled — you authenticate through GitHub.

### OpenObserve — Observability (Logs, Metrics, Traces)
Lightweight all-in-one observability platform. Single binary, one UI, ~140× lower storage cost than Elasticsearch-based stacks. Receives OTLP from the OTel Collector.

No provisioning API or automation user — the OTel collector and the Traefik auto-auth middleware both use the **root admin credentials** (the `openobserve_admin` managed credential / `zo_root_user_password` Swarm secret). OpenObserve's own login page is never shown: it is gated by `admin-auth` (or `sso-auth`) plus a static `openobserve-auto-auth` middleware that injects a Basic `Authorization` header on every request.

### OpenTelemetry Collector — Telemetry Aggregation
Runs as a global service on every node. Auto-discovers containers via the Docker observer, tails their logs, enriches with Swarm metadata, collects Docker resource metrics, forwards everything to OpenObserve. The pipeline config is generated by pmcluster and shipped as a Docker config (replicated to every node by Swarm itself).

### Backup stack — offen agents + the in-cluster SeaweedFS store
The `backup` platform stack runs **offen/docker-volume-backup** agents plus (when enabled) a headless **SeaweedFS** object store:

- **Volume agent** — one per **storage node** (`pmcluster.storage`-labeled; the leader always qualifies), or on **every** node when `backup_all_nodes=true`. Snapshots the whole volume root (`<volume_root>`, default `/var/stack/data`) on the `backup_cron` schedule — **hourly by default** — into `backup-<nodeID>-<ts>.tar.gz`, pruned after `backup_retention_days` (default 15).
- **Control-plane agent** — manager/platform-node pinned, daily 03:00, 30-day retention: archives `~/.pmcluster` itself (`data.db` + `.encryption_key` + `config.yaml` + the rendered config tree) under a `pmcluster-ctlplane-` prefix, so a lost manager disk ≠ a full re-bootstrap.
- **In-cluster SeaweedFS store** — S3 API on **:8333** (published on the routing mesh, reachable at `127.0.0.1:8333` on every node), WebDAV on **:7333** (cluster-internal, the agents' write leg), pinned to a **non-storage, non-platform node** so the durable copy lives in a different failure domain than the data. Credentials are the `seaweedfs_admin` managed credential; buckets auto-create on first upload.
- **Offsite double-write** — when the `backup_s3_*` settings are configured, every agent writes each archive to BOTH the in-cluster store (WebDAV) and the offsite S3/R2 target (offen's native `AWS_*` backend); offen's own pruning keeps both copies in lockstep. No separate replicator service.

Store and offsite archives are **discovered** into `pmcluster backup list` and the console (control-plane archives excluded), so the full backup picture is visible from any node. See [`docs/storage-and-databases.md`](docs/storage-and-databases.md) for the whole story — restores, cross-node moves, storage failover.

### pmcluster-edge — Smart Proxy & Operator Console
A small Go service (gin + HTMX) deployed as the `pmcluster-edge` Swarm service on the manager. It owns the `pmcluster.<domain>` origin end to end:

- **Operator console** — a web UI for managing API keys, users, webhooks, per-host TLS certificates, stacks (deploy/rollback/delete/move), backups, and cluster settings. When the console's own login is enabled (standalone), the first admin account is created on first run; in the normal swarm deployment the Traefik gate (`edge_admin` credential) is the authenticator and the console runs as a synthetic admin.
- **Smart reverse proxy** — everything the console doesn't handle is proxied to the pmcluster daemon (`host.docker.internal:9090`). The daemon's host-bound port is no longer exposed publicly.
- **Edge hardening** — per-real-IP token-bucket rate limits (separate buckets for `/api/*` and `/webhook/*`), an in-flight request shield (503 when saturated), request timeouts, body size caps, and automatic IP blocklisting (403) after repeated abuse or upstream failures.

Its image is built from `cmd/edge/` and published to GHCR; the embedded `edge-stack.yml` pins the version-keyed tag so `cluster up`/`cluster update` can detect and roll out upgrades.

### pmcluster — Control Plane (Go)
Single static binary, lives on the manager host. Replaces the bash setup script and owns deployment end to end.

- `pmcluster init` — creates `~/.pmcluster/` (SQLite + encryption key) and prints a one-time bootstrap admin token for the API
- `pmcluster setup` — **interactive wizard**: collects domain, TLS (LE or BYO), Traefik admin user, SSO enable (GitHub creds + org + optional repo restriction), edge login preference, volume root, backup-store placement (`backup_store_on`), and — when offsite S3 is configured — automatic storage failover; the node hostname is applied via `hostnamectl` so the Swarm records the name your `placement:` pins refer to; then runs `cluster up` (fresh) or `cluster update` (existing). All questions have matching flags for scripted use.
- `pmcluster cluster up` — **init-only**: brings a fresh cluster up (prevents re-running if a cluster already exists — run `cluster update` instead). Configs are stored in the DB only (no `~/.pmcluster/config/*.yml` disk files); rendered at deploy time from embedded templates. The five platform stacks are themselves DSL manifests (`platform: true`) rendered through the same pipeline as app stacks.
- `pmcluster cluster update` — **content-aware, self-healing reconcile**: DB is the source of truth. Compares rendered compose hashes (`rendered_hash`) against stored values AND verifies each platform stack is actually present in the swarm (a wiped/half-deployed swarm is rebuilt); re-deploys only changed/missing stacks; re-ensures overlay networks, re-materializes managed credentials, rebuilds missing configs/secrets from the DB index, detects a wiped Swarm (changed swarm ID), and keeps the storage-node labels in sync with the `storage_nodes` setting (the leader is always a storage node; labeled-but-unlisted nodes are adopted).
- `pmcluster cluster reset [--restore <archive>]` — non-destructive rebuild of the **Swarm side** from the local store (networks, secrets, configs, platform stacks, storage labels; never rotates credentials, never touches volumes). `--restore` first extracts a `cluster down --purge` backup tarball (`purge-backup-<ts>.tar.gz`) back into the data dir.
- The **control loop** (v0.2.132) — the Swarm-leader daemon runs a background reconcile loop (event-driven + a `reconcile_interval` safety tick, default 60s, `0` disables). Each pass re-renders the platform configs and re-deploys drifted stacks, re-translates each app stack's latest source and syncs when its rendered hash drifted (plus a live-swarm check — every service carries an `io.pmcluster.rendered_hash` label, so manual `docker service update`-style drift is caught too), and writes a per-stack/per-service health snapshot to the `stack_status` table. One pass at a time; the loop never restarts services on health — it only reports. Stacks whose storage node is down are paused (or, with `storage_failover=true`, moved to a healthy storage node); stacks whose latest deploy failed are marked accordingly.
- `pmcluster cluster settings` — list all cluster settings (KEY/VALUE; secret-typed keys masked); `pmcluster cluster settings get <key>`; `pmcluster cluster settings set key=value ...` (changes are applied to the swarm by the next `cluster update` — or the console's **Apply to swarm**)
- `pmcluster cluster status` / `cluster down [--yes] [--purge]` — `--purge` removes pmcluster-managed secrets, configs, networks AND the local store, after writing a restorable `purge-backup-<ts>.tar.gz` (restore with `cluster reset --restore`)
- `pmcluster serve` — runs the long-running daemon (REST API + webhook receiver). Listens on `0.0.0.0:9090` (all interfaces, so the edge container reaches it via `host.docker.internal:9090`); Traefik routes `pmcluster.<domain>` to `pmcluster-edge`, which proxies `/api/*` and `/webhook/*` to the daemon
- `pmcluster deploy <file>` / `pmcluster stack list|show|badge|move|ack` / `pmcluster rollback <stack> <rev>` — DSL-based application deploys with versioned rollback; `badge <stack>` prints README status-badge markdown (`--services` for the combined per-service badge); `move <stack> --to <node>` relocates a stateful stack's storage; `ack <stack>` acknowledges a storage failover (clears the amber badge)
- `pmcluster credentials list|show|rotate` — managed bootstrap passwords (Traefik / OpenObserve / edge / seaweedfs); `rotate` is atomic — the new versioned swarm secret is created before the store row is updated, the old secret stays mounted, and a self-heal step rebuilds missing mounts
- `pmcluster service list|ps|tasks|logs|restart|exec` — whitelisted service operations (replica health, crash history, log tailing, restart, non-interactive exec) working locally or over the remote API; `list` marks platform services in a `PLATFORM` column and `--platform` filters to them
- `pmcluster node list|join-token|promote|demote` — swarm node reads plus storage-node lifecycle: `promote <hostname>` registers a storage node (`storage_nodes` setting + `pmcluster.storage` label; workers qualify), `demote` removes one (the leader is refused — it must stay a storage node)
- `pmcluster registry add|list|remove` — Docker registry credentials, replayed on `serve` startup so private images keep pulling
- `pmcluster webhook add|list|remove|deliveries` — HMAC-signed webhook sources for CI integrations (timestamped to prevent replay); `deliveries <source>` shows newest-first delivery history (ID/STATUS/STACK/REVISION/REPO/FILE/WHEN/ERROR + retry count)
- `pmcluster tls hosts add|list|remove` — per-host TLS certificates for customer domains served by Traefik (independent of the cluster wildcard cert)
- `pmcluster tls site show|set` — inspect or rotate the cluster's own (main) certificate in place, with expiry metadata
- `pmcluster backup create|list|browse|restore` — on-demand offen volume snapshots (run on a storage node); `browse <id>` lists files inside a backup archive (TYPE/SIZE/PATH); `restore <id>` extracts a SUCCEEDED, stack-scoped backup under `dest_root/<stack>` — `--volume <name>` for a single volume, `--from-s3` to force the fetch from the offsite bucket — and routes the restore to the node that OWNS the stack's volume when it lives on another node
- `pmcluster secret create|edit|list|show|verify|delete|heal` — DB-backed secrets (AES-256-GCM encrypted, shown as hashes); `edit` mirrors the new value into a content-addressed swarm secret; `heal` verifies every secret decrypts with the current key, repairs stale hashes and re-materializes missing swarm mirrors
- `pmcluster config create|list|get|edit|history|rollback|delete` — DB-backed configs with version history, mirrored to content-addressed swarm configs
- `pmcluster user create|list|remove` — issue and manage API tokens for additional users (`--stack <name>` scopes a token to one stack); tokens print once, hashed at rest; CLI-created keys are admin-tier, console-minted API keys are operator-tier
- `pmcluster usage` — secret/config usage graph: CONFIG/USED BY + SECRET/USED BY
- `pmcluster logs [--tail=N] [--since=24h] [--follow]` — tail the JSON audit log at `~/.pmcluster/logs/`. Files rotate daily, swept after 14 days.
- `pmcluster version` — prints version, commit, and build date (also accessible via `--version` flag)

---

## Repository Structure

The Go code follows a **domain-first (DDD) layout**: one package per bounded
context (`apikeys`, `webhooks`, `secrets`, `configs`, `backups`, `certs`,
`stacks`, `cluster`), each owning its model types, port (interface), local
adapter and REST handler. Consumers depend only on ports; `internal/cli` and
`internal/server` are the two composition roots; `internal/remote` is a single
REST-client family that implements the same ports against the daemon API (so
the CLI can run off-node with `--api-url`). See
`pmcluster/internal/ARCHITECTURE.md` for the dependency-rule and domain
inventory.

```
poor-man-cluster/
├── pmcluster/                          # Go control plane (Cobra CLI + HTTP daemon)
│   ├── cmd/pmcluster/                  # entry point
│   ├── cmd/edge/                       # pmcluster-edge binary (console + smart proxy)
│   ├── internal/
│   │   ├── cli/                        # Cobra command tree + backend.go (the single
│   │   │                               #   local-vs-remote switchpoint)
│   │   ├── server/                     # daemon composition root (chi; Deps = ports only)
│   │   ├── api/                        # infra endpoints only (health, me, nodes, cluster/info)
│   │   ├── remote/                     # one shared REST-client family (off-node adapters)
│   │   ├── apikeys/ webhooks/ secrets/ configs/ backups/ certs/ stacks/ cluster/
│   │   │                               # domain packages: model + port + local + http
│   │   ├── workflow/                   # named-step runner (up, update, reset, deploy 6, …)
│   │   ├── edgeproxy/                  # rate limit, shield, blocklist, real IP, proxy
│   │   ├── ui/                         # operator console (gin + HTMX, controllers + templates)
│   │   ├── docker/                     # SDK wrapper behind a small interface
│   │   ├── cluster/embeds/             # platform stack DSL manifests + configs (//go:embed)
│   │   ├── config/, store/, auth/      # config, SQLite, bearer-token auth
│   │   ├── credentials/                # AES-GCM encryption for stored creds
│   │   └── buildinfo/                  # version/commit/date with VCS fallback
│   ├── docs/                           # openapi.yaml, service-ops-design.md, restore-design.md, …
│   ├── migrations/                     # *.sql (0001..0025), applied lexicographically and tracked in schema_version
│   └── e2e/                            # smoke end-to-end tests
├── docs/
│   ├── dsl.md                          # Deploy DSL reference
│   ├── webhook.md                      # Webhook integration guide for CI
│   ├── storage-and-databases.md        # Storage & database architecture guide
│   ├── network-topology.md             # Topology options, firewall, DNS & load-balancer routing
│   ├── control-loop-design.md          # Control-loop architecture & extension design
│   ├── security-and-improvements.md    # Security findings & hardening tracker
│   ├── improvements.md                 # Shipped improvements + remaining backlog
│   └── test-reports/                   # per-test-case field reports (historical)
├── harden-host.sh                      # one-shot host hardening (ufw, DOCKER-USER, fail2ban)
└── README.md
```

---

## Getting Started

### Prerequisites

On the manager node:
- Docker Engine 20.10+ installed (`docker --version`).
- Swarm: either initialise it yourself (`docker swarm init --advertise-addr <ip>`) or let `cluster up` / `setup` do it — on a first node that has never joined a Swarm, pmcluster runs `docker swarm init` transparently (advertise address auto-detected, or `--swarm-advertise-addr`, or the tailnet IPv4 with `--tailscale`). Nodes already in a Swarm are left untouched.
- TLS: pick one
  - **Let's Encrypt (recommended for new clusters):** point `*.<your-domain>` at the manager's public IP, ensure port 80 is reachable. Pmcluster + Traefik handle issuance and renewal.
  - **Operator-supplied cert + key:** any CA, including a wildcard from your existing internal/purchased PKI.

### 1. Install pmcluster

One-line install (latest release):

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | bash
```

The script picks the right `darwin|linux` × `arm64|amd64` archive from the [GitHub releases](https://github.com/hazemarian/poor-man-cluster/releases), verifies its SHA256, and drops the binary in `/usr/local/bin/pmcluster` (override with `PREFIX=…` or pin a version with `VERSION=v0.2.179`).

**With private registry credentials (GHCR, Docker Hub, etc.):**

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | \
  PMCLUSTER_REGISTRY="ghcr.io=my-username=ghp_abc123" bash
```

The `PMCLUSTER_REGISTRY` env var accepts comma-separated `host=user=token` entries. The script runs `docker login` and persists credentials encrypted via `pmcluster registry add` so workers can pull private images.

Or build from source (requires Go 1.25+):

```bash
cd pmcluster
make build           # → ./bin/pmcluster
sudo install -m 0755 bin/pmcluster /usr/local/bin/
```

### 2. Bring the cluster up

**Interactive (recommended):**

```bash
pmcluster init                          # creates ~/.pmcluster, prints admin token
pmcluster setup                         # interactive wizard — collects all settings
```

The setup wizard walks you through domain, TLS, Traefik admin user, SSO (optional GitHub OAuth), edge login preference, and the node hostname, then runs `cluster up` (fresh) or `cluster update` (existing cluster). All questions have flags for scripted use — see `pmcluster setup --help`.

**Manual / scripted:**

```bash
pmcluster init                          # creates ~/.pmcluster, prints admin token

# Let's Encrypt (recommended)
pmcluster cluster up \
  --domain=example.com \
  --acme-email=ops@example.com \
  --openobserve-email=ops@example.com

# OR operator-supplied cert
pmcluster cluster up \
  --domain=example.com \
  --openobserve-email=ops@example.com \
  --cert=/path/to/cert.pem \
  --key=/path/to/key.pem
```

`cluster up` is **init-only** — if a cluster already exists it will error and tell you to run `cluster update`. This prevents accidental re-bootstrapping. Configs are stored **only in the SQLite DB** (no `~/.pmcluster/config/*.yml` disk files); they are rendered at deploy time from embedded templates.

It will:
1. Preflight (Docker reachable, Swarm active, this node is a manager)
2. Ensure the storage root directories exist (`/var/stack/data`, `/var/stack/backup`)
3. Sync the platform config templates into the store (DB is the source of truth; stale rows refreshed, operator edits preserved)
4. Create the `traefik-net` and `monitoring-net` overlay networks
5. Configure TLS — either wire ACME into Traefik (HTTP-01 via the `:80` entrypoint) or load the operator's cert/key into Swarm secrets
6. Generate random bootstrap credentials (Traefik gate / OpenObserve root / the edge daemon token / the SeaweedFS store `seaweedfs_admin` / the SSO cookie secret when enabled), store encrypted in SQLite, mirror to Swarm secrets
7. Render the OTel + Traefik dynamic configs in-process and create them as Docker configs (Swarm replicates to every node)
8. Deploy the `infra`, `edge`, `observability`, `backup`, and (if SSO enabled) `sso` stacks via `docker stack deploy` — all five are DSL manifests (`platform: true`) through the same pipeline as app stacks
9. Wait for the platform services to become healthy
10. Snapshot the rendered configs into the store (with content hashes)
11. Persist the install state (domain, OpenObserve admin email, TLS mode) and seed the storage settings (`storage_nodes` ← the leader's hostname; `platform_node` defaults to the leader)

The bootstrap passwords are printed **once** at the end. Save them, or retrieve them later:

```bash
pmcluster credentials list
pmcluster credentials show openobserve_admin
```

Once DNS resolves, the dashboards are live at:
- `https://traefik.<your-domain>` — Traefik dashboard (gated by admin-auth or sso-auth)
- `https://observ.<your-domain>` — OpenObserve (gated by admin-auth + auto-auth header; no login prompt)
- `https://pmcluster.<your-domain>` — **pmcluster operator console + API/webhooks** (served by `pmcluster-edge`); the `/web/*` routes are gated by admin-auth or sso-auth, the `/api/*` and `/webhook/*` routes use their own Bearer / webhook-signature auth

Service-level operations (replica health, task history, logs, restart, exec)
are handled by pmcluster itself — see the `pmcluster service` CLI group and the
**Services** tab in the operator console. There is no Portainer.

### 3. Run the pmcluster daemon

The daemon is managed by the CLI itself. `cluster up`, `cluster update`, and
`join` install the systemd unit (`/etc/systemd/system/pmcluster.service`,
`ExecStart=… pmcluster serve`) and (re)start it — no manual unit management.
On non-Linux hosts (or for a foreground run) use:

```bash
pmcluster serve                         # foreground; ^C to stop
```

`serve` is **leader-aware**: on a multi-manager Swarm it serves only on the
Raft leader (elected by Docker itself) and stands by on the others, polling
every 15s; swarm **workers** stand by too (no control loop). At startup —
before the store is opened — the daemon restores the control plane from the
newest Raft-replicated `pmcluster_state_*` Docker config when the local DB is
missing or older (repairing a missing `.encryption_key` even when the DB is
current; a current DB is never clobbered); on promotion it only *publishes* a
fresh snapshot. Pre-L2 clusters fall back to the newest
`pmcluster-ctlplane-*.tar.gz` archive.

A reference unit template ships at [`pmcluster/contrib/systemd/pmcluster.service`](pmcluster/contrib/systemd/pmcluster.service).

Inspect the daemon's audit log:

```bash
pmcluster logs --tail=200               # most recent JSON lines
pmcluster logs --since=24h --follow     # stream new entries
pmcluster logs --tail=200 | jq 'select(.level=="error")'
```

JSON files live at `~/.pmcluster/logs/pmcluster-YYYY-MM-DD.log` and are swept after 14 days.

### 4. Use the operator console

`cluster up` deploys `pmcluster-edge`, so `https://pmcluster.<your-domain>` is a full management UI as soon as DNS resolves:

- **Stacks** — view deployed stacks and revision history, deploy new manifests, roll back, move a stateful stack to another node, acknowledge a storage failover; platform stacks (infra/edge/observability/backup/sso) are hidden — they are not user workloads
- **Services** — live replica health, task/crash history, log tailing, restart, exec, and a browser terminal per service; customer app services only
- **Platform** — a read-only page listing the platform-managed services (stacks, replica health, node placement), separate from customer apps
- **Overview** — cluster info plus the swarm nodes table with a per-node **storage pill** and promote/demote buttons (the storage-node lifecycle)
- **Webhooks** — create/list/remove CI webhook sources (secrets shown once) + delivery history
- **API keys** — issue and revoke bearer tokens for other users/CI systems (operator-tier)
- **TLS** — rotate the cluster's own (main) certificate and manage per-host certificates for customer domains, with expiry warnings
- **Backups** — trigger snapshots, browse archive contents, restore, and view the audit log (archives other nodes uploaded to the store included)
- **Users** — manage console accounts with RBAC roles (admin > operator > viewer); admins only. Hidden from nav when behind SSO/admin-auth.
- **Settings** — cluster settings (secret-ish values masked for non-admins), cluster-scope configs/secrets with **Apply to swarm**, and the read-only rendered platform configs (operator/admin only — they embed root credentials)
- **Preferences** — theme plus the brand palette picker (six accent palettes, persisted before first paint)
- **External** — nav links to `https://observ.<domain>` (OpenObserve) and `https://traefik.<domain>/dashboard/` (Traefik dashboard)
- **Status badges** — every stack has a public, no-auth status badge endpoint at `https://pmcluster.<domain>/api/public/badge/<stack>` (flat SVG: healthy / in progress / degraded / error / unknown / **failover**) that reads the control loop's `stack_status` DB snapshot; an unacknowledged storage failover dominates the badge until `stack ack` or a move-back. Paste it into any GitHub README — e.g. `![stack services](https://pmcluster.<domain>/api/public/badge/<stack>/services)` renders one badge with a segment per service. The console's stack page shows the copyable markdown (single + combined), and `pmcluster stack badge <stack>` prints it.

The console authenticates against a dedicated `edge` daemon user and stores its session state in its own SQLite volume. When the console is deployed behind Traefik's `admin-auth` or `sso-auth` gate (`/web/*`), its own login page is disabled (`EDGE_LOGIN_DISABLED=true`) — you authenticate at the Traefik level and the console runs as a synthetic admin session.

### 5. Add worker nodes (optional)

On each additional machine — install Docker, then a single command:

```bash
pmcluster join --role worker --token <TOKEN> --manager <MANAGER_IP>:2377
```

`pmcluster join` joins the Swarm, initialises the local pmcluster state (data dir, config, DB migrations), installs the systemd daemon unit and starts it, and verifies the joined role. Get the token from the manager:

```bash
docker swarm join-token worker
```

To join over a private WireGuard tailnet instead of the public IP — `pmcluster join --role worker --token <TOKEN> --manager <MANAGER_IP>:2377 --tailscale --tailscale-auth-key tskey-...` — the node joins the tailnet, advertises its tailnet IPv4 to the Swarm, and node-to-node traffic (2377/7946/4789) needs no firewall rules. Opt-in; fails loudly on tailnet errors. Read the tailnet caveats in [`docs/network-topology.md`](docs/network-topology.md) first (the ufw catch-all DENY and join-time advertise addresses).

Add the node as a **storage node** at join time with `--storage-node`: the joiner stamps the swarm-visible `pmcluster.storage` label, and the leader's next `cluster update` adopts it into the `storage_nodes` setting — stateful stacks can then round-robin onto it and a backup agent is scheduled there. (You can also do this later with `pmcluster node promote <hostname>`.)

The manager automatically schedules the OTel Collector on the new node (global), and the backup agent when it is a storage node. No script, no config file.

#### Required Firewall Ports (manager ↔ worker)

| Port | Protocol | Purpose |
|------|----------|---------|
| 2377 | TCP | Swarm cluster management |
| 7946 | TCP/UDP | Node-to-node communication |
| 4789 | UDP | Overlay network traffic (VXLAN) |

For the full topology options (single node → leader+workers → load-balanced HA), the DNS routing table per option, and how to point your domains at the cluster — see [`docs/network-topology.md`](docs/network-topology.md).

---

## Tear Down

```bash
pmcluster cluster down --yes              # removes infra/edge/observability/backup stacks
pmcluster cluster down --yes --purge      # also removes pmcluster-managed secrets, configs,
                                          # and overlay networks, and deletes the local store —
                                          # after writing a restorable purge-backup-<ts>.tar.gz
                                          # (data.db + .encryption_key + config.yaml) that is KEPT
```

Restore a purge with `pmcluster cluster reset --restore <purge-backup-archive>` — it extracts the store back into the data dir and rebuilds the Swarm side from it. `~/.pmcluster` is otherwise **not** removed by `--purge`'s backup step; deleting the directory manually (including the kept backup archive) makes encrypted credentials unrecoverable.

---

## Deploying Your Own Services

`pmcluster deploy` accepts a small higher-level DSL and translates it to the verbose Docker Swarm Compose YAML, eliminating the boilerplate (Traefik labels, networks block, app/env/version labels, secret/network wiring, restart/update policies).

> Full field-by-field reference: [`docs/dsl.md`](docs/dsl.md).

### Manifest shape

```yaml
app: donation-campaign           # required — stack name
env: production                  # required — environment
domain: example.com              # required — root domain for Traefik routing
registry: ghcr.io/acme         # optional — image registry prefix
version: latest                  # optional — default image tag (overridable via --version)
repo_url: https://github.com/... # optional — metadata only (shown in stack list)
env_file: .env                   # optional — load env vars from a file

backup_before_deploy: true       # optional — snapshot volumes before deploy
strict_backup: true              # optional — abort the deploy if that backup fails

secrets:                         # optional — external Swarm secrets (must pre-exist)
  - donation_campaign_db_password

services:                        # required — one or more services
  db:
    image: postgres:14-alpine
    placement: node-01      # stateful → pin to ONE specific node (not manager/worker)
    volumes: [db_data:/var/lib/postgresql/data]
    env: { POSTGRES_DB: donation_campaign, POSTGRES_USER: user }
    secrets: [donation_campaign_db_password]
    healthcheck: { type: pg_isready }

  migration:
    image: ${registry}/${app}:${version}
    command: [./migrate]         # optional — override command (entrypoint also supported)
    run_once: true               # optional → one-shot job (restart: none)
    depends_on:                  # optional — wait for these services before starting
      - db                       #   pmcluster generates the wait (Swarm ignores depends_on)

  api:
    image: ${registry}/${app}:${version}
    replicas: 2                  # optional — default 1 (ignored when run_once)
    expose:                      # optional — auto-wires Traefik
      port: 8080                 #   container-side port
      host: api.${app}.${domain} #   primary FQDN
      aliases:                   #   extra hostnames → their own Traefik router
        - api.customer.com       #   (e.g. a customer's own domain)
      cors_disabled: false       #   opt out of the cluster-wide CORS middleware
    healthcheck: { type: http, path: /health }
    update:                      # optional — Swarm rolling-update tuning
      parallelism: 1             #   default 1
      delay: 10s                 #   default 10s
      order: start-first         #   start-first (default) | stop-first
```

**Service fields:** `image` (required), `replicas`, `run_once`, `mode` (`global`), `restart`, `restart_delay`, `placement`, `constraints`, `command`, `entrypoint`, `env`, `volumes`, `binds`, `ports`, `configs` (`config_path(<name>)` file mounts), `secrets`, `extra_hosts`, `resources`, `user`, `labels`, `logging`, `networks`, `expose` (with `external`/`mode`/`aliases`/`cors_disabled`), `healthcheck` (incl. `start_period`), `update`, `depends_on`, `skip_filelog` (exclude the service from the OTel log-tailing receiver when the app ships logs via OTLP itself). Top-level extras: `platform` (pmcluster-reserved), `networks`, `volumes`, `backup_before_deploy`, `strict_backup`.

**Stateful-aware defaults (no manifest change):** any service that mounts a **volume** is treated as stateful — it automatically gets `update: order: stop-first` (avoids the Postgres `postmaster.pid` shutdown race on redeploys) and, when `placement` is empty, is auto-pinned to a storage node: a per-stack `stack move` pin outranks the `storage_nodes` round-robin, which outranks the `platform_node` fallback (defaulting to the leader). The pin is visible as the service's `io.pmcluster.node` label.

**`depends_on` actually works on Swarm:** `docker stack deploy` parses `depends_on` but ignores it (no dependency graph). pmcluster enforces the ordering **in the control plane**: the deploy pipeline topologically sorts the services into `depends_on` levels and deploys level by level — each level's services get their own compose (rendered from the same IR), deployed via `docker stack deploy`, and waited for (long-running services until replicas ≥ desired; `run_once` jobs until their task completes) before the next level starts. One drift-prune pass runs at the end with the full-stack compose. Because the ordering lives in the deploy step, it works for **any** image — no shell wrapper is injected, so baked-entrypoint images (Postgres, MySQL, …), distroless images without `sh`, and images with no declared `command:` all behave identically. `run_once` services with `depends_on` also get `restart_policy: on-failure` (max 3 retries). A cycle or a reference to a service that does not exist fails the deploy loudly before anything is created.

**Healthchecks:** `{ type: pg_isready }` (uses `$POSTGRES_USER`/`$POSTGRES_DB`) or `{ type: http, path: /health }` (defaults to `/`), or the full form (`test`, `interval`, `timeout`, `retries`).

Substitution: `${app}`, `${env}`, `${version}`, `${registry}`, `${domain}`, plus `${env:VAR}` for OS env. Strict YAML — unknown keys are rejected.

### DB-backed secrets & configs

Beyond plain env values, `env` can reference entries from pmcluster's DB
stores — managed with `pmcluster secret` / `pmcluster config` or the operator
console:

```yaml
env:
  ADMIN_PASS: secrets(app_secret)     # stored secret → mount path /run/secrets/app_secret
                                      # (must also be listed in the service secrets: array)
  ADMIN_ENABLED: config(app_config)   # stored config content
  STORAGE_ROOT: settings(volume_root) # a live cluster setting's value
  DB_URL: secret(db_url)              # a stored secret's VALUE, printed as-is
```

Four reference kinds exist (all whole-value env entries): `secrets(<name>)` resolves to the **mount path** `/run/secrets/<name>` (the secret must also be mounted via the `secrets:` array); `config(<name>)` injects a stored config's content; `settings(<name>)` injects a cluster setting; `secret(<name>)` injects the secret's **value itself** (for consumers that cannot read a mounted file — e.g. OpenObserve's root password env). Services can also mount configs as **files** with `configs: [- config_path(<name>)]` (default path `/etc/<name>`). See [`docs/dsl.md`](docs/dsl.md).

- **Secrets** are stored **AES-256-GCM encrypted** in `data.db` (key:
  `~/.pmcluster/.encryption_key`). The plaintext is shown **once** at creation;
  afterwards only the **sha256 hash** is displayed (`pmcluster secret
  list/show/verify`), and the UI lists every secret with its hash — never its
  value. An authenticated operator can still **decrypt a value on demand**
  (`GET /api/secrets/{name}/value`, or the *Reveal* button in the console,
  which asks for confirmation first) when they need to rotate a value
  elsewhere. Values can also be **edited** in place (`PUT /api/secrets/{name}`)
  without recreating the secret.
- **Configs** are stored in plain text with a full **version history**
  (`pmcluster config history` / `rollback`). Editing a config records the
  previous value; rolling back restores it. UI shows content, history, and a
  rollback button.
- Both come in two **scopes**:
  - **`cluster`** — the platform's own templates and secrets. These live in the
    **Settings** page in the console. Editing a cluster config re-stamps it
    with the current binary version, so the platform stacks render from it on
    the next update; the **Apply to swarm** button (`POST /api/update`) runs
    `cluster update` immediately so the change reaches the swarm side.
  - Rendered platform configs (the post-substitution YAML actually sent to the
    Swarm) are snapshotted into the database by every cluster update and shown
    read-only on the **Settings** page under "Rendered cluster configs"
    (`GET /api/cluster/rendered` — **operator/admin role only**: the rendered
    YAML embeds the OpenObserve root credentials).
  - **`service`** — values belonging to one stack, reached from the stacks
    list via the **Config** button (page `/stacks/<name>/config`). Create
    forms prefill the `<stack>_` name prefix and record the owning stack.
- CLI cheat-sheet (both `create` commands accept `--stack <name>` for
  service-scope values):
  - `pmcluster secret create <name> [value] [--scope cluster|service] [--stack <name>]` / `list` / `show <name>` / `verify <name> <value>` / `edit <name>` (re-mirrors to a content-addressed swarm secret) / `delete <name>` / `heal` (verify + repair every secret)
  - `pmcluster config create|list|get|edit|history|rollback|delete <name> [--scope ...] [--stack <name>]`
- **Swarm-first, content-addressed:** every DB config/secret row is mirrored
  into the swarm as a content-addressed object (`<name>_<sha256-first-8-hex>`)
  — the same value always maps to the same object, a rotation mints a new one
  and the old, in-use secret stays mounted (Docker secrets are immutable). The
  DB is the index **and** holds the value: translate reads values from the
  swarm and rebuilds a missing object from the DB, and every `cluster update`
  runs a rebuild-on-missing repair pass.
- REST API: `GET/POST /api/secrets` (`?scope=`/`?stack=` filters),
  `PUT/DELETE /api/secrets/{name}`, `GET /api/secrets/{name}/value`,
  `GET/POST /api/configs` (`?scope=`/`?stack=` filters),
  `GET/PUT/DELETE /api/configs/{name}`, `GET /api/configs/{name}/versions`,
  `POST /api/configs/{name}/rollback`, and `POST /api/update` (apply platform
  config to the swarm) — see `docs/openapi.yaml`.

### Deploy

Locally, on the manager:

```bash
pmcluster deploy ./donation-campaign.yaml [--version <tag>] [--app <name>] [--repo <url>] [--file <path>]
```

Remotely, via the HTTP API (note: returns `202 Accepted` — the swarm apply runs in the background):

```bash
curl -X POST https://pmcluster.example.com/api/stacks \
     -H "Authorization: Bearer <admin-token>" \
     -H "Content-Type: application/json" \
     -d "{\"app_name\":\"donation-campaign\",\"manifest\": $(jq -Rs . < donation-campaign.yaml)}"
```

### Inspect, list, roll back

```bash
pmcluster stack list                      # name, current revision, last update
pmcluster stack show donation-campaign    # metadata + recent revisions (→ marks current)
pmcluster rollback donation-campaign 1778439014   # re-apply a stored revision
```

Every deploy gets a unix-timestamp revision id. Rollback re-applies a stored revision as a NEW revision (preserves the audit trail — both deploys are recorded; it re-translates the *source* manifest with the current translator, never re-deploys stale rendered YAML). The corresponding REST endpoints are `GET /api/stacks`, `GET /api/stacks/{name}`, `GET /api/stacks/{name}/revisions/{rev}`, `POST /api/stacks/{name}/rollback`, `POST /api/stacks/{name}/sync`, `POST /api/stacks/{name}/move`, `POST /api/stacks/{name}/ack`. A stack can be removed (services `docker stack rm`, named volumes, mounted Swarm secrets, plus the DB record, its revisions and its service-scope configs/secrets) from the console's stacks list — `DELETE /api/stacks/{name}`.

### Remote CLI (off-node)

The CLI is a thin consumer of the same service ports as the daemon API, so it
can run **on any machine** against a reachable daemon — no need for the node's
SQLite or Docker socket:

```bash
export PMCLUSTER_API_URL=https://pmcluster.example.com   # or --api-url
export PMCLUSTER_API_TOKEN=pmc_...                       # or --api-token

pmcluster --api-url "$PMCLUSTER_API_URL" --api-token "$PMCLUSTER_API_TOKEN" \
  stack list
pmcluster ... deploy ./donation-campaign.yaml
pmcluster ... secret list
pmcluster ... config list
pmcluster ... tls site show
pmcluster ... backup create
pmcluster ... user create ci-bot
```

Tokens come from `pmcluster user create <name>` (or the console); every
data command (`config`, `secret`, `webhook`, `user`, `backup`, `deploy`,
`stack`, `rollback`, `tls`) switches between the local core and the REST API
through one backend factory in the binary. Bootstrap/repair commands
(`init`, `cluster *`, `credentials *`, `node`, `registry`, `logs`, `serve`)
stay local — they need the manager's filesystem and Docker socket by design.

### Webhooks (CI integrations)

```bash
pmcluster webhook add github-prod      # prints a 64-char hex secret ONCE; save it
```

CI side — sign the request with HMAC-SHA256 keyed by that secret string and POST to `/webhook/<source>`. Two headers are required: a current Unix-seconds timestamp and the signature over `timestamp + body` (no separator). Timestamps outside a 5-minute window are rejected, which blocks replay attacks. The body must carry **provenance** (`repo_url` + `file`) — a webhook deploy without both is rejected. A successful deploy answers **`202 Accepted`** (the swarm apply runs in the background); a failing deploy is retried (2 extra attempts, 30s apart) before a `502` with the retry count.

```bash
TIMESTAMP=$(date +%s)
SIG=$(printf '%s' "${TIMESTAMP}${BODY}" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print "sha256=" $2}')
curl -X POST https://pmcluster.example.com/webhook/github-prod \
     -H "X-Pmcluster-Timestamp: $TIMESTAMP" \
     -H "X-Pmcluster-Signature: $SIG" \
     -H "Content-Type: application/json" \
     -d "$BODY"
```

The webhook returns the same generic 401 for "wrong secret", "wrong source", "bad/missing timestamp", and "missing signature" — no information leak about which case tripped. See [`docs/webhook.md`](docs/webhook.md) for the full integration guide and a GitHub Actions example.

### Private registries

```bash
# Interactive (prompts for password):
pmcluster registry add ghcr.io

# Scripted (no shell history leak):
echo "$GITHUB_PAT" | pmcluster registry add ghcr.io --username myuser --password-stdin

# At install time (one-shot):
curl -fsSL .../install.sh | PMCLUSTER_REGISTRY="ghcr.io=myuser=$GITHUB_PAT" bash

pmcluster registry list
```

`pmcluster serve` re-runs `docker login` for every persisted registry on startup, so a manager rebuild that wiped `~/.docker/config.json` keeps pulling private images via `--with-registry-auth`.

### Pre-deploy & on-demand backups

Add `backup_before_deploy: true` to a manifest's top level and pmcluster will trigger an offen volume snapshot before `docker stack deploy` (a WAL checkpoint flushes SQLite first, and the trigger retries once if the agent is mid-restart). The deploy proceeds even if the backup fails (a flaky backup container shouldn't block urgent rollouts) — **unless** `strict_backup: true` is also set, which aborts the deploy. Failures show up loud in `pmcluster backup list`.

```bash
pmcluster backup create                       # on-demand snapshot (run on a storage node)
pmcluster backup list                         # every run + discovered archives (store + local)
pmcluster backup browse <id>                  # archive contents (TYPE/SIZE/PATH)
```

The scheduled agents run **hourly by default** (`backup_cron`) on every storage node and write each archive to the in-cluster SeaweedFS store (WebDAV) and, when `backup_s3_*` is configured, to the offsite S3/R2 target — see the [backup stack](#stack-components) section above. A second, manager-pinned **control-plane agent** archives `~/.pmcluster` itself (SQLite DB, encryption key, config, rendered configs) daily with a `pmcluster-ctlplane-` prefix and 30-day retention, uploaded offsite too — a lost manager disk no longer means a full re-bootstrap.

Restore is **implemented**: `pmcluster backup restore <id>` extracts a SUCCEEDED stack-scoped run under the volume root with the archive's `/backup/data` prefix stripped; whole-disk runs restore to the volume root and cover every stack; control-plane archives are refused into the volume root. Three knobs refine it:

- `--volume <name>` restores a **single volume** only — matched as a full path segment inside the archive, so a per-volume restore never leaks entries from other volumes or stacks (a stack-scoped run's `--volume` is anchored to that run's stack; whole-disk runs accept `<app>/<volume>` to scope to one app).
- Restore sources are resolved in order: a local copy of the archive (when present on this node's archive dir) → the configured object store. The daemon prefers the **in-cluster SeaweedFS store** for the fetch (its objects are indexed into `backup list` no matter which node uploaded them); the CLI's direct local path falls back to the **offsite** `backup_s3_*` bucket. `--from-s3` always forces the **offsite** bucket, even when a store/local copy exists. With no archive anywhere, the command fails loudly and says where the archive lives.
- **Routed restores:** when `--volume` names a stack whose volume lives on another node, the restore is routed there via the store-transit mover (a one-shot swarm service on the target pulls the archive object from the store and unpacks it into the owning node's volume root) — never silently into the wrong node.

See [`pmcluster/docs/restore-design.md`](pmcluster/docs/restore-design.md) and [`docs/storage-and-databases.md`](docs/storage-and-databases.md) for the shipped design and the full story.

## Storage & Databases

pmcluster standardizes on a **single host root mount** — the `volume_root` setting, default `/var/stack/data` — for all application data. How you configure, format, or replicate the underlying host storage is entirely up to you. Databases run **inside the cluster** (no managed DB services); replication, when you need it, is handled at the database level (CockroachDB / MariaDB Galera / Patroni / rqlite) by the workload itself — **OS-level block replication (DRBD/LINSTOR) is deliberately off the platform's roadmap** (the "Path 1" decision: pinned placement + hourly backups + `stack move` / storage failover deliver the recovery outcomes at a fraction of the footprint). Every byte is backed up hourly to the in-cluster store and (when configured) offsite to S3-compatible storage (R2 / S3 / IONOS / Hetzner Storage Box).

**The zero-effort rule for stateful services:** if you have a stateful service (anything with a volume) and you do not want to deal with storage replication or mirroring at all, pin it to one specific node with `placement: <hostname>` — not `manager`/`worker`. It then runs on that node forever and its data never has to migrate; nothing else needs configuring. `manager`/`worker` allow any node of that role, which is unsafe for volumes once you have more than one node of that role. Leaving `placement` empty is also safe: volume services are auto-pinned to a **storage node** (per-stack `stack move` pin → `storage_nodes` round-robin → `platform_node` fallback).

```yaml
services:
  db:                       # stateful — SQLite, Postgres, anything with a volume
    image: ghcr.io/you/db
    placement: node-01 # a SPECIFIC node hostname, not "manager"/"worker"
    volumes:
      - db_data:/var/lib/db
```

The full decision flowchart, storage matrix, database strategies, the backup architecture (offen agents + the SeaweedFS store + offsite double-write), restore/move/failover, and a quickstart example stack live in [`docs/storage-and-databases.md`](docs/storage-and-databases.md).

---

## Per-Host TLS (customer domains)

An app can be reachable on a pmcluster subdomain *and* on a customer's own domain, each served over HTTPS with its own real certificate. Add the extra hostname as an `expose.aliases` entry in the manifest (that creates the Traefik router), then supply the matching certificate:

```bash
# Store a cert+key as text (paste inline) or from files:
pmcluster tls hosts add api.customer.com --cert-file cert.pem --key-file key.pem
pmcluster tls hosts add api.customer.com --cert "$(cat cert.pem)" --key "$(cat key.pem)"

pmcluster tls hosts list                    # host, expiry, SANs
pmcluster tls hosts remove api.customer.com
```

Per-host certs use the same table and flow as the cluster's own certificate: the PEM bytes become **content-addressed** Swarm secrets (`hostcert-<host>_<sha8>` / `hostkey-<host>_<sha8>` — identical content reuses the object, a rotation mints a new one) and the metadata (expiry, SANs, hashes) is recorded in the `site_certs` DB table — they are **never** written to the manager's filesystem. Adding or removing one re-renders the Traefik dynamic config and re-deploys the infra stack so it takes effect immediately; pass `--no-refresh` to defer the refresh to the next `pmcluster cluster update`. The cluster's own wildcard certificate (`cluster up --cert/--key`) is never touched by these commands.

---

## REST API & API Keys

The daemon exposes a JSON REST API under `/api/*` (Bearer auth) plus the unauthenticated `GET /health` liveness probe. The full spec lives at [`pmcluster/docs/openapi.yaml`](pmcluster/docs/openapi.yaml). Highlights:

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/me` | Current user |
| `GET` | `/api/cluster/info` | Cluster + Swarm status |
| `GET` | `/api/nodes` | Swarm nodes (incl. each node's storage flag) |
| `POST`/`DELETE` | `/api/nodes/{hostname}/storage` | Promote / demote a storage node |
| `GET` | `/api/usage` | Config/secret usage graph (which stacks reference each) |
| `GET` | `/api/cluster/settings` | List all editable cluster settings (29 allowlisted keys; secret-ish values masked for non-admin bearers) |
| `PUT` | `/api/cluster/settings` | Atomic update of cluster settings (does NOT redeploy) |
| `GET` | `/api/cluster/rendered` | Stored rendered platform configs (operator/admin role only) |
| `GET`/`POST` | `/api/stacks` | List / deploy a stack (deploy → `202`, applying in the background) |
| `GET` | `/api/stacks/{name}` | Stack detail + revisions |
| `GET` | `/api/stacks/{name}/revisions/{rev}` | A stored revision |
| `POST` | `/api/stacks/{name}/rollback` | Roll back to a revision |
| `POST` | `/api/stacks/{name}/sync` | Re-apply the stored manifest (k8s-style reconcile) |
| `POST` | `/api/stacks/{name}/move` | Move a stateful stack's storage to another node |
| `POST` | `/api/stacks/{name}/ack` | Acknowledge a storage failover |
| `DELETE` | `/api/stacks/{name}` | Remove a stack (services, volumes, secrets, record) |
| `GET`/`POST` | `/api/backups` | List / trigger backups |
| `GET` | `/api/backups/{id}/files` | List files inside a backup run's archive |
| `POST` | `/api/backups/{id}/restore` | Restore a backup to a destination root |
| `GET` | `/api/services` | Swarm services (each carries a `platform` flag) |
| `GET`/`POST` | `/api/services/{stack}/{service}/tasks|logs|restart|exec` | Whitelisted service ops (exec/ws for the interactive terminal) |
| `GET`+`HEAD` | `/api/public/badge/{stack}[/services|/{service}]` | Public no-auth status SVG badges (reads the DB snapshot) |
| `POST` | `/webhook/{source}` | HMAC-verified CI deploy (outside `/api`) |
| `GET`/`PUT`/`DELETE` | `/api/tls/hosts` | Per-host TLS certs |
| `GET`/`PUT` | `/api/tls/site` | Cluster's own (main) certificate |
| `GET`/`POST` | `/api/secrets` | DB-backed secrets (list/create) |
| `GET`/`PUT`/`DELETE` | `/api/secrets/{name}` | Secret metadata / in-place edit / delete (+ `/value` reveal) |
| `GET`/`POST` | `/api/configs` | DB-backed configs (list/create) |
| `GET`/`PUT`/`DELETE` | `/api/configs/{name}` | Config content + versions/rollback |
| `GET`/`POST`/`DELETE` | `/api/webhooks` | Webhook sources |
| `GET` | `/api/webhooks/{source}/deliveries` | Webhook delivery history (newest first, incl. retries) |
| `GET`/`POST` | `/api/api_keys` | API keys |

API tokens are created with `pmcluster user create <name>` or the console. They use the format `pmc_<token_id>_<secret>` (a public 8-hex-char lookup id plus a base64url secret) so the daemon can find the row without scanning every user; the plaintext token is shown once and only a hash is stored.

```bash
curl -H "Authorization: Bearer pmc_a1b2c3d4_..." \
  https://pmcluster.example.com/api/cluster/info
```

The edge proxy applies per-IP rate limits: `200 req/s` (burst 400) for `/api/*` and `40 req/s` (burst 60) for `/webhook/*`; exceeded requests get `429` with `Retry-After: 1`. Oversized bodies are rejected with `413` before reaching the daemon.

---

## Cluster settings catalogue

Editable settings are a fixed **29-key allowlist** (`cluster settings list|get|set`, the console's Settings page, `GET/PUT /api/cluster/settings`). Values persist in the store; the swarm side picks them up on the next `cluster update` (or the console's **Apply to swarm**). Unknown keys are rejected atomically.

| Key | Default | Who sets it | What it controls |
|---|---|---|---|
| `volume_root` | `/var/stack/data` | setup wizard (`--volume-root`) / operator | the host root every container volume is forced under |
| `platform_node` | the **leader** (auto-written by `cluster up`/`update` when unset) | operator-optional | pins the platform services (OpenObserve, edge, backup agents, SSO) to one node; empty = `node.role == manager` |
| `storage_nodes` | leader's hostname (auto-seeded) | operator, `join --storage-node`, `node promote/demote` | comma-separated storage node hostnames; stateful stacks round-robin across them; the current leader is always re-added, a former leader is removed, and labeled-but-unlisted nodes are adopted |
| `storage_failover` | `false` | setup prompt (only when offsite S3 is configured) / operator | automatic storage failover in the control loop (requires `backup_s3_*`; refuses to enable without it) |
| `backup_cron` | `0 * * * *` (hourly) | operator | the offen agents' schedule (5-field cron) |
| `backup_retention_days` | `15` | operator | prune window for backup rows + archives (local and remote) |
| `backup_all_nodes` | `false` | operator | run the volume agent on **every** node instead of storage nodes only |
| `backup_store_on` | `""` (= worker/auto) | setup wizard / operator | where the in-cluster SeaweedFS store runs: `leader`, or a non-storage non-platform worker (auto) |
| `backup_s3_endpoint` / `_bucket` / `_access_key` / `_secret_key` / `_region` | unset (`_region` defaults `auto`) | setup wizard / operator | the offsite S3/R2 destination — when set, agents double-write every archive offsite |
| `reconcile_interval` | `60` (seconds) | operator | the control loop's safety tick; `0` disables the loop |
| `edge_login_disabled` | `true` in swarm deployments | setup wizard / edge stack | hides the console's own login (the Traefik admin-auth/SSO gate is the authenticator); `false` keeps password login (standalone) |
| `sso_enabled` / `sso_provider` / `sso_client_id` / `sso_client_secret` / `sso_github_org` / `sso_github_repos` / `sso_cookie_expire` | off / `github` / unset / unset / unset / unset / `1h` | setup wizard (`--sso-*`) / operator | GitHub SSO via oauth2-proxy: org (and optional repo) restriction, tightened 1h default session |
| `domain` | — | `cluster up` / setup wizard | the cluster's base domain |
| `oo_admin_email` | — | `cluster up` (`--openobserve-email`) | the OpenObserve root admin login |
| `traefik_admin_user` | `admin` | setup wizard / `cluster up` | the basic-auth username for the Traefik gate |
| `openobserve_logs_retention_days` / `_metrics_` / `_traces_` | `7` each | operator | OpenObserve stream retention (unbounded retention is how a cluster silently grows to hundreds of GB) |
| `log_level` | `info` | operator | daemon verbosity (`debug`/`info`/`warn`/`error`); applied live on save |

**Not in the allowlist** (managed elsewhere): `tls_mode` / `tls_cert_path` / `tls_key_path` / `tls_acme_email` (the setup wizard / `cluster up --acme-email|--cert|--key`; switching modes needs `--force-tls-mode`); `storage_leader` (written by `cluster update` to remember which hostname was promoted because it held leadership); `stack_pin_<stack>` (written only by `stack move`, outranks the round-robin); `swarm_id` (wipe detection, written by `cluster update`).

---

## Minimum Hardware

| Node | CPU | RAM | Notes |
|------|-----|-----|-------|
| Manager (leader) | 2 vCPU | 4 GB | Traefik + OpenObserve + OTel Collector + backup agent + pmcluster-edge + pmcluster |
| Additional manager | 1 vCPU | 1 GB | Traefik (global on managers) + OTel Collector + standby daemon |
| Worker | 1 vCPU | 1 GB | OTel Collector + (backup agent + your stateful workloads when it is a storage node) |

OpenObserve alone needs ~512 MB RAM at idle (it and the edge are pinned to the leader — `platform_node` defaults there so a tiny node never hosts the platform stack). On a manager with less than 4 GB it will compete with Traefik under load. `pmcluster-edge` is tiny (64–256 MB, capped).

---

## TLS Certificates & Renewal

**Let's Encrypt mode (`--acme-email`):** Traefik handles issuance and renewal automatically (HTTP-01 via the `:80` entrypoint). Certs are stored in the `traefik_acme` Docker volume on the managers. Nothing to rotate manually. Note: each Traefik instance runs its own ACME challenge listener — behind a load balancer that may route the challenge to a *different* manager, so for LB-fronted multi-manager topologies use an operator-supplied certificate instead (see [network-topology.md](docs/network-topology.md)).

**Operator-supplied mode (`--cert`/`--key`):** the PEM pair is stored in the DB and materialized as **content-addressed** Swarm secrets (`cert_<sha8>` / `key_<sha8>`, mounted at their versioned paths). To rotate, use the supported in-place flow (no `docker secret rm` needed — secrets are immutable and rotation mints a new object):

```bash
pmcluster tls site set --cert-file new.pem --key-file new.key.pem
```

Switching an existing cluster between ACME and operator-cert mode requires `--force-tls-mode`. After renewing a certificate on disk, `pmcluster tls site set` (or `cluster update`) re-applies the stored cert/key without a full bring-up. For per-host (customer-domain) certificates, use `pmcluster tls hosts …` — see [Per-Host TLS](#per-host-tls-customer-domains).

### Renewing the main certificate in place

The cluster's own (main) certificate can be rotated without a full bring-up, from any of the three entry points — the result is identical: the pair is validated against the cluster domain, stored in the DB, materialized as content-addressed Swarm secrets (`cert_<sha8>`/`key_<sha8>`), wired into the Traefik dynamic config, and its metadata (validity window, SANs, hashes) recorded in the DB for expiry monitoring.

```bash
# CLI — status + upload
pmcluster tls site show                                    # domain, expiry, SANs, secret names, hashes
pmcluster tls site set --cert-file new.pem --key-file new.key.pem

# REST API
curl -X PUT https://pmcluster.<domain>/api/tls/site \
  -H "Authorization: Bearer $PMC_TOKEN" -H 'Content-Type: application/json' \
  -d "$(jq -n --rawfile c new.pem --rawfile k new.key.pem '{cert:$c,key:$k}')"
```

The operator console exposes the same flow on the **TLS** page (a *Main certificate* card above the per-host list), including the current expiry. Certificates within **30 days** of expiry are flagged in the console and warned about on every `cluster up`/`cluster update` — operator certs are not auto-renewed, so this is the reminder to rotate. (ACME-mode clusters renew automatically and have no such row.)

---

## Alerting

OpenObserve has a built-in alerting engine. Once your stack is running, set up alerts via the OpenObserve UI under **Alerts → Alert Rules**. Recommended starting points:

- **Service down** — alert when a service stops sending logs/metrics for >5 min
- **High error rate** — alert when `severity_text = ERROR` log count spikes
- **High memory usage** — alert on `container.memory.usage` from the OTel Collector

Notifications: email, Slack webhook, any HTTP endpoint.

---

## License

MIT. Copyright © 2025 Hazem Arian.

---

## RFC & Discussion

The current design — what actually shipped — lives in **[issue #1: RFC v2](https://github.com/hazemarian/poor-man-cluster/issues/1)**. It covers the architecture, the trade-offs vs. the original v1 pitch, and what's intentionally out of scope.

---

## Contact

Built and maintained by **Hazem Arian**.

- Email: [hazeem.arian@gmail.com](mailto:hazeem.arian@gmail.com)
- LinkedIn: [linkedin.com/in/hazem-a-467b4183](https://www.linkedin.com/in/hazem-a-467b4183/)

Contributions, issues, and feedback welcome.

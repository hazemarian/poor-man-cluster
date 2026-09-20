# Poor Man's Cluster

A simple, cost-effective, production-ready deployment stack on open-source tools. No Kubernetes, no managed cloud services, no expensive licensing — just Docker Swarm, a small Go control plane (`pmcluster`), and a handful of well-chosen tools that get the job done.

This project gives you a full production cluster with HTTPS ingress, observability, programmatic deployments, automated backups, and a single CLI to manage it.

The control plane is a single static Go 1.25 binary (`pmcluster`, ~25 MB, no cgo) that brings the cluster up, deploys applications via a small DSL with versioned rollbacks, accepts HMAC-verified webhooks from CI, manages registry credentials and bootstrap passwords, and ships its own JSON audit logs.

In front of it sits **`pmcluster-edge`** — a small Go service deployed as a Swarm service that publishes `pmcluster.<domain>` as the single public origin for the **operator console** (a web UI for API keys, TLS, webhooks, stacks, and more), the REST API, and webhook receivers — all shielded by per-IP rate limiting, a connection shield, and automatic IP blocklisting.

- **Design + trade-offs:** [RFC v2 — issue #1](https://github.com/hazemarian/poor-man-cluster/issues/1) (what actually shipped)
- **Current release:** [v0.2.71](https://github.com/hazemarian/poor-man-cluster/releases)

---

## How It Works

The stack runs on Docker Swarm. One machine acts as the **manager node** — it controls the cluster, hosts the management UIs, and runs `pmcluster`. Any number of **worker nodes** can join with `docker swarm join`; the manager automatically schedules global services on them.

Two overlay networks connect everything:

- **`traefik-net`** — application traffic between Traefik and your services
- **`monitoring-net`** — telemetry (logs, metrics, traces) between services and OpenObserve

All sensitive credentials are stored as Docker Swarm secrets (encrypted at rest and in transit) AND mirrored encrypted in `pmcluster`'s SQLite for retrieval. Platform configs (Traefik dynamic, OTel collector, observability stack, etc.) are stored **only in the DB** — no `~/.pmcluster/config/*.yml` disk files. On each `cluster update`, configs are rendered from embedded templates, hashed, and compared against the stored `rendered_hash`; only changed stacks are re-deployed. Drift-prune removes services that were dropped from a compose. The bootstrap admin passwords for Traefik/OpenObserve/edge are generated randomly on first `cluster up` — no `.env` editing required.

```
Internet
   │
   ▼
Traefik (HTTPS ingress, auto-routes via Docker labels + file provider)
   ├──▶ pmcluster.<domain>
   │        ├── /web/*  ──▶ pmcluster-edge ──▶ operator console (gin + HTMX)
   │        │             gated by admin-auth (htpasswd) or sso-auth (forwardAuth)
   │        └── /api/*  ──▶ pmcluster-edge ──▶ pmcluster daemon (127.0.0.1:9090)
   ├──▶ observ.<domain> ──▶ OpenObserve
   │        gated by admin-auth + openobserve-auto-auth (no login prompt)
   ├──▶ traefik.<domain>/dashboard/ ──▶ Traefik dashboard
   │        gated by admin-auth or sso-auth
   ├──▶ sso.<domain> ──▶ oauth2-proxy (SSO, optional)
   └──▶ Your App(s)

Your App(s) ──OTLP──▶ OTel Collector ──▶ OpenObserve
Traefik     ──OTLP──▶ OTel Collector ──▶ OpenObserve
pmcluster-edge shields: per-real-IP rate limits, in-flight shield, timeouts,
                       body caps, auto-ban of abusive IPs (403)

Every node: OTel Collector + Backup Agent (global services)
```

---

## Stack Components

### Traefik — Ingress & API Gateway
Sits at the edge and routes HTTPS traffic to the right service based on Docker labels (for swarm services) and a file provider (for the pmcluster route + TLS certificates). TLS certs are loaded from Swarm secrets. Built-in OpenTelemetry support sends traces and metrics to the collector.

### pmcluster operator console — Service & Platform UI
Web UI (served by `pmcluster-edge` on the `pmcluster.<domain>` origin) for day-to-day operations: view stacks and **services** (live replica health, task/crash history, tailed logs), **restart** services, run one-off `exec` commands, manage webhooks/API keys/TLS/backups, manage **users** with **RBAC roles** (admin > operator > viewer), and edit cluster settings. The console lives under `/web` and is gated by Traefik's `admin-auth` (htpasswd) middleware — or by `sso-auth` (oauth2-proxy forwardAuth) when GitHub SSO is enabled. When behind the auth gate, the console's own login page is disabled (`EDGE_LOGIN_DISABLED=true`) and the Users CRUD is hidden from the nav. No Portainer — its role is fully covered here plus the `pmcluster service` CLI.

### oauth2-proxy (SSO, optional)
When GitHub SSO is enabled via `pmcluster setup --sso-enabled`, an `oauth2-proxy` sidecar runs in its own stack (`sso.<domain>`) on the manager. It performs GitHub OAuth (with optional org restriction) and exposes `/oauth2/*` endpoints. Traefik's `sso-auth` forwardAuth middleware replaces `admin-auth` on the console (`/web/*`), OpenObserve (`observ.<domain>`), and the Traefik dashboard (`traefik.<domain>`). The `/oauth2/*` path resolves on every gated host so the OAuth redirect loop works correctly. The cookie is shared across all `<domain>` subdomains (`OAUTH2_PROXY_COOKIE_DOMAINS`). When SSO is on, the edge console's own login is disabled — you authenticate through GitHub.

### OpenObserve — Observability (Logs, Metrics, Traces)
Lightweight all-in-one observability platform. Single binary, one UI, ~140× lower storage cost than Elasticsearch-based stacks. Receives OTLP from the OTel Collector.

No provisioning API or automation user — the OTel collector and the Traefik auto-auth middleware both use the **root admin credentials** (the `openobserve_admin` managed credential / `zo_root_user_password` Swarm secret). OpenObserve's own login page is never shown: it is gated by `admin-auth` (or `sso-auth`) plus a static `openobserve-auto-auth` middleware that injects a Basic `Authorization` header on every request.

### OpenTelemetry Collector — Telemetry Aggregation
Runs as a global service on every node. Auto-discovers containers via the Docker observer, tails their logs, enriches with Swarm metadata, collects Docker resource metrics, forwards everything to OpenObserve. The pipeline config is generated by pmcluster and shipped as a Docker config (replicated to every node by Swarm itself).

### offen/docker-volume-backup — Automated Backups
Runs as a global service on every node. Nightly tarballs of all Docker volumes, named with the node ID, 7-day retention, optional S3 upload.

### pmcluster-edge — Smart Proxy & Operator Console
A small Go service (gin + HTMX) deployed as the `pmcluster-edge` Swarm service on the manager. It owns the `pmcluster.<domain>` origin end to end:

- **Operator console** — a web UI (user `admin`, password provisioned at `cluster up`) for managing API keys, users, webhooks, per-host TLS certificates, stacks (deploy/rollback/delete), backups, and cluster settings.
- **Smart reverse proxy** — everything the console doesn't handle is proxied to the pmcluster daemon (`host.docker.internal:9090`). The daemon's host-bound port is no longer exposed publicly.
- **Edge hardening** — per-real-IP token-bucket rate limits (separate buckets for `/api/*` and `/webhook/*`), an in-flight request shield (503 when saturated), request timeouts, body size caps, and automatic IP blocklisting (403) after repeated abuse or upstream failures.

Its image is built from `cmd/edge/` and published to GHCR; the embedded `edge-stack.yml` pins the version-keyed tag so `cluster up`/`cluster update` can detect and roll out upgrades.

### pmcluster — Control Plane (Go)
Single static binary, lives on the manager host. Replaces the bash setup script and owns deployment end to end.

- `pmcluster init` — creates `~/.pmcluster/` (SQLite + encryption key) and prints a one-time bootstrap admin token for the API
- `pmcluster setup` — **interactive wizard**: collects domain, TLS (LE or BYO), Traefik admin user, SSO enable (GitHub creds + org), edge login preference, then runs `cluster up` (fresh) or `cluster update` (existing). All questions have matching flags for scripted use.
- `pmcluster cluster up` — **init-only**: brings a fresh cluster up (prevents re-running if a cluster already exists — run `cluster update` instead). Configs are stored in the DB only (no `~/.pmcluster/config/*.yml` disk files); rendered at deploy time from embedded templates.
- `pmcluster cluster update` — **content-aware reconcile**: DB is the source of truth. Compares rendered compose hashes (`rendered_hash`) against stored values; re-deploys only changed stacks. Drift-prune removes services dropped from a compose.
- `pmcluster cluster settings` — list all cluster settings (KEY/VALUE; secret-typed keys masked); `pmcluster cluster settings get <key>`; `pmcluster cluster settings set key=value ...`
- `pmcluster cluster status` / `cluster down`
- `pmcluster serve` — runs the long-running daemon (REST API + webhook receiver). Listens on `127.0.0.1:9090`; Traefik routes `pmcluster.<domain>` to it via `host.docker.internal:host-gateway`
- `pmcluster deploy <file>` / `pmcluster stack list|show` / `pmcluster rollback <stack> <rev>` — DSL-based application deploys with versioned rollback
- `pmcluster credentials list|show|rotate` — managed bootstrap passwords (Traefik / OpenObserve / edge)
- `pmcluster service list|ps|tasks|logs|restart|exec` — whitelisted service operations (replica health, crash history, log tailing, restart, non-interactive exec) working locally or over the remote API
- `pmcluster registry add|list|remove` — Docker registry credentials, replayed on `serve` startup so private images keep pulling
- `pmcluster webhook add|list|remove|deliveries` — HMAC-signed webhook sources for CI integrations (timestamped to prevent replay); `deliveries <source>` shows newest-first delivery history (ID/STATUS/STACK/REVISION/REPO/FILE/WHEN/ERROR)
- `pmcluster tls hosts add|list|remove` — per-host TLS certificates for customer domains served by Traefik (independent of the cluster wildcard cert)
- `pmcluster tls site show|set` — inspect or rotate the cluster's own (main) certificate in place, with expiry metadata
- `pmcluster backup create|list|browse|restore` — on-demand offen volume snapshots; deploys can opt-in via `backup_before_deploy: true`; `browse <id>` lists files inside a backup archive (TYPE/SIZE/PATH); `restore <id>` extracts a SUCCEEDED, stack-scoped backup back under `dest_root/<stack>`
- `pmcluster node list|join-token` — wraps `docker node` for the read paths
- `pmcluster secret create|list|show|verify|delete` — DB-backed secrets (AES-256-GCM encrypted, shown as hashes)
- `pmcluster config create|list|get|edit|history|rollback` — DB-backed configs with version history
- `pmcluster user create|list|edit|remove` — issue and manage API tokens for additional users (RBAC roles: admin/operator/viewer; tokens print once, hashed at rest)
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
poor-man-stack/
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
│   │   ├── workflow/                   # named-step runner (up 10, update 8, deploy 5, …)
│   │   ├── edgeproxy/                  # rate limit, shield, blocklist, real IP, proxy
│   │   ├── ui/                         # operator console (gin + HTMX, controllers + templates)
│   │   ├── docker/                     # SDK wrapper behind a small interface
│   │   ├── cluster/embeds/             # bundled compose YAMLs (//go:embed)
│   │   ├── config/, store/, auth/      # config, SQLite, bearer-token auth
│   │   ├── credentials/                # AES-GCM encryption for stored creds
│   │   └── buildinfo/                  # version/commit/date with VCS fallback
│   ├── ARCHITECTURE.md                 # dependency rule + domain inventory
│   ├── migrations/                     # *.sql (0001..0010), embedded via //go:embed
│   └── e2e/                            # smoke end-to-end tests
├── docs/
│   ├── dsl.md                          # Deploy DSL reference
│   ├── webhook.md                      # Webhook integration guide for CI
│   ├── storage-and-databases.md        # Storage & database architecture guide
│   └── openapi.yaml                    # REST API spec (also at pmcluster/docs/)
└── README.md
```

---

## Getting Started

### Prerequisites

On the manager node:
- Docker Engine 20.10+ installed (`docker --version`).
- Swarm initialised (`docker swarm init --advertise-addr <ip>`). pmcluster intentionally does NOT init Swarm — that decision belongs to the operator.
- TLS: pick one
  - **Let's Encrypt (recommended for new clusters):** point `*.<your-domain>` at the manager's public IP, ensure port 80 is reachable. Pmcluster + Traefik handle issuance and renewal.
  - **Operator-supplied cert + key:** any CA, including a wildcard from your existing internal/purchased PKI.

### 1. Install pmcluster

One-line install (latest release):

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | bash
```

The script picks the right `darwin|linux` × `arm64|amd64` archive from the [GitHub releases](https://github.com/hazemarian/poor-man-cluster/releases), verifies its SHA256, and drops the binary in `/usr/local/bin/pmcluster` (override with `PREFIX=…` or pin a version with `VERSION=v0.2.71`).

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

The setup wizard walks you through domain, TLS, Traefik admin user, SSO (optional GitHub OAuth), and edge login preference, then runs `cluster up` (fresh) or `cluster update` (existing cluster). All questions have flags for scripted use — see `pmcluster setup --help`.

**Manual / scripted:**

```bash
pmcluster init                          # creates ~/.pmcluster, prints admin token

# Let's Encrypt (recommended)
pmcluster cluster up \
  --domain=example.com \
  --acme-email=ops@example.com

# OR operator-supplied cert
pmcluster cluster up \
  --domain=example.com \
  --cert=/path/to/cert.pem \
  --key=/path/to/key.pem
```

`cluster up` is **init-only** — if a cluster already exists it will error and tell you to run `cluster update`. This prevents accidental re-bootstrapping. Configs are stored **only in the SQLite DB** (no `~/.pmcluster/config/*.yml` disk files); they are rendered at deploy time from embedded templates.

It will:
1. Preflight (Docker reachable, Swarm active, this node is a manager)
2. Create the `traefik-net` and `monitoring-net` overlay networks
3. Configure TLS — either wire ACME into Traefik (HTTP-01 via the `:80` entrypoint) or load the operator's cert/key into Swarm secrets
4. Generate random bootstrap passwords for Traefik / OpenObserve / the edge console, store encrypted in SQLite, mirror to Swarm secrets
5. Render the OTel + Traefik dynamic configs in-process and create them as Docker configs (Swarm replicates to every node)
6. Deploy the `infra`, `edge`, `observability`, `backup`, and (if SSO enabled) `sso` stacks via `docker stack deploy`

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

```bash
pmcluster serve                         # foreground; ^C to stop
```

Production: supervise via systemd. A starter unit ships at [`pmcluster/contrib/systemd/pmcluster.service`](pmcluster/contrib/systemd/pmcluster.service) — copy to `/etc/systemd/system/`, edit the `User=` line for your install user, then `systemctl enable --now pmcluster`.

Inspect the daemon's audit log:

```bash
pmcluster logs --tail=200               # most recent JSON lines
pmcluster logs --since=24h --follow     # stream new entries
pmcluster logs --tail=200 | jq 'select(.level=="error")'
```

JSON files live at `~/.pmcluster/logs/pmcluster-YYYY-MM-DD.log` and are swept after 14 days.

### 4. Use the operator console

`cluster up` deploys `pmcluster-edge`, so `https://pmcluster.<your-domain>` is a full management UI as soon as DNS resolves:

- **Stacks** — view deployed stacks and revision history, deploy new manifests, roll back
- **Services** — live replica health, task/crash history, log tailing, restart, exec
- **Webhooks** — create/list/remove CI webhook sources (secrets shown once)
- **API keys** — issue and revoke bearer tokens for other users/CI systems
- **TLS** — rotate the cluster's own (main) certificate and manage per-host certificates for customer domains, with expiry warnings
- **Backups** — trigger snapshots and view the audit log
- **Users** — manage console accounts with RBAC roles (admin > operator > viewer); admins only. Hidden from nav when behind SSO/admin-auth.
- **Settings / overview** — cluster info and management (incl. "Apply to swarm" sync button)
- **External** — nav links to `https://observ.<domain>` (OpenObserve) and `https://traefik.<domain>/dashboard/` (Traefik dashboard)

The console authenticates against a dedicated `edge` daemon user and stores its session state in its own SQLite volume. When the console is deployed behind Traefik's `admin-auth` or `sso-auth` gate (`/web/*`), its own login page is disabled (`EDGE_LOGIN_DISABLED=true`) — you authenticate at the Traefik level and the console runs as a synthetic admin session.

### 5. Add worker nodes (optional)

On each additional machine — install Docker, then a single command:

```bash
docker swarm join --token <TOKEN> <MANAGER_IP>:2377
```

Get the token from the manager:

```bash
docker swarm join-token worker
```

The manager automatically schedules the OTel Collector and the volume backup agent on the new node. No script, no config file.

#### Required Firewall Ports (manager ↔ worker)

| Port | Protocol | Purpose |
|------|----------|---------|
| 2377 | TCP | Swarm cluster management |
| 7946 | TCP/UDP | Node-to-node communication |
| 4789 | UDP | Overlay network traffic (VXLAN) |

---

## Tear Down

```bash
pmcluster cluster down --yes              # removes infra/edge/observability/backup stacks
pmcluster cluster down --yes --purge      # also removes pmcluster-managed secrets,
                                          # configs, and overlay networks
```

`~/.pmcluster` (encryption key + SQLite) is **not** removed by `--purge`. Delete the directory manually for a fully clean slate; understand that doing so makes encrypted credentials unrecoverable.

---

## Deploying Your Own Services

`pmcluster deploy` accepts a small higher-level DSL and translates it to the verbose Docker Swarm Compose YAML, eliminating the boilerplate (Traefik labels, networks block, app/env/version labels, secret/network wiring, restart/update policies).

> Full field-by-field reference: [`docs/dsl.md`](docs/dsl.md).

### Manifest shape

```yaml
app: donation-campaign           # required — stack name
env: production                  # required — environment
domain: example.com              # required — root domain for Traefik routing
registry: ghcr.io/nextrum-sy     # optional — image registry prefix
version: latest                  # optional — default image tag (overridable via --version)
repo_url: https://github.com/... # optional — metadata only (shown in stack list)
env_file: .env                   # optional — load env vars from a file

backup_before_deploy: true       # optional — snapshot volumes before deploy
strict_backup: true              # optional — abort the deploy if that backup fails

secrets:                         # optional — external Swarm secrets (must pre-exist)
  - donation_campaign_db_password

volumes: [db_data]               # optional — named volumes to create

services:                        # required — one or more services
  db:
    image: postgres:14-alpine
    placement: manager           # optional — manager | worker
    volumes: [db_data:/var/lib/postgresql/data]
    env: { POSTGRES_DB: donation_campaign, POSTGRES_USER: user }
    secrets: [donation_campaign_db_password]
    healthcheck: { type: pg_isready }

  migration:
    image: ${registry}/${app}:${version}
    command: [./migrate]         # optional — override command (entrypoint also supported)
    run_once: true               # optional → restart_policy: condition: none

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

**Service fields:** `image` (required), `replicas`, `run_once`, `placement`, `command`, `entrypoint`, `env`, `volumes`, `secrets`, `expose`, `healthcheck`, `update`, `skip_filelog` (exclude the service from the OTel log-tailing receiver when the app ships logs via OTLP itself).

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
```

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
    (`GET /api/cluster/rendered`).
  - **`service`** — values belonging to one stack, reached from the stacks
    list via the **Config** button (page `/stacks/<name>/config`). Create
    forms prefill the `<stack>_` name prefix and record the owning stack.
- CLI cheat-sheet (both `create` commands accept `--stack <name>` for
  service-scope values):
  - `pmcluster secret create <name> [value] [--scope cluster|service] [--stack <name>]` / `list` / `show <name>` / `verify <name> <value>` / `delete <name>`
  - `pmcluster config create|list|get|edit|history|rollback <name> [--scope ...] [--stack <name>]`
- REST API: `GET/POST /api/secrets` (`?scope=`/`?stack=` filters),
  `PUT/DELETE /api/secrets/{name}`, `GET /api/secrets/{name}/value`,
  `GET/POST /api/configs` (`?scope=`/`?stack=` filters),
  `GET/PUT/DELETE /api/configs/{name}`, `GET /api/configs/{name}/versions`,
  `POST /api/configs/{name}/rollback`, and `POST /api/update` (apply platform
  config to the swarm) — see `docs/openapi.yaml`.

### Deploy

Locally, on the manager:

```bash
pmcluster deploy ./donation-campaign.yaml [--version <tag>] [--app <name>] [--repo <url>]
```

Remotely, via the HTTP API:

```bash
curl -X POST https://pmcluster.example.com/api/stacks \
     -H "Authorization: Bearer <admin-token>" \
     -H "Content-Type: application/json" \
     -d "{\"manifest\": $(jq -Rs . < donation-campaign.yaml)}"
```

### Inspect, list, roll back

```bash
pmcluster stack list                      # name, current revision, last update
pmcluster stack show donation-campaign    # metadata + recent revisions (→ marks current)
pmcluster rollback donation-campaign 1778439014   # re-apply a stored revision
```

Every deploy gets a unix-timestamp revision id. Rollback re-applies a stored revision as a NEW revision (preserves the audit trail — both deploys are recorded). The corresponding REST endpoints are `GET /api/stacks`, `GET /api/stacks/{name}`, `GET /api/stacks/{name}/revisions/{rev}`, `POST /api/stacks/{name}/rollback`. A stack can be removed (services `docker stack rm`, named volumes, mounted Swarm secrets, plus the DB record, its revisions and its service-scope configs/secrets) from the console's stacks list — `DELETE /api/stacks/{name}`.

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

CI side — sign the request with HMAC-SHA256 keyed by that secret string and POST to `/webhook/<source>`. Two headers are required: a current Unix-seconds timestamp and the signature over `timestamp + body` (no separator). Timestamps outside a 5-minute window are rejected, which blocks replay attacks.

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

### Pre-deploy backups

Add `backup_before_deploy: true` to a manifest's top level and pmcluster will trigger an offen volume snapshot before `docker stack deploy`. The deploy proceeds even if the backup fails (a flaky backup container shouldn't block urgent rollouts) — failures show up loud in `pmcluster backup list`.

```bash
pmcluster backup create                       # on-demand snapshot
pmcluster backup list                         # audit log of every triggered run
```

In addition to the per-node app-volume agent, the backup stack runs a manager-only **control-plane agent** (`control-plane-backup`). Every night it archives `~/.pmcluster` itself — the SQLite DB (users, API keys, webhook secrets, credentials, revisions), the encryption key, TLS certs, and rendered configs — into the same `/var/backups/docker-volumes` directory under a `pmcluster-ctlplane-` prefix (30-day retention). This closes the "lost manager disk = full re-bootstrap" gap: app data and the control plane both ride along in the nightly archive.

Restore is a known design gap, sketched in [`pmcluster/docs/restore-design.md`](pmcluster/docs/restore-design.md).

## Storage & Databases

pmcluster standardizes on a **single host root mount** at `/var/stack/data` for all application data. How you configure, format, or replicate the underlying host storage is entirely up to you — from a single VPS local SSD to multi-node replicated disks. Databases run **inside the cluster** (no managed DB services), with replication handled either at the database level (CockroachDB / MariaDB Galera / Patroni) or the OS block level (DRBD + LINSTOR), and every byte is backed offsite to S3-compatible storage (R2 / S3 / Hetzner Storage Box) by the backup containers.

The full decision flowchart, storage matrix, database strategies, backup rules for replicated environments, and a quickstart example stack live in [`docs/storage-and-databases.md`](docs/storage-and-databases.md).

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

Per-host certs use the same table and flow as the cluster's own certificate: the PEM bytes become versioned Swarm secrets (`hostcert-<host>_vNNN` / `hostkey-<host>_vNNN`) and the metadata (expiry, SANs, hashes) is recorded in the `site_certs` DB table — they are **never** written to the manager's filesystem. Adding or removing one re-renders the Traefik dynamic config and re-deploys the infra stack so it takes effect immediately; pass `--no-refresh` to defer the refresh to the next `pmcluster cluster update`. The cluster's own wildcard certificate (`cluster up --cert/--key`) is never touched by these commands.

---

## REST API & API Keys

The daemon exposes a JSON REST API under `/api/*` (Bearer auth) plus the unauthenticated `GET /health` liveness probe. The full spec lives at [`pmcluster/docs/openapi.yaml`](pmcluster/docs/openapi.yaml). Highlights:

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/me` | Current user |
| `GET` | `/api/cluster/info` | Cluster + Swarm status |
| `GET` | `/api/nodes` | Swarm nodes |
| `GET` | `/api/usage` | Config/secret usage graph (which stacks reference each) |
| `GET` | `/api/cluster/settings` | List all cluster settings (12 allowlisted keys) |
| `PUT` | `/api/cluster/settings` | Atomic update of cluster settings (does NOT redeploy) |
| `GET`/`POST` | `/api/stacks` | List / deploy a stack |
| `GET` | `/api/stacks/{name}` | Stack detail + revisions |
| `GET` | `/api/stacks/{name}/revisions/{rev}` | A stored revision |
| `POST` | `/api/stacks/{name}/rollback` | Roll back to a revision |
| `DELETE` | `/api/stacks/{name}` | Remove a stack (services, volumes, secrets, record) |
| `GET`/`POST` | `/api/backups` | List / trigger backups |
| `GET` | `/api/backups/{id}/files` | List files inside a backup run's archive |
| `POST` | `/api/backups/{id}/restore` | Restore a backup to a destination root |
| `GET`/`PUT`/`DELETE` | `/api/tls/hosts` | Per-host TLS certs |
| `GET`/`PUT` | `/api/tls/site` | Cluster's own (main) certificate |
| `GET`/`POST` | `/api/secrets` | DB-backed secrets (list/create) |
| `GET`/`POST` | `/api/configs` | DB-backed configs (list/create) |
| `GET`/`PUT`/`DELETE` | `/api/configs/{name}` | Config content + versions/rollback |
| `GET`/`POST`/`DELETE` | `/api/webhooks` | Webhook sources |
| `GET` | `/api/webhooks/{source}/deliveries` | Webhook delivery history (newest first) |
| `GET`/`POST` | `/api/api_keys` | API keys |

API tokens are created with `pmcluster user create <name>` or the console. They use the format `pmc_<token_id>_<secret>` (a public 8-hex-char lookup id plus a base64url secret) so the daemon can find the row without scanning every user; the plaintext token is shown once and only a hash is stored.

```bash
curl -H "Authorization: Bearer pmc_a1b2c3d4_..." \
  https://pmcluster.example.com/api/cluster/info
```

The edge proxy applies per-IP rate limits: `200 req/s` (burst 400) for `/api/*` and `40 req/s` (burst 60) for `/webhook/*`; exceeded requests get `429` with `Retry-After: 1`.

---

## Minimum Hardware

| Node | CPU | RAM | Notes |
|------|-----|-----|-------|
| Manager | 2 vCPU | 4 GB | Traefik + OpenObserve + OTel Collector + Backup Agent + pmcluster-edge + pmcluster |
| Worker | 1 vCPU | 1 GB | OTel Collector + Backup Agent + your application workloads |

OpenObserve alone needs ~512 MB RAM at idle. On a manager with less than 4 GB it will compete with Traefik under load. `pmcluster-edge` is tiny (64–256 MB, capped).

---

## TLS Certificates & Renewal

**Let's Encrypt mode (`--acme-email`):** Traefik handles issuance and renewal automatically. Certs are stored in the `infra_traefik_acme` Docker volume on the manager. Nothing to rotate manually.

**Operator-supplied mode (`--cert`/`--key`):** certificates live in Swarm secrets `cert` and `key`. To rotate:

```bash
docker secret rm cert key
pmcluster cluster update                 # re-applies stored cert/key from the DB
docker service update --force infra_traefik
```

Switching an existing cluster between ACME and operator-cert mode requires `--force-tls-mode`. After renewing a certificate on disk, `pmcluster cluster update` re-applies the stored cert/key without a full bring-up. For per-host (customer-domain) certificates, use `pmcluster tls hosts …` — see [Per-Host TLS](#per-host-tls-customer-domains).

### Renewing the main certificate in place

The cluster's own (main) certificate can be rotated without a full bring-up, from any of the three entry points — the result is identical: the pair is validated against the cluster domain, stored in the DB, materialized as versioned Swarm secrets (`cert_vN`/`key_vN`), wired into the Traefik dynamic config, and its metadata (validity window, SANs, hashes) recorded in the DB for expiry monitoring.

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

---
name: poor-man-cluster-deploy
description: >
  Deploy services onto a Poor Man's Stack cluster using the pmcluster CLI, REST API,
  or the operator console. Covers the pmcluster DSL manifest format, deploy/rollback/
  list/show commands, webhook setup for CI (HMAC + required timestamp), registry
  credential management, per-host TLS for customer domains, backup triggering, API
  tokens, and cluster lifecycle (up/update/down/status). Use when deploying
  applications to a Docker Swarm-based cluster managed by pmcluster, setting up
  CI/CD pipelines targeting pmcluster, or operating the pmcluster-edge console.
  Do NOT use for Docker/Kubernetes deployments outside this stack.
---

# Poor Man's Stack — Deploy Skill

Deploy applications to a Docker Swarm cluster managed by `pmcluster`, the control plane from [poor-man-cluster](https://github.com/hazemarian/poor-man-cluster).

**Public origin:** everything (console + REST API + webhooks) is served at `https://pmcluster.<your-domain>` by the `pmcluster-edge` service. The daemon itself listens only on `http://127.0.0.1:9090` (host-local).

Full references in the repo: [`docs/dsl.md`](https://github.com/hazemarian/poor-man-cluster/blob/main/docs/dsl.md), [`docs/webhook.md`](https://github.com/hazemarian/poor-man-cluster/blob/main/docs/webhook.md), [`pmcluster/docs/openapi.yaml`](https://github.com/hazemarian/poor-man-cluster/blob/main/pmcluster/docs/openapi.yaml).

## Prerequisites

Before any deploy operation, verify the environment:

```bash
# Is Docker running and Swarm active?
docker info --format '{{.Swarm.LocalNodeState}}'   # must be "active"

# Is pmcluster installed and initialized?
pmcluster version
pmcluster cluster status
```

If pmcluster is not installed:

```bash
# Basic install:
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | bash
pmcluster init

# With private registry credentials (e.g. GHCR):
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | \
  PMCLUSTER_REGISTRY="ghcr.io=my-username=ghp_abc123" bash
pmcluster init
```

Install env vars: `VERSION` (pin release), `PREFIX` (install path), `PMCLUSTER_USER` (systemd user), `PMCLUSTER_REGISTRY` (comma-separated `host=user=token` entries).

If the cluster is not up, run the **interactive setup wizard** or bring it up with flags:

```bash
# Interactive wizard (recommended) — prompts for domain, TLS, admin user, SSO, etc.:
pmcluster setup

# Non-interactive: flags for every prompt:
pmcluster setup --domain=example.com --acme-email=you@host \
  --traefik-admin-user=admin --sso-enabled --sso-client-id=... --sso-client-secret=...
```

The wizard persists settings, then runs `cluster up` (fresh install) or `cluster update` (existing cluster).

Alternatively, use `cluster up` directly with flags:

```bash
pmcluster cluster up --domain=<your-domain> --openobserve-email=admin@<your-domain> --cert=<cert.pem> --key=<key.pem>
# OR with Let's Encrypt (HTTP-01; DNS must point here and port 80 reachable):
pmcluster cluster up --domain=<your-domain> --openobserve-email=admin@<your-domain> --acme-email=<you@host>
```

> **`cluster up` is init-only.** It refuses to run on an already-initialised cluster — use `cluster update` instead. When no data flags are supplied, it falls back to the interactive setup wizard.

`cluster up` deploys four stacks: `infra`, `edge`, `observability`, `backup` (plus `sso` when enabled). It also mints the `edge_admin` console password and the `edge` daemon API token.

The daemon runs as a systemd service. `cluster up`, `cluster update`, and `pmcluster join` install `/etc/systemd/system/pmcluster.service` and (re)start it — no manual unit management. `serve` is leader-aware: it serves on the Swarm leader and stands by (15s poll) on non-leader managers; on promotion it restores the control-plane database from the newest `pmcluster-ctlplane-*.tar.gz` archive only when the local DB is missing or older (safe with shared storage).

```bash
pmcluster serve   # foreground fallback (non-Linux hosts / manual runs)
```

## Operator Console

`https://pmcluster.<domain>` is a gin + HTMX operator console for stacks (deploy/rollback/delete), webhooks, API keys, per-host TLS, backups, and settings. The stacks list has a **Sync** button per stack that re-deploys from the stored manifest (skipping no-ops when rendered content is unchanged) and a **Delete** button (with confirmation).

- **Login:** user `admin`, password from `pmcluster credentials show edge_admin`.
- It talks to the daemon with a dedicated `edge` API token; you don't manage that token manually.
- **Behind SSO:** When SSO is enabled, the console's own password login is disabled (`EDGE_LOGIN_DISABLED=true`). Users authenticate through the Traefik SSO gate instead.

Prefer the console for interactive work; prefer the CLI/API for automation.

## Traefik Dashboard

The Traefik dashboard is at `https://traefik.<domain>/dashboard/`. The router rule is `Host(...) && (PathPrefix(/api) || PathPrefix(/dashboard))`. Without SSO it is gated by `admin-auth` (htpasswd basicAuth); with SSO enabled it is gated by `sso-auth` (forwardAuth through oauth2-proxy).

## SSO (Single Sign-On)

When enabled during `pmcluster setup`, the `sso` stack deploys [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/) at `sso.<domain>`. GitHub OAuth with optional org restriction.

- **Callback URL:** `https://sso.<domain>/oauth2/callback`
- **Traefik middleware switch:** admin-auth (htpasswd basicAuth) → sso-auth (forwardAuth through oauth2-proxy). The `traefik_dashboard` credential is no longer used for gating.
- **oauth2-proxy env vars (critical — plural forms required):**
  - `OAUTH2_PROXY_COOKIE_DOMAINS` (plural — singular `COOKIE_DOMAIN` is silently ignored in v7)
  - `OAUTH2_PROXY_WHITELIST_DOMAINS` (plural — singular is silently ignored in v7)
  - `OAUTH2_PROXY_REVERSE_PROXY: "true"` (requires `trusted-proxy-ip` / `TRUSTED_PROXY_CIDRS` for hardening)
- **Console behind SSO:** `EDGE_LOGIN_DISABLED=true` — the edge console's own login page is bypassed; Traefik's SSO gate handles identity. Console is at `https://pmcluster.<domain>/web/`.

### Enabling SSO on an existing cluster

```bash
pmcluster setup --sso-enabled --sso-client-id=<id> --sso-client-secret=<secret> [--sso-github-org=<org>]
# then:
pmcluster cluster update
```

The `sso_cookie_secret` credential is minted automatically by credential bootstrap. oauth2-proxy needs a base64-encoded 32+ byte cookie secret.

## OpenObserve

OpenObserve is deployed by the `observability` stack at `observ.<domain>`. There is **no provisioning API or ingestion token** — both the OTel collector and the Traefik auto-auth header use the root admin credential (`openobserve_admin` / `zo_root_user_password` secret). The observability UI is gated by the same admin-auth or sso-auth middleware as the console (no separate OpenObserve login prompt).

## Authentication & API Tokens

pmcluster uses Bearer tokens for API authentication. Tokens are generated once by `pmcluster init` and printed to stdout — save them immediately; they cannot be recovered.

### Token Format

As of pmcluster v2, tokens use a structured format:

```
pmc_<hex_token_id>_<base64_secret>
```

- `pmc_` — fixed prefix identifying v2 tokens
- `hex_token_id` — 8 hex chars (4 random bytes), the public index used for fast database lookup
- `base64_secret` — 32 random bytes, base64url-encoded (~43 chars); this is the sensitive part

### Creating additional users

```bash
pmcluster user create my-user     # prints the token ONCE — save it
pmcluster user list               # id, name, created (never token material)
pmcluster user remove my-user     # revoke a token immediately (refuses "edge")
```

All API requests pass the token as a Bearer header. Against the public origin:

```bash
curl -H "Authorization: Bearer pmc_a1b2c3d4_..." https://pmcluster.example.com/api/me
# Daemon-local (e.g. on the manager host):
curl -H "Authorization: Bearer pmc_a1b2c3d4_..." http://127.0.0.1:9090/api/me
```

### Rate Limiting

The `pmcluster-edge` proxy enforces per-real-IP rate limits:

| Path prefix   | Limit                |
|---------------|----------------------|
| `/api/*`      | 200 req/s, 400 burst |
| `/webhook/*`  | 40 req/s, 60 burst   |
| `/health`     | unlimited            |

Exceeding the limit returns HTTP 429 with `Retry-After: 1`. Repeated rate-limit trips or upstream auth/5xx failures can also get the IP auto-banned (HTTP 403) for a short window (default 10 min).

## Remote CLI mode (run off-node)

Every data command can run against the daemon API instead of a local
`~/.pmcluster` + Docker socket. Set the API base URL and a bearer token:

```bash
export PMCLUSTER_API_URL=https://pmcluster.example.com   # or --api-url
export PMCLUSTER_API_TOKEN=pmc_<id>_<secret>                # or --api-token

# Now all of these work from any machine:
pmcluster config list
pmcluster secret list --scope service
pmcluster deploy app.yaml
pmcluster stack list
pmcluster rollback my-app 1789626082
pmcluster webhook list
pmcluster webhook deliveries github-prod
pmcluster user create ci-bot
pmcluster backup create
pmcluster backup browse <id>
pmcluster backup restore <id>
pmcluster tls site show
pmcluster cluster settings
pmcluster usage
```

- The token comes from `pmcluster user create <name>` (or the `edge` daemon
  token from `pmcluster credentials show edge_api_token`).
- **Local-only** (need the node's SQLite + Docker socket): `init`,
  `cluster up/update/status/down`, `credentials *`, `node`, `registry`,
  `logs`, `serve`.
- Remote `tls hosts add` uploads PEM text and refreshes Traefik through the
  daemon; remote `config edit` re-stamps the current binary version, then run
  the console **Apply to swarm** (or `cluster update` on the node) to reach
  the swarm.

## Manifest DSL Format

Create a `.yaml` manifest for each service. The schema (see [`docs/dsl.md`](https://github.com/hazemarian/poor-man-cluster/blob/main/docs/dsl.md) for the full reference):

```yaml
app: my-app                    # required — application/stack name
env: production                # required — environment (staging/production/etc.)
domain: example.com            # required — root domain for Traefik routing
registry: ghcr.io/my-org       # optional — container registry
version: latest                # optional — default image tag (overridable at deploy)
repo_url: https://github.com/my-org/my-app   # optional — metadata only
env_file: .env                 # optional — env file path

backup_before_deploy: true     # optional — trigger offen volume snapshot before deploy
strict_backup: true            # optional — abort deploy if the pre-deploy backup fails
                               #   (requires backup_before_deploy: true)

secrets:                       # optional — external Swarm secrets (must already exist)
  - my_app_db_password

services:                      # required — one or more service definitions
  service-name:
    image: postgres:14-alpine
    # OR with variable substitution:
    # image: ${registry}/${app}:${version}

    placement: manager         # optional — manager | worker | (empty)
    replicas: 2                # optional — default 1 (ignored when run_once)
    run_once: true             # optional — restart_policy: condition: none (one-off jobs)
    skip_filelog: true         # optional — exclude from OTel log tailing (app ships OTLP itself)
    command: [./migrate]       # optional — override command (entrypoint also supported)

    expose:                    # optional — auto-wire Traefik
      port: 8080               #   container-side port
      host: api.${app}.${domain}
      aliases: [api.customer.com]   # extra hostnames → own Traefik router
      cors_disabled: false          # opt out of the CORS middleware

    volumes:
      - db_data:/var/lib/postgresql/data

    env:
      POSTGRES_DB: my_app
      POSTGRES_USER: my_user

    secrets:
      - my_app_db_password

    healthcheck:
      type: pg_isready         # or: http (uses expose.port; optional `path`, default /)
      # full form also supported: test / interval / timeout / retries

    update:                    # optional — Swarm rolling-update policy
      parallelism: 1           #   default 1
      delay: 10s               #   default 10s
      order: start-first       #   start-first (default) | stop-first
```

Named volumes are **auto-collected** from service mounts — there is no top-level `volumes:` block (a top-level `volumes:` key is rejected by the strict DSL). Every container volume — named or bind — is forced under a single host root: `<volume_root>/<app>/<name>` (default `/var/stack/data`, configurable via `pmcluster setup --volume-root` or `cluster settings set volume_root=`). Named volumes keep named-volume semantics via `driver_opts {type:none, o:bind, device:<root>/<app>/<name>}` so Docker's first-use ownership copy still runs for DB images; host bind mounts are relocated to `<root>/<app>/<basename>`.

### Variable Substitution

Auto-resolved: `${app}`, `${env}`, `${version}`, `${registry}`, `${domain}`. OS environment variables: `${env:VAR}` (error if unset).

### DB-backed secrets & configs in `env`

`env` values can reference pmcluster's DB stores instead of hard-coding values:

```yaml
env:
  ADMIN_PASS: secrets(my_app_db_password)   # stored secret → mount path /run/secrets/...
                                            # (must also be in the service secrets: array)
  ADMIN_ENABLED: config(my_app_config)      # stored config content
```

- Secrets live **AES-256-GCM encrypted** in `data.db` (key `~/.pmcluster/.encryption_key`). The plaintext is shown **once** at creation; afterwards only the **sha256 hash** is shown. Values can be **revealed on demand** (with confirmation in the UI) and **edited** in place (`PUT /api/secrets/{name}`).
- Configs keep a full **version history**; `rollback` restores an old value.
- Both have `cluster` (platform) and `service` (app) scopes, plus an owning `stack` for service scope. Resolved at **deploy time** against the DB — rotate then re-deploy to pick up the new value.
- Management:
  - `pmcluster secret create <name> [value]` (prompt/stdin ok; `--scope` / `--stack` flags) / `list` / `show <name>` / `verify <name> <value>` / `delete <name>`
  - `pmcluster config create|list|get|edit|history|rollback <name>` (`--scope` / `--stack` flags)
- The operator console manages them from **Settings** (cluster-scope configs/secrets — the platform's own templates; editing re-stamps the current binary version and **Apply to swarm** (`POST /api/update`) re-runs `cluster update`) and from **`/stacks/<name>/config`** (service-scope values for that stack, reached via the **Config** button in the stacks list). Create forms prefill the `<stack>_` name prefix. Every cluster update snapshots the rendered platform configs (post-substitution YAML) into the DB; they're viewable read-only from Settings → **Rendered cluster configs**.
- Validation fails on malformed references (`config(name` / `secrets()`), on `secrets(name)` env refs that aren't mounted in the service `secrets:` array, and at deploy time if the named secret/config doesn't exist. Multi-line content can't be injected into env — file-mount it via the `secrets:` array instead.

### Validation Rules

- Strict YAML — unknown keys are rejected at every level.
- `app` must match `^[a-z][a-z0-9_-]{0,62}$`; `env`, `domain`, `services` are required.
- `image` is required per service.
- `replicas` and `run_once` are mutually exclusive.
- `expose` requires `port` (1..65535) and `host`.
- `healthcheck.type` must be `http`, `pg_isready`, or empty (full form).
- `update.order` must be `start-first` or `stop-first`.

## Deploy Workflow

### 1. Create the manifest

Write a `.yaml` manifest file for your service. Start from the template above.

### 2. Validate

`pmcluster deploy` validates and translates the DSL in one step; malformed manifests fail before any Swarm change. To inspect the generated Compose without deploying, deploy first and read the rendered manifest from the revision (`pmcluster stack show <app>` or the console's revision view).

### 3. Deploy

```bash
pmcluster deploy ./manifest.yaml

# Override version at deploy time:
pmcluster deploy ./manifest.yaml --version v1.2.3

# Override app name:
pmcluster deploy ./manifest.yaml --app custom-name

# Attach a repo URL (metadata):
pmcluster deploy ./manifest.yaml --repo https://github.com/org/repo
```

Each deploy creates a new revision keyed by Unix timestamp.

### 4. Verify

```bash
pmcluster stack list                        # all deployed stacks
pmcluster stack show my-app                 # metadata + revision history (→ marks current)
docker service ls                           # raw Swarm view
docker service ps my-app_api                # container-level status
```

## Rollback

```bash
pmcluster stack show my-app                 # find the revision you want
pmcluster rollback my-app 1719000000        # redeploy that revision
```

Rollback creates a NEW revision (preserves audit trail) pointing at the old compose. Both forward and backward deploys are recorded.

## REST API Deploy (Remote / CI)

When deploying from CI or a remote machine, use the REST API at the public origin:

```bash
BASE=https://pmcluster.example.com

# Deploy a manifest
curl -X POST $BASE/api/stacks \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d "{\"app_name\":\"my-app\",\"version\":\"v1.2.3\",\"manifest\": $(jq -Rs . < manifest.yaml)}"

# List stacks
curl $BASE/api/stacks -H "Authorization: Bearer <admin-token>"

# Show a stack
curl $BASE/api/stacks/my-app -H "Authorization: Bearer <admin-token>"

# Rollback
curl -X POST $BASE/api/stacks/my-app/rollback \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"revision": 1719000000}'

# Remove a stack (services, named volumes, mounted secrets, record + configs/secrets)
curl -X DELETE $BASE/api/stacks/my-app -H "Authorization: Bearer <admin-token>"
```

Other endpoints: `GET /api/cluster/info`, `GET /api/nodes`, `GET /api/stacks/{name}/revisions/{rev}`, `GET|POST /api/backups`, `GET /api/backups/{id}/files`, `POST /api/backups/{id}/restore`, `GET|POST|DELETE /api/tls/hosts`, `GET|POST|DELETE /api/webhooks`, `GET /api/webhooks/{source}/deliveries`, `GET|POST /api/api_keys`, `GET /api/cluster/settings`, `PUT /api/cluster/settings`, `GET /api/usage`. Full spec in `pmcluster/docs/openapi.yaml`. The console's stacks list has a **Delete** button (with confirmation) per stack — same as `DELETE /api/stacks/{name}`.

## CI/CD Webhook Setup

Webhooks are authenticated via HMAC-SHA256, not Bearer tokens. Each webhook source has a shared secret; the CI system computes a signature over the request and pmcluster verifies it server-side. **A timestamp header is required** for replay protection.

### 1. Create a webhook source on the manager

```bash
pmcluster webhook add github-prod
# Prints a 64-char hex secret ONCE — save it.
```

### 2. Configure CI (e.g., GitHub Actions)

Two headers are required on every webhook request:

| Header                   | Value                                    |
|--------------------------|------------------------------------------|
| `X-Pmcluster-Timestamp`  | Unix seconds of the request              |
| `X-Pmcluster-Signature`  | `sha256=<hex>` HMAC over timestamp+body  |

**The HMAC input is**: `timestamp_as_decimal_string + body` (no separator). Timestamps more than 5 minutes old (or from the future) are rejected.

Production pattern — build the body with `jq`, sign `timestamp + body`, POST, and fail the job on a non-200:

```yaml
name: Deploy
on:
  push:
    branches: [main]
    paths: ["deploy/my-app.yaml"]
jobs:
  deploy:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5
      - name: Deploy via pmcluster webhook
        env:
          PMCLUSTER_WEBHOOK_URL: ${{ secrets.PMCLUSTER_WEBHOOK_URL }}
          PMCLUSTER_WEBHOOK_SECRET: ${{ secrets.PMCLUSTER_WEBHOOK_SECRET }}
        run: |
          FILE="deploy/my-app.yaml"
          MANIFEST_CONTENT=$(cat "${FILE}")
          APP=$(printf '%s' "${MANIFEST_CONTENT}" | grep '^app:' | awk '{print $2}')
          VERSION=$(printf '%s' "${MANIFEST_CONTENT}" | grep '^version:' | awk '{print $2}')

          BODY=$(jq -n --arg app_name "${APP}" --arg version "${VERSION}" \
            --arg manifest "${MANIFEST_CONTENT}" \
            '{app_name: $app_name, version: $version, manifest: $manifest}')

          TIMESTAMP=$(date +%s)
          SIG=$(printf '%s' "${TIMESTAMP}${BODY}" \
            | openssl dgst -sha256 -hmac "$PMCLUSTER_WEBHOOK_SECRET" | awk '{print "sha256=" $2}')

          HTTP_CODE=$(curl -s -o /tmp/resp.txt -w "%{http_code}" \
            --connect-timeout 10 --max-time 30 \
            -X POST "$PMCLUSTER_WEBHOOK_URL" \
            -H "X-Pmcluster-Timestamp: $TIMESTAMP" \
            -H "X-Pmcluster-Signature: $SIG" \
            -H "Content-Type: application/json" \
            -d "$BODY")

          echo "HTTP ${HTTP_CODE}:"; cat /tmp/resp.txt
          [ "$HTTP_CODE" = "200" ] || { echo "deploy failed"; exit 1; }
```

The payload is `{"app_name","version","manifest","repo_url"?}` where `manifest` is the **entire manifest YAML as a string**.

### 3. Responses

| Status | Meaning |
|--------|---------|
| `200` | Deploy accepted → `{"stack","revision"}` |
| `400` | Missing source, bad JSON, or manifest validation failure |
| `401` | Any HMAC failure (bad secret, unknown source, bad/missing timestamp, bad signature) |
| `413` | Body > 1 MB |
| `502` | `docker stack deploy` failed |
| `429` | Edge rate limit — honor `Retry-After: 1` |

All 401s are identical by design — no information leak. Use `pmcluster webhook list` to see `last_used_at`.

List webhooks: `pmcluster webhook list` · Remove: `pmcluster webhook remove github-prod`

Full guide: [`docs/webhook.md`](https://github.com/hazemarian/poor-man-cluster/blob/main/docs/webhook.md).

## TLS Certificates

There are two independent certificate flows — the cluster's own (main) certificate and per-host certificates for customer domains. Both validate the PEM pair, materialize versioned Swarm secrets and refresh Traefik; only the target differs.

### Main certificate (the cluster's own domain)

Rotate the cluster's own certificate in place — no full bring-up:

```bash
pmcluster tls site show                                    # domain, expiry, SANs, secret names, hashes
pmcluster tls site set --cert-file new.pem --key-file new.key.pem
# or inline: pmcluster tls site set --cert "$(cat new.pem)" --key "$(cat new.key.pem)"
```

The pair is validated against the persisted cluster domain, written to `~/.pmcluster/config/site/{cert,key}.pem` (0600), materialized as `cert_vN`/`key_vN` Swarm secrets, wired into the Traefik dynamic config, and its metadata recorded in the DB. Same flow via the console TLS page (**Main certificate** card) or `PUT /api/tls/site`.

Certificates within **30 days** of expiry are flagged in the console and warned about on every `cluster up`/`cluster update` — operator certs are not auto-renewed. (ACME-mode clusters renew automatically; no row is recorded.)

### Per-host TLS (customer domains)

Serve the same app on a customer's own domain with its own certificate. Per-host certs use the **same table and flow as the main certificate** — the PEM bytes become versioned Swarm secrets and the metadata lands in the same `site_certs` DB table (the cluster's own domain is just one row among them). The only difference is at Traefik render time: the main-domain row feeds the default-cert block, every other row becomes an extra `tls.certificates` entry referencing its own secrets (`hostcert-<host>_vNNN` / `hostkey-<host>_vNNN`). Per-host certs are **never written to the manager's filesystem**.

1. Add the hostname to the service's `expose.aliases` in the manifest (creates the Traefik router).
2. Store the matching cert+key:

```bash
pmcluster tls hosts add api.customer.com --cert-file cert.pem --key-file key.pem
# or inline text:
pmcluster tls hosts add api.customer.com --cert "$(cat cert.pem)" --key "$(cat key.pem)"

pmcluster tls hosts list                      # host, expiry, SANs, secret names
pmcluster tls hosts remove api.customer.com
```

Adding/removing re-renders Traefik and re-deploys the infra stack; use `--no-refresh` to defer to `pmcluster cluster update`. The cluster's own wildcard cert is never touched.

## Private Registries

If your images are in a private registry (e.g. `ghcr.io`):

```bash
# Interactive (prompts for password):
pmcluster registry add ghcr.io

# Scripted (no shell history leak):
echo "$GITHUB_PAT" | pmcluster registry add ghcr.io --username myuser --password-stdin

# At install time (one-shot):
curl -fsSL .../install.sh | PMCLUSTER_REGISTRY="ghcr.io=myuser=$GITHUB_PAT" bash

pmcluster registry list                   # verify
```

`pmcluster serve` auto-replays `docker login` for all persisted registries on startup, so worker nodes can pull private images even after a manager rebuild.

## Pre-Deploy Backups

Add `backup_before_deploy: true` to a manifest to trigger an offen volume snapshot before deployment. By default the deploy proceeds even if the backup fails (best-effort semantics).

To abort the deploy on backup failure, also set `strict_backup: true`:

```yaml
backup_before_deploy: true
strict_backup: true   # abort the deploy if the backup fails
```

The daemon flushes the SQLite WAL before triggering each backup so the snapshot captures a consistent database state.

```bash
pmcluster backup list                      # audit log of every triggered run
pmcluster backup create                    # on-demand snapshot
```

### Control-plane backups (pmcluster's own state)

The backup stack ships a second agent, `control-plane-backup`, that runs **only on the manager node**. Every night it archives `~/.pmcluster` itself — `data.db` (users, API keys, webhook secrets, credentials, stack revisions, TLS metadata), the `.encryption_key`, and `config/` — into the same `/var/stack/backup` directory as the volume backups, with a `pmcluster-ctlplane-` prefix and 30-day retention.

```bash
ls /var/stack/backup/pmcluster-ctlplane-*   # control-plane archives
```

This closes the "lost manager disk = full re-bootstrap" gap: app volumes AND the control plane are both archived daily. The `backup` stack is rendered with the daemon's data dir (`${DATA_DIR}`), so the bind mount points at the real `~/.pmcluster` on the manager.

## Cluster Management

```bash
pmcluster cluster status                   # health overview
pmcluster cluster update                   # content-aware reconcile (DB is source of truth)
pmcluster cluster down --yes               # remove all stacks (infra, edge, observability, backup, sso)
pmcluster cluster down --yes --purge       # also remove secrets, configs, networks
pmcluster node list                        # Swarm nodes
pmcluster node join-token worker           # get join token for new workers
```

`cluster up` is init-only — it refuses to run on an already-initialised cluster (run `cluster update` instead). `cluster update` is the content-aware reconcile: the DB is the source of truth for all platform configs (there are no `~/.pmcluster/config/*.yml` disk files). It compares `rendered_hash` to decide which platform stacks (observability / infra / edge / backup / +sso) need re-deploying. Drift-prune removes services that were dropped from a compose. It is idempotent — a second run with no changes reports `No rendered content changed — nothing to redeploy.`

### Upgrading pmcluster / the edge service

The edge image is pinned to the release version tag — `ghcr.io/hazemarian/pmcluster-edge:<version>` (override with `PMCLUSTER_EDGE_IMAGE=<tag>`). The stack template lives **inside the pmcluster binary** (embedded `edge-stack.yml`). So the correct upgrade path is:

1. **Build + publish the release first** — push a `v*` tag; the release workflow cross-compiles the binaries and pushes the new edge image to GHCR:
   ```bash
   git tag v0.2.84 && git push origin v0.2.84
   ```
2. **Update the binary with install.sh** — it installs the new binary and, because `~/.pmcluster/config.yaml` exists, automatically runs `pmcluster cluster update`:
   ```bash
   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.84 bash
   # or simply: | bash   (resolves latest release)
   ```
3. `cluster update` **re-syncs the platform config templates** from the new binary's embedded copies into the store (the DB is the source of truth; operator edits are preserved), then re-renders. Because the edge-stack.yml content changed, the **edge stack is re-deployed** automatically and pulls the new version-pinned image. OTel/Traefik/cert are re-applied content-aware as usual. A second `cluster update` with no changes reports `No rendered content changed — nothing to redeploy.`

Manual `docker service update --image ... edge_pmcluster-edge` is NOT the supported path — always use the tag → install.sh → `cluster update` flow. On a fresh box, install.sh runs `cluster up` instead (when `PMCLUSTER_DOMAIN` is set). Set `CLUSTER_APPLY=none` to skip the auto apply.

## Credentials

```bash
pmcluster credentials list                 # all managed bootstrap passwords
pmcluster credentials show openobserve_admin  # show a specific one
pmcluster credentials show edge_admin      # the operator console login password
pmcluster credentials rotate openobserve_admin # generate + apply a new password
```

Managed credentials: `traefik_dashboard`, `openobserve_admin`, `edge_admin`, `edge_ui_secret`, `edge_api_token`, `sso_cookie_secret` (SSO only). Edge credentials are persisted by the console on first boot, so `rotate` refuses them.

To extract just the password value: `pmcluster credentials show edge_admin | awk -F": *" '/^password/{print $2}'`.

## Audit Logs

```bash
pmcluster logs --tail=200                  # recent JSON log lines
pmcluster logs --since=24h --follow        # stream in real time
pmcluster logs --tail=200 | jq 'select(.level=="error")'
```

Log files live at `~/.pmcluster/logs/pmcluster-YYYY-MM-DD.log` and auto-sweep after 14 days.

## Troubleshooting

### "Swarm not active"
Run `docker swarm init --advertise-addr <ip>` on the manager. Pmcluster never initializes Swarm for you.

### "Docker not reachable"
Ensure Docker engine is running and the current user has permission (`docker info`).

### "secret already exists"
`pmcluster cluster up` is init-only — it refuses to run on an already-initialised cluster. To reconcile an existing cluster, use `pmcluster cluster update` (content-aware, idempotent).

### "cluster already initialised"
`cluster up` is init-only — it refuses to run when a cluster already exists. Use `pmcluster cluster update` instead.

### "no such stack"
Run `pmcluster stack list` to see deployed stacks. Stack names come from the `app` field in manifests.

### Deploy fails
Check the Swarm service logs: `docker service logs my-app_api --tail=50`. Inspect the translated compose from the latest revision: `pmcluster stack show my-app`.

### Deploy blocked by strict backup failure
If `strict_backup: true` is set and the pre-deploy backup fails, the deploy aborts. Check `pmcluster backup list`. Fix the backup issue or remove `strict_backup: true`.

### Worker node can't pull images
Ensure `pmcluster serve` is running (it replays registry credentials). Verify with `pmcluster registry list`.

### Webhook returns 401
Common causes:
1. **Missing/incorrect `X-Pmcluster-Timestamp`** — required; current unix seconds within ±5 min. Ensure CI clocks are NTP-synced.
2. **Wrong HMAC input** — signature is over `timestamp + body` (no separator), using the timestamp exactly as sent.
3. **Body mutated after signing** — sign the exact bytes you POST (`printf '%s'`, not `echo`; avoid re-serializing JSON).
4. **Wrong webhook secret** — recreate with `pmcluster webhook remove <source> && pmcluster webhook add <source>` and update the CI secret.

### Rate limited (HTTP 429) or banned (HTTP 403)
The edge proxy throttles per real IP (`/api/*` 200/s, `/webhook/*` 40/s). Space out bursts. A 403 means the IP was auto-banned after repeated trips/failures; bans expire (default 10 min).

### Console login fails
Get the password with `pmcluster credentials show edge_admin`. If the `pmui-data` volume was wiped, the console re-reads the Swarm secret on next boot.

### After SSO-related sso-stack/edge redeploy: initial 404/403
After enabling or updating SSO, Traefik label convergence takes ~45–60 seconds. Initial 404/403 errors on `pmcluster.<domain>` or `observ.<domain>` are expected during this window. Wait and retry.

### oauth2-proxy not proxying (SSO enabled but login loop)
Ensure `OAUTH2_PROXY_COOKIE_DOMAINS` and `OAUTH2_PROXY_WHITELIST_DOMAINS` are **plural** (the singular forms are silently ignored in oauth2-proxy v7). Also verify `OAUTH2_PROXY_REVERSE_PROXY: "true"` is set and the edge container has `trusted-proxy-ip` / `TRUSTED_PROXY_CIDRS` configured for hardening.

### Edge image not updating after upgrade
The edge image tag is pinned to the release version (`ghcr.io/hazemarian/pmcluster-edge:<version>`). If the tag doesn't match, override with `PMCLUSTER_EDGE_IMAGE=<tag>`. Always use the `install.sh` → `cluster update` flow rather than `docker service update --image` directly.

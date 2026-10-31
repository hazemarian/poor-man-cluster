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
# Interactive wizard (recommended) — prompts for domain, TLS, admin user, SSO, node hostname, etc.:
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
pmcluster backup restore <id> [--volume <name>] [--from-s3]
pmcluster tls site show
pmcluster cluster settings
pmcluster usage
```

Restore is **local-first**: an archive still on the node restores from disk; when
it is gone the archive is fetched from the configured offsite S3 store
(`backup_s3_*` settings, R2-compatible SigV4) — `--from-s3` forces the fetch.
`--volume <name>` restores a single volume (anchored to the run's stack;
whole-disk runs accept `<app>/<volume>`). With no local archive and no S3
configured the command fails loudly and names where the archive lives.

```bash
pmcluster join --role worker --token <TOKEN> --manager <MANAGER>:2377 \
  --tailscale --tailscale-auth-key tskey-...   # opt-in tailnet advertise-addr
```

The **control loop** (leader daemon) reconciles in the background:
`reconcile_interval` (default 60s, `0` disables) + Swarm events. Each pass
re-deploys drifted platform stacks, syncs app stacks whose rendered hash
drifted, and writes a health snapshot to `stack_status` — the source for the
public **status badges** at `https://pmcluster.<domain>/api/public/badge/<stack>`
(and `/<stack>/services` for the combined per-service badge), which you can
paste into any GitHub README.

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

    placement: manager         # optional — manager | worker | <node hostname> | (empty)
    #   stateful services (with a volume) should pin to ONE specific node
    #   hostname (e.g. "node-01") so their data never has to migrate;
    #   "manager"/"worker" allow any node of that role (unsafe for volumes)
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

- Secrets live **AES-256-GCM encrypted** in `data.db` (key `~/.pmcluster/.encryption_key`). The plaintext is shown **once** at creation; afterwards only the **sha256 hash** is shown. Values can be **revealed on demand** (with confirmation in the UI) and **edited** in place (`PUT /api/secrets/{name}`). `pmcluster secret edit <name> --value <v>` (or piped via stdin) updates the DB row **and mirrors the new value to the Docker Swarm secret** (Swarm secrets are immutable, so the old secret is removed and recreated — containers must be redeployed to mount the new value).
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

### CI workflows — two things that break naive watchers

- **A tag build creates no `docs` CI run.** Only a push triggers the `docs` workflow; pushing a `v*` tag runs `release.yml` alone. A release watcher that waits for a `docs=success` check on a tag will wait forever. Gate release readiness on the tag's own workflow (or the last push to `main`), not on a `docs` run that will never appear.
- **`swarm-e2e` can fail intermittently on an ordering race, not a real fault.** `docker stack deploy` attaches networks **by name** and skips creating one if a same-named network is merely *listed* — so a leftover `backup_net` still being removed asynchronously gets listed, creation is skipped, and the name is gone by the time the service is created. That surfaces as `network backup_net not found`. It is fixed by ensuring per-stack overlays before deploy, but if you see that exact string on an older run, it is the race, not a broken stack.

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

**Registry auth on join:** `pmcluster join` warns when the host has no Docker registry credentials (`~/.docker/config.json` missing or empty) — without them, private images silently fall back to whatever stale cached copy exists. Authenticate before deploying private images: `docker login ghcr.io --username <user>` (a copy of the manager's config is sufficient).

**Image freshness + paused updates in the console:** the services/stacks pages show a `⚠ Image N days old` pill when the node is running a stale cached image (registry auth missing is the usual cause), and an `Update paused` banner with the failing task's error when Swarm pauses a rollout — check the Tasks tab for the failing task.

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

`cluster up` is init-only — it refuses to run on an already-initialised cluster (run `cluster update` instead). `cluster update` is the content-aware reconcile: the DB is the source of truth for all platform configs (there are no `~/.pmcluster/config/*.yml` disk files). It compares `rendered_hash` to decide which platform stacks (observability / infra / edge / backup / +sso) need re-deploying. Drift-prune removes services that were dropped from a compose. It is idempotent.

The summary reports **stack redeploys and config replacements on two separate axes**, and must be read literally:

- `Stacks re-deployed     : [...]` — stacks whose rendered compose moved (or that drifted out of band in the live swarm).
- `Configs replaced       : [...]` — rendered configs replaced **without** a stack redeploy (e.g. the OTel collector config or the observability provisioner), printed only when non-empty.

The no-op line is printed only when **neither** axis moved:

```
  (no stacks redeployed and no configs replaced — nothing to redeploy)
```

A TLS cert/key rotation with no stack or config change is called out separately rather than folded into that sentence (`… only the TLS cert/key secret rotated`). Before v0.2.212 the summary tracked stack redeploys only, so a replaced rendered config was reported as "nothing changed" while the OTel config and the provisioner were in fact swapped. On an older binary, verify a suspected config replacement with `docker service inspect <svc>` and read the mounted config back instead of trusting the summary.

### Upgrading pmcluster / the edge service

Two binaries are involved: the `pmcluster` binary on each node (the control plane) and the version-pinned `pmcluster-edge` image the platform deploys. `cluster update` reconciles the second — it does **not** replace the binary on the nodes, and there is no `upgrade` subcommand.

#### 1. Build + publish the release first

Push a `v*` tag; the release workflow cross-compiles the binaries and pushes the new edge image to GHCR:

```bash
git tag v0.2.213 && git push origin v0.2.213
```

#### 2. Replace the binary on every node — `hack/upgrade-nodes.sh`

```bash
pmcluster/hack/upgrade-nodes.sh <version>
```

Run from a checkout (needs `gh` and `ssh`). It downloads `pmcluster-<version>-linux-amd64.tar.gz` + `SHA256SUMS.txt`, **verifies the checksum**, copies the binary to each node, stages it in `/tmp`, installs it to `/usr/local/bin/pmcluster`, restarts the unit, and prints the version before/after per node. Only once every node agrees does it run `cluster update` on the leader, so the re-render is done by the new version everywhere. The previous binary is kept as `/usr/local/bin/pmcluster.<version>.prev`.

| Env var | Default | Meaning |
|---|---|---|
| `PMCLUSTER_NODES` | *(empty)* | Comma-separated node IPs. **Pass this in practice** — see the discovery caveat below. Selects *which* nodes to touch; the restart order is still derived from the swarm when a seed answers. |
| `PMCLUSTER_SEED` | *(empty)* | One reachable node to discover the swarm from; used only when `PMCLUSTER_NODES` is empty (and as the ordering seed when it is set). |
| `PMCLUSTER_SSH_KEY` | `~/.ssh/pmcluster_vps4_ed25519` | SSH private key. |
| `PMCLUSTER_SSH_USER` | `root` | SSH user for every node. Set to `wafaa` for the wafaa node. |

**Ordering:** workers and follower managers are upgraded **before the leader**, which goes last, so the swarm never loses its only manager mid-upgrade. This is a safety property, not a preference: even with `PMCLUSTER_NODES` set, the script reads each node's role from the swarm and reorders the list — falling back to the stated order (never aborting) only if the swarm cannot be read.

**Failure semantics:** every step that can fail is fatal. If any node's copy or install fails, the script sets `FAILED`, finishes the loop, and exits non-zero (`✖ upgrade incomplete — some nodes are still on the old binary`) — a partial upgrade never passes silently. The copy is deliberately separate from **and strictly before** the install, so a copy failure leaves that node byte-for-byte untouched and prints `copy FAILED … nothing changed on this node`. A half-upgraded cluster is treated as worse than an un-upgraded one.

**The two production targets** (hard-won access facts — do not re-derive them):

- **Production swarm** — nodes `217.160.143.22` (leader), `217.160.143.24`, `217.160.143.26`, and worker `217.160.128.29`; key `~/.ssh/pmcluster_vps4_ed25519`; user `root`.
- **wafaa** — single-node manager at `195.62.50.111`, hostname `idlib-wafaa`; key `~/.ssh/wafaa_m_ed25519`; user **`wafaa`** (not root); **passwordless `sudo -n` required**; sshd has the **SFTP subsystem disabled**; login shell is **zsh**.

```bash
# production swarm (followers-first order; the leader is listed last)
PMCLUSTER_NODES=217.160.143.24,217.160.143.26,217.160.128.29,217.160.143.22 \
  PMCLUSTER_SSH_KEY=~/.ssh/pmcluster_vps4_ed25519 \
  pmcluster/hack/upgrade-nodes.sh v0.2.213

# wafaa (single node, non-root user)
PMCLUSTER_NODES=195.62.50.111 PMCLUSTER_SSH_USER=wafaa \
  PMCLUSTER_SSH_KEY=~/.ssh/wafaa_m_ed25519 \
  pmcluster/hack/upgrade-nodes.sh v0.2.213
```

> **Always pass `PMCLUSTER_NODES`.** Discovery reads each node's `Status.Addr`, which on these clusters is the swarm-**internal** address (`10.7.224.x`) — unreachable from outside. The script discovers them happily and then cannot SSH to a single one (it now prints a warning on stderr saying exactly this). Pass the public IPs explicitly, or set `PMCLUSTER_SEED` to a host from which those internal addresses do resolve.

#### Traps that cost real time — read before debugging a failed upgrade

1. **The user is `wafaa`, not `root`, and `idlib-wafaa` is the hostname, not a username.** wafaa's sshd accepts only the `wafaa` account; it does not accept `root`. Sending the correct key to the wrong user fails as `Permission denied (publickey)` — **indistinguishable from a missing or wrong key**, so the obvious move is to re-check the key path when the real problem is the username.
2. **`scp` needs the SFTP subsystem.** wafaa has it disabled, so a copy over `scp` fails with `scp: Connection closed`. On OpenSSH 9.x that message also looks like an auth refusal or a fail2ban ban — it is neither, and it sends you into `sshd_config` and firewall rules that were never the problem. The script's **primary copy path is `ssh "$host" "cat > /tmp/pmcluster.new" < "$BIN"`** (the exec channel only, so it works wherever `ssh` works); `scp -O` (legacy SCP protocol, also skips SFTP) is a **fallback only**. `scp` is never the primary path.
3. **zsh does not word-split unquoted variables.** wafaa's login shell is zsh, and zsh does not split an unquoted parameter. The `SUDO="sudo -n"; $SUDO install …` indirection therefore tried to exec a command named literally `sudo -n` and died with `zsh: command not found: sudo -n` — so the non-root install and `systemctl restart` never ran. (The same bug sat in the single-quoted `cluster update` one-liner, where the failure was piped into `grep | tail`, exited 0, and silently skipped the reconcile.) Privileged steps are now written out once per branch — `if [ "$(id -u)" -eq 0 ]; then install …; else sudo -n install …; fi` — which cannot depend on any shell's splitting behaviour.

#### 3. Reconcile the platform

```bash
pmcluster cluster update
```

On a **single node**, `install.sh` is the alternative to steps 1–2: it installs the new binary and, because `~/.pmcluster/config.yaml` exists, automatically runs `pmcluster cluster update`:

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.213 bash
# or simply: | bash   (resolves latest release)
```

`cluster update` **re-syncs the platform config templates** from the new binary's embedded copies into the store (the DB is the source of truth; operator edits are preserved), then re-renders. Because the `edge-stack.yml` content changed, the **edge stack is re-deployed** automatically and pulls the new version-pinned image. OTel/Traefik/cert are re-applied content-aware as usual. A second `cluster update` with no changes reports `(no stacks redeployed and no configs replaced — nothing to redeploy)`.

Manual `docker service update --image ... edge_pmcluster-edge` is NOT the supported path — always use the tag → binary upgrade (`hack/upgrade-nodes.sh` on a multi-node swarm, `install.sh` on a single node) → `cluster update` flow. On a fresh box, install.sh runs `cluster up` instead (when `PMCLUSTER_DOMAIN` is set). Set `CLUSTER_APPLY=none` to skip the auto apply.

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
The edge image tag is pinned to the release version (`ghcr.io/hazemarian/pmcluster-edge:<version>`). If the tag doesn't match, override with `PMCLUSTER_EDGE_IMAGE=<tag>`. Always use the release → binary upgrade (`hack/upgrade-nodes.sh`, or `install.sh` on a single node) → `cluster update` flow rather than `docker service update --image` directly.

## Upgrading a multi-node deployment (`hack/upgrade-nodes.sh`)

```bash
cd pmcluster
PMCLUSTER_NODES=<ip>,<ip>,<ip> \
PMCLUSTER_SSH_KEY=$HOME/.ssh/<key> \
bash hack/upgrade-nodes.sh v0.2.218
```

The script downloads the release, verifies its checksum, copies the binary over `ssh … "cat > /tmp/x"`
(SFTP is not required), installs it, restarts the unit, and only then runs `pmcluster cluster update`
on the last node in the list.

### Two traps, both silent

**1. A public IP list defeats the role ordering.** The script sorts your list by the swarm's roles, but
it matches entries against `docker node inspect … {{.Status.Addr}}` — the *internal* address
(`10.7.224.x` here). Public IPs match nothing, so every node is treated as a plain worker and your
stated order is used as-is. "Leader last" is then **your** job: put the leader at the end of
`PMCLUSTER_NODES`. (Same caveat as discovery: internal addresses are right for ordering and wrong as
SSH targets.)

**2. A timeout on port 22 is usually a stale address, not a down host.** Before concluding a node is
unreachable, ask the node itself, through one that is reachable:

```bash
# from a reachable node, jump to the suspect node's INTERNAL address
ssh -i $KEY -o "ProxyCommand=ssh -i $KEY -W %h:%p root@<reachable-node>" root@10.7.224.x '
  hostname; ip -4 -o addr show scope global | awk "{print \$2, \$4}"; ss -ltnp | grep ":22"'
```

Full `ping` loss plus a port-22 timeout, while the swarm reports the node `Ready`, means the swarm is
fine and the address you typed is not the node's current public IP (rebuilt hosts get new IPs from the
provider). Re-run with the address the node reports; do not touch its firewall.

### Recovering a node reachable only on the internal network

Mirror the script's own steps over a jump host — never `docker service update` your way around it:

```bash
gh release download <version> -R hazemarian/poor-man-cluster -p '*linux-amd64.tar.gz' -p 'SHA256SUMS.txt'
shasum -a 256 -c want.txt && tar -xzf pmcluster-<version>-linux-amd64.tar.gz
ssh -o "ProxyCommand=ssh -i $KEY -W %h:%p root@<reachable-node>" root@10.7.224.x \
  'cat > /tmp/pmcluster.new' < pmcluster-<version>-linux-amd64
ssh -o "ProxyCommand=…" root@10.7.224.x '
  cp -a /usr/local/bin/pmcluster /usr/local/bin/pmcluster.<old>.prev
  install -m 0755 /tmp/pmcluster.new /usr/local/bin/pmcluster && rm -f /tmp/pmcluster.new
  systemctl restart pmcluster && sleep 6
  pmcluster version | head -1; systemctl is-active pmcluster'
```

Then run `pmcluster cluster update` on the leader once every node reports the new version — a mixed
cluster is worse than an un-upgraded one.

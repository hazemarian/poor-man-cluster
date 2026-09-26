# Manifest DSL Reference

`pmcluster` deploys applications from a small, strict YAML manifest. The manifest describes *what* you want (app, domain, services, images); pmcluster translates it into the verbose Docker Swarm Compose YAML — injecting Traefik labels, overlay network membership, secret declarations, and restart/update policies.

```
Parse (strict YAML) → Interpolate (${…}) → Validate (semantics) → Translate (Compose v3.9)
```

Deploy with `pmcluster deploy <file>` (local) or push it through a webhook (CI/CD). To inspect the generated Compose, deploy once and open the revision's Rendered manifest in the console or `pmcluster stack show`.

---

## Top-level fields

```yaml
app: donation-campaign        # required — stack name (also ${app})
env: production               # required — environment label (also ${env})
domain: example.com           # required — base domain (also ${domain})
version: latest               # optional — image tag, defaults to "latest" (also ${version})
registry: ghcr.io/acme        # optional — registry prefix (also ${registry})
repo_url: https://github.com/acme/donation-campaign   # optional — metadata only
env_file: .env                # optional — env file path (recorded; substitution is ${env:VAR})

secrets:                      # optional — external Swarm secrets (must already exist)
  - donation_campaign_db_password

backup_before_deploy: true    # optional — snapshot volumes before deploy
strict_backup: true           # optional — abort the deploy if that backup fails

services:                     # required — one or more service definitions
  ...
```

| Field | Required | Rules |
|-------|----------|-------|
| `app` | yes | must match `^[a-z][a-z0-9_-]{0,62}$` (Swarm stack naming) |
| `env` | yes | non-empty (e.g. `production`, `staging`) |
| `domain` | yes | non-empty |
| `services` | yes | at least one service |
| `version` | no | defaults to `latest` |
| `repo_url` | no | metadata only — pmcluster never reads from git |
| `strict_backup` | no | only meaningful with `backup_before_deploy: true` |

---

## Service fields

```yaml
services:
  api:
    image: ${registry}/${app}:${version}   # required
    replicas: 2                            # optional, default 1 (≥ 0)
    run_once: false                        # optional — mutually exclusive with replicas
    placement: manager                     # optional — manager | worker | (empty)
    command: ["./server", "--port", "8080"]
    entrypoint: ["/bin/sh", "-c"]
    env:
      NODE_ENV: production
    volumes:
      - db_data:/var/lib/postgresql/data   # name:path
      - /host/config:/etc/app/config       # /host:/container
    secrets:
      - donation_campaign_db_password
    expose:
      port: 8080
      host: api.${app}.${domain}
      aliases: [api.customer.com]
      cors_disabled: false
    healthcheck:
      type: http
      path: /health
    update:
      parallelism: 1
      delay: 10s
      order: start-first
```

| Field | Default | Notes |
|-------|---------|-------|
| `image` | — | required; supports `${…}` substitution |
| `replicas` | `1` | must be ≥ 0; ignored when `run_once` is true |
| `run_once` | `false` | `true` → `restart_policy: condition: none`; for migrations/jobs |
| `skip_filelog` | `false` | excludes the service from the OTel log-tailing receiver (set when the app ships logs via OTLP itself) |
| `placement` | (any) | `manager` → `node.role == manager`; `worker` → `node.role == worker` |
| `command` | — | overrides the image's `CMD` |
| `entrypoint` | — | overrides the image's `ENTRYPOINT` |
| `env` | — | map of environment variables (values support substitution) |
| `volumes` | — | each entry must contain `:` (`name:path` or `/host:/container`) |
| `secrets` | — | each must also be declared (or referenced) at the top level |
| `expose` | — | presence triggers Traefik wiring + extra network membership |
| `healthcheck` | — | shorthand or full form (see below) |
| `update` | `1` / `10s` / `start-first` | Swarm rolling-update policy (skipped for `run_once`) |

### Referencing DB-backed secrets & configs in `env`

`env` values can reference entries from pmcluster's DB-backed **secrets** and
**configs** stores (managed via `pmcluster secret` / `pmcluster config` or the
operator console) instead of hard-coding values:

```yaml
env:
  ADMIN_PASS: secrets(app_secret)      # inject a stored secret's value
  ADMIN_ENABLED: config(app_config)    # inject a stored config's content
  DEBUG: "false"                       # plain values still work
```

Behavior:

- `secrets(<name>)` — resolves to the secret's **mount path** `/run/secrets/<name>`
  as the env value. The secret is **not** auto-mounted: you must list it in the
  service's `secrets:` array too, or validation fails. Mounting the secret
  explicitly puts its content at that path inside the container.
- `config(<name>)` — resolves the DB config row and injects its content as the
  env value.
- The reference is resolved at deploy time against the DB — rotating the
  secret/config and re-deploying picks up the new value.
- Validation fails at parse time if the pattern is malformed
  (`config(name` / `secrets()`), if a `secrets(name)` env ref is not mounted
  in the service's `secrets:` array, and at deploy time if the named
  secret/config doesn't exist.
- Multi-line content cannot be injected as an env value (it would break the
  compose `environment` block) — use the `secrets:` array to file-mount such
  values instead.
- `secrets(name)` in `env` is convenient, but note that env vars are visible
  via `docker service inspect`; for truly sensitive values prefer the
  `secrets:` array (write-only file mount).

### `expose`

```yaml
expose:
  port: 8080                 # required — container-side port (1..65535)
  host: api.${app}.${domain} # required — primary FQDN
  aliases:                   # optional — extra hostnames → own Traefik routers
    - api.customer.com
  cors_disabled: false       # optional — opt out of the CORS middleware
```

- `host` and each `alias` must look like a hostname (at least one dot).
- Every exposed service automatically joins `traefik-net` (so Traefik can reach it) and `monitoring-net` (so OTel can scrape it).
- `aliases` let a customer's own domain serve the same backend alongside the canonical `${domain}` host. Each alias gets its own Traefik router sharing the same backend.
- When aliases are present, pmcluster emits a **per-app CORS middleware** whose origin regex spans the primary host and every alias (foreign domains aren't covered by the cluster-wide regex).
- `cors_disabled: true` detaches all CORS middleware from the router — use it when the app owns CORS itself or lives on a domain the shared regex can't cover.

### `healthcheck`

**Shorthand** (pmcluster fills in `interval: 10s`, `timeout: 5s`, `retries: 5`):

```yaml
healthcheck:
  type: pg_isready    # CMD-SHELL pg_isready -U $POSTGRES_USER -d $POSTGRES_DB
```

```yaml
healthcheck:
  type: http          # CMD-SHELL wget -q --spider http://127.0.0.1:<port><path>
  path: /health       # default "/"
```

> The HTTP shorthand probes `127.0.0.1` (not `localhost`) to avoid IPv6/IPv4 mismatches inside containers. It uses the service's `expose.port`.

**Full form** (Compose passthrough):

```yaml
healthcheck:
  test: ["CMD", "curl", "-f", "http://localhost/"]
  interval: 30s
  timeout: 5s
  retries: 3
```

Rules:
- Shorthand `type` and explicit `test` are mutually exclusive.
- Full form requires `test` if `interval`/`timeout`/`retries` are set.
- `type` must be `pg_isready`, `http`, or empty.

### `update`

Swarm rolling-update policy. Defaults apply even when `update` is omitted:

```yaml
update:
  parallelism: 1       # default 1
  delay: 10s           # default 10s
  order: start-first   # default; or stop-first
```

---

## Variable substitution

Built-in placeholders, resolved anywhere a value is a string:

| Placeholder | Value |
|-------------|-------|
| `${app}` | `app` |
| `${env}` | `env` |
| `${version}` | `version` (default `latest`) |
| `${registry}` | `registry` |
| `${domain}` | `domain` |
| `${env:VAR}` | OS environment variable `VAR` — **error if unset** |

Substitution is applied to: `domain`, `registry`, `version`, `repo_url`, `env_file`, top-level `secrets`/`volumes`, and each service's `image`, `command`, `entrypoint`, `env` values, `volumes`, `expose.host`, and `expose.aliases`.

Example: `image: ${registry}/${app}:${version}` → `ghcr.io/acme/donation-campaign:latest`.

---

## Strict YAML

Unknown keys are rejected at every level — both top-level and inside a service. A typo like `replica:` instead of `replicas:` fails the deploy rather than being silently ignored. Validation errors are path-prefixed (e.g. `services.api.image: required`).

---

## What the translator injects

You never write these by hand; pmcluster adds them:

- **Networks** — a private per-app overlay `<app>-net` (always) plus `traefik-net` and `monitoring-net` for exposed services.
- **Secrets** — every referenced secret is declared `external: true` at the top level (they must exist in Swarm first).
- **Volumes** — named volumes are auto-collected from service mounts (no top-level declaration) and declared with `driver: local` plus `driver_opts {type: none, o: bind, device: /var/stack/data/<app>/<name>}` so every volume — named or host bind — is forced under the volume root (default `/var/stack/data`, configurable via `volume_root` / `setup --volume-root`). Host binds are relocated to `<root>/<app>/<basename>`. See [`docs/storage-and-databases.md`](storage-and-databases.md).
- **Labels** — `service`, `application`, `environment`, and `version` on every service.
- **Traefik** (exposed services) — router/service names scoped `<app>-<service>`; `entrypoints=websecure`, `tls=true`, load-balancer port, `traefik.docker.network=traefik-net`, and the CORS middleware.
- **Restart policy** — `on-failure` by default; `none` for `run_once`.
- **Deploy** — `replicas`, `placement` constraints, and `update_config` defaults.
- **`io.pmcluster.skip_filelog=true`** — when `skip_filelog: true`.

The output is a `version: "3.9"` Compose file applied with `docker stack deploy`.

---

## Complete example

```yaml
app: donation-campaign
env: production
domain: example.com
registry: ghcr.io/acme
version: latest
repo_url: https://github.com/acme/donation-campaign

backup_before_deploy: true
strict_backup: false

secrets:
  - donation_campaign_db_password

services:
  db:
    image: postgres:14-alpine
    placement: manager
    volumes: [db_data:/var/lib/postgresql/data]
    env:
      POSTGRES_DB: donation_campaign
      POSTGRES_USER: user
    secrets: [donation_campaign_db_password]
    healthcheck: { type: pg_isready }

  migration:
    image: ${registry}/${app}:${version}
    command: ["./migrate"]
    run_once: true

  api:
    image: ${registry}/${app}:${version}
    replicas: 2
    expose:
      port: 8080
      host: api.${app}.${domain}
      aliases: [api.donations.example.org]
    env:
      PORT: "8080"
      OTEL_EXPORTER_OTLP_ENDPOINT: otel-collector:4317
      OTEL_SERVICE_NAME: donation-campaign-api
    healthcheck: { type: http, path: /health }
    update: { parallelism: 1, delay: 10s, order: start-first }
```

Deploy it:

```bash
pmcluster deploy ./donation-campaign.yaml             # apply
pmcluster deploy ./donation-campaign.yaml --version v1.2.3   # override version
```

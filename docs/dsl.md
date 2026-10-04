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
| `platform` | no | **reserved for pmcluster's own platform stacks** (`infra`/`edge`/`observability`/`backup`/`sso`) — stamps `io.pmcluster.platform=true` on every service. User deploys are refused this flag. |
| `networks` | no | additional **external** Swarm networks every service joins (alongside the auto-injected per-stack overlay). When set, the private overlay net is skipped entirely — services reach each other by fully-qualified DNS on the named networks. |
| `volumes` | no | map of top-level named volumes declared verbatim (`name: ""` for a plain volume, or `name: /host/path` for a bind) — **never** relocated under the volume root. Used by platform stacks whose volumes already exist on hosts (`openobserve_data`, `traefik_acme`, `pmui-data`). |

---

## Service fields

```yaml
services:
  api:
    image: ${registry}/${app}:${version}   # required
    replicas: 2                            # optional, default 1 (≥ 0)
    run_once: false                        # optional — mutually exclusive with replicas
    placement: node-01                     # optional — manager | worker | <node hostname> | (empty)
    #   stateful services (with a volume) should pin to ONE specific node
    #   hostname so their data never has to migrate; "manager"/"worker"
    #   allow any node of that role (unsafe for volumes)
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
| `run_once` | `false` | `true` → one-shot job (`restart_policy: none`, or `on-failure` max 3 when `depends_on` is set); for migrations/jobs |
| `skip_filelog` | `false` | excludes the service from the OTel log-tailing receiver (set when the app ships logs via OTLP itself) |
| `placement` | (any) | `manager` → `node.role == manager`; `worker` → `node.role == worker`; **any other value → `node.hostname == <value>`** (pin a stateful service to one specific node so its volume-backed data never has to migrate); empty + a volume mount → auto-pinned to a storage node (round-robin across `storage_nodes`, `platform_node` fallback) |
| `command` | — | overrides the image's `CMD` |
| `entrypoint` | — | overrides the image's `ENTRYPOINT` |
| `env` | — | map of environment variables (values support substitution) |
| `volumes` | — | each entry must contain `:` (`name:path` or `/host:/container`) |
| `binds` | — | raw host binds emitted verbatim (`/host:/container[:ro]`), never relocated under the volume root — for sockets, device paths, host dirs |
| `secrets` | — | each must also be declared (or referenced) at the top level |
| `mode` | — | `global` → Swarm global mode (one task per node); mutually exclusive with `replicas` and `run_once` |
| `restart` | — | overrides `restart_policy.condition`: `any`, `on-failure`, `none` |
| `restart_delay` | — | `restart_policy.delay` (e.g. `5s`) |
| `constraints` | — | raw Swarm placement constraints appended after the `placement`/auto-pin rule (e.g. `- node.labels.pmcluster.storage == true`) |
| `ports` | — | published ports: `target` (container, required), `published`, `protocol` (`tcp`/`udp`), `mode` (`ingress`/`host`) |
| `configs` | — | Swarm config mounts: `source` (DB config name) + `target` (container path) |
| `extra_hosts` | — | `host:ip` entries added to `/etc/hosts` (e.g. `host.docker.internal:host-gateway`) |
| `resources` | — | `reservations`/`limits` with `cpus` + `memory` (e.g. `128M`, `1G`) |
| `user` | — | container user (`0:0`) |
| `labels` | — | raw Swarm service labels (standard auto-injected labels always win on collision) |
| `logging` | — | `driver` + `options` map (e.g. `json-file` with `max-size`/`max-file`) |
| `expose` | — | presence triggers Traefik wiring + extra network membership |
| `healthcheck` | — | shorthand or full form (see below) |
| `update` | `1` / `10s` / `start-first` | Swarm rolling-update policy (skipped for `run_once`); volume-holding services automatically use `stop-first` |

### Referencing DB-backed secrets & configs in `env`

`env` values can reference entries from pmcluster's DB-backed **secrets** and
**configs** stores (managed via `pmcluster secret` / `pmcluster config` or the
operator console) instead of hard-coding values:

```yaml
env:
  ADMIN_PASS: secrets(app_secret)      # inject a stored secret's value
  ADMIN_ENABLED: config(app_config)    # inject a stored config's content
  STORAGE_ROOT: settings(volume_root)  # inject a cluster setting's value
  DEBUG: "false"                       # plain values still work
```

Behavior:

- `secrets(<name>)` — resolves to the secret's **mount path** `/run/secrets/<name>`
  as the env value. The secret is **not** auto-mounted: you must list it in the
  service's `secrets:` array too, or validation fails. Mounting the secret
  explicitly puts its content at that path inside the container.
- `config(<name>)` — resolves the DB config row and injects its content as the
  env value.
- `settings(<name>)` — resolves a cluster setting (e.g. `volume_root`) as the
  env value. Resolved at render time against the cluster store.
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

Services that mount **volumes** are treated as stateful: pmcluster automatically uses `order: stop-first` (a start-first rollout races the old container's shutdown against the new start — e.g. old Postgres deletes the freshly written `postmaster.pid` and the replacement immediately shuts down) and, when `placement` is empty, pins them to a **storage node**: a round-robin pick across the `storage_nodes` cluster setting (main node by default — see [storage-and-databases.md](storage-and-databases.md)), with `platform_node` as the single-node fallback. Each deployed service carries an `io.pmcluster.node` label naming its pinned node (visible in the console Services table and `pmcluster service ps`). No manifest change needed.

---

### `depends_on`

Optional list of services that must be running before this one starts (a `migration` before `db`, for example):

```yaml
migration:
  image: ${registry}/app:${version}
  command: ["./migrate"]
  run_once: true
  depends_on:
    - db
```

**How it works on Swarm:** `docker stack deploy` parses `depends_on` and **ignores it** — the Swarm scheduler has no dependency graph, so every service is scheduled independently. The compose map/condition form (`db: {condition: service_healthy}`) is rejected outright ("must be a list"); only the list form is accepted. pmcluster therefore enforces the ordering **in the control plane**, not in a rendered artifact:

- The deploy pipeline topologically sorts the stack's services into `depends_on` levels and deploys **level by level**: each level's services get their own compose rendered from the same intermediate representation, deployed via `docker stack deploy`, and waited for (long-running services until replicas ≥ desired; `run_once` jobs until their task completes) before the next level starts.
- One drift-prune pass runs at the end with the full stack compose (partial deploys never prune, or they would remove not-yet-deployed sibling services).
- `rendered_yaml` (stored whole-stack, hash-compared for sync no-ops) is an audit/display artifact only — it is never executed directly.
- A `run_once` service with `depends_on` additionally gets `restart_policy: on-failure` with `max_attempts: 3`, so a transient failure retries instead of dying permanently.

Because ordering lives in the deploy step, `depends_on` works for **any** image — no shell wrapper is injected, so baked-entrypoint images (Postgres, MySQL, …), distroless images without `sh`, and images with no declared `command:` all behave identically. A `depends_on` cycle or a reference to a service that does not exist in the manifest fails the deploy loudly before anything is created.

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

- **Networks** — a private per-app overlay declared as `net` (Docker Swarm prefixes it with the stack name, so the deployed network is `<app>_net` — never `<app>_<app>-net`, which would repeat the app name and push long names over Docker's 63-char limit), plus `traefik-net` and `monitoring-net` for exposed services.
- **Secrets** — every referenced secret is declared `external: true` at the top level (they must exist in Swarm first).
- **Volumes** — named volumes are auto-collected from service mounts (no top-level declaration) and declared with `driver: local` plus `driver_opts {type: none, o: bind, device: /var/stack/data/<app>/<name>}` so every volume — named or host bind — is forced under the volume root (default `/var/stack/data`, configurable via `volume_root` / `setup --volume-root`). Host binds are relocated to `<root>/<app>/<basename>`. See [`docs/storage-and-databases.md`](storage-and-databases.md).
- **Labels** — `service`, `application`, `environment`, and `version` on every service.
- **Traefik** (exposed services) — router/service names scoped `<app>-<service>`; `entrypoints=websecure`, `tls=true`, load-balancer port, `traefik.docker.network=traefik-net`, and the CORS middleware.
- **Restart policy** — `on-failure` by default; `none` for `run_once` (a `run_once` service with `depends_on` gets `on-failure` with `max_attempts: 3`).
- **Deploy** — `replicas`, `placement` constraints (stateful services auto-pin to the platform node when `placement` is empty), and `update_config` defaults (`stop-first` for volume-holding services).
- **`depends_on` list** — emitted for compose parity; startup ordering is enforced by the control plane (see [`depends_on`](#depends_on)) — no wrapper is injected.
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
    placement: node-01        # stateful → pin to ONE specific node (not manager/worker)
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

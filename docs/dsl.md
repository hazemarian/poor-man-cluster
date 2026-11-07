# Manifest DSL Reference

`pmcluster` deploys applications from a small, strict YAML manifest. The manifest describes *what* you want (app, domain, services, images); pmcluster translates it into the verbose Docker Swarm Compose YAML — injecting Traefik labels, overlay network membership, secret declarations, and restart/update policies.

```
Parse (strict YAML) → Interpolate (${…}) → Validate (semantics) → BuildIR (neutral IR) → ComposeWriter (Compose v3.9)
```

This is the **single render pipeline** — pmcluster's own platform stacks (`infra` / `edge` / `observability` / `backup` / `sso`) are DSL manifests with `platform: true` that flow through exactly these stages (after a Go-template pre-pass); user manifests share the same translator, writer and deploy path.

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
log_level: warn               # optional — LOG_LEVEL for every service (debug|info|warn|error)

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
| `log_level` | no | `debug`, `info`, `warn` or `error` (`warning` is accepted as an alias of `warn`, and the rendered value is always the canonical `warn`). Any other value fails validation with the allowed set named. This is the level every service in the stack receives as `LOG_LEVEL`; when omitted, the cluster's `app_log_level` setting applies, then `warn`. A service that declares `LOG_LEVEL` in its own `env:` keeps that value — see [What the translator injects](#what-the-translator-injects). |
| `strict_backup` | no | only meaningful with `backup_before_deploy: true` |
| `platform` | no | **reserved for pmcluster's own platform stacks** (`infra`/`edge`/`observability`/`backup`/`sso`) — stamps `io.pmcluster.platform=true` on every service, excludes the stack from the app drift loop, makes it non-deletable via the API, and is only removed by `cluster down --purge`. User deploys (`pmcluster deploy`, `POST /api/stacks`, webhooks) are **refused** this flag: `platform: true is reserved for pmcluster-managed platform stacks`. |
| `networks` | no | additional **external** Swarm networks every service joins. When set, the auto-injected private overlay net is **skipped** (not created) — services reach each other by fully-qualified DNS on the named networks. |
| `volumes` | no | map of top-level named volumes declared verbatim (`name: ""` for a plain local volume, or `name: /host/path` to bind that host path) — **never** relocated under the volume root. Used by platform stacks whose volumes already exist on hosts (`seaweeddata`, `traefik_acme`, `openobserve_data`). |

---

## Service fields

```yaml
services:
  api:
    image: ${registry}/${app}:${version}   # required
    replicas: 2                            # optional, default 1 (≥ 0)
    run_once: false                        # optional — mutually exclusive with replicas
    placement: node-01                     # optional — manager | worker | <node hostname> | (empty)
    #   a stateful service (with a volume) is auto-pinned to a storage node;
    #   set a specific hostname here to override; "manager"/"worker" allow
    #   any node of that role (unsafe for volumes)
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
| `replicas` | `1` | must be ≥ 0; mutually exclusive with `run_once` and `mode: global`. An explicit `replicas: 0` (scale to zero) is rendered verbatim, not collapsed to the default |
| `run_once` | `false` | `true` → one-shot job (`restart_policy: none`, or `on-failure` max 3 when `depends_on` is set); for migrations/jobs |
| `skip_filelog` | `false` | excludes the service from the OTel log-tailing receiver (set when the app ships logs via OTLP itself) |
| `placement` | (any) | `manager` → `node.role == manager`; `worker` → `node.role == worker`; **any other value → `node.hostname == <value>`** (pin a stateful service to one specific node so its volume-backed data never has to migrate); empty + a volume mount → auto-pinned to a storage node (per-stack `stack move` pin, then a deterministic `storage_nodes` round-robin) |
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
| `ports` | — | published ports: `target` (container, required), `published`, `mode` (`ingress`/`host`). All publishing is TCP (`protocol` was removed — no service needs UDP). For single-port services prefer `expose.external` |
| `configs` | — | Swarm config file mounts: each entry is `config_path(<name>)`. The platform resolves the path — `<config_mount_root>/<name>.yaml`, default `/etc/config/app.yaml`, `.yaml` appended only when the name has no extension — and the rendered compose references the **content-addressed** Docker config (`<name>_<sha8>`) automatically. Same syntax in app and platform DSL |
| `extra_hosts` | — | `host:ip` entries added to `/etc/hosts` (e.g. `host.docker.internal:host-gateway`) |
| `resources` | — | `reservations`/`limits` with `cpus` + `memory` (e.g. `128M`, `1G`) |
| `user` | — | container user (`0:0`) |
| `labels` | — | raw Swarm service labels (standard auto-injected labels always win on collision) |
| `logging` | — | `driver` + `options` map (e.g. `json-file` with `max-size`/`max-file`) |
| `networks` | — | per-service **external** Swarm networks joined in addition to the app-level `networks` (merged) — used when one stack splits membership (e.g. OpenObserve joins `traefik-net` + `monitoring-net` while the OTel collector joins only `monitoring-net`) |
| `expose` | — | Traefik routing (see below). Add `external: <port>` to also publish the port to the swarm ingress as a real host port (TCP) — reachable directly, not only through Traefik |
| `healthcheck` | — | shorthand or full form (see below) |
| `update` | `1` / `10s` / `start-first` | Swarm rolling-update policy (skipped for `run_once`); volume-holding services automatically use `stop-first` |

### Referencing DB-backed secrets & configs in `env`

`env` values may reference entries from pmcluster's DB-backed **secrets** and
**configs** stores (managed with `pmcluster secret` / `pmcluster config` or the
operator console) instead of hard-coding values. There are exactly four tokens —
two families, each with a value form and a location form:

| token | resolves to | stored in |
|---|---|---|
| `config(name)` | the config's **content** | `configs` |
| `config_path(name)` | the file **path** the config is mounted at | `configs` |
| `secret(name)` | the secret's **value**, decrypted | `secrets` |
| `secret_path(name)` | the file **path** the secret is mounted at, `/run/secrets/<name>` | `secrets` |

Singular is a **value**, `_path` is a **location**. When in doubt reach for the
location form: a path in an env var is safe to print, safe to log, and safe to
leak into `docker service inspect`, while a value is none of those.

```yaml
services:
  web:
    secrets:
      - db_password                     # the mount, without which the path is fiction
    configs:
      - config_path(app_settings)
    env:
      DATABASE_URL_FILE: secret_path(db_password)   # /run/secrets/db_password
      SETTINGS_FILE: config_path(app_settings)      # /etc/config/app_settings.yaml
      ADMIN_ENABLED: config(app_settings)           # content inline (single line only)
      ZO_ROOT_USER_PASSWORD: secret(oo_admin_password)  # value inline
      DEBUG: "false"                                # plain values still work
```

Enforced rules, not advice:

- `secret_path(name)` requires `secrets: [name]`; `config_path(name)` requires
  `configs: [name]`. Validation rejects the reference and names the array entry to
  add — a path is never handed to an app for a file nobody mounted.
- `secret(name)` resolves within the owning stack only. A service-scope secret
  belonging to another stack is refused, with the owning stack named and the value
  never echoed. Cluster-scope (stack-less) secrets are shared and always
  resolvable.
- Multi-line content cannot be injected as an env value — it would break the
  compose `environment` block. Use the mount form.
- An unrecognised `x(y)` value is **rejected as a malformed reference**, never
  written through verbatim. Both `settings(x)` (removed in v0.2.224) and
  `secrets(x)` (removed in v0.2.225) fail this way: without that check the string
  `secrets(db_pass)` would land in the container environment and "work" as a
  literal — a plausible, wrong value instead of an error.
- Cluster settings are **not** addressable from a manifest. The `cluster_settings`
  table (`domain`, `reconcile_interval`, `storage_nodes`, `backup_*`,
  `config_mount_root`) belongs to the platform; manifests name resources.

### Mounting configs and secrets as files

The platform owns the mount path; the manifest only names the resource.

```yaml
services:
  web:
    configs:
      - config_path(app_settings)     # → /etc/config/app_settings.yaml
    secrets:
      - db_password                   # → /run/secrets/db_password
```

- **Configs** land under the cluster setting `config_mount_root` (default
  `/etc/config`), named `<name>` with `.yaml` appended when the name carries no
  extension — `config_path(nginx.conf)` keeps its own name. Every service that
  mounts at least one config gets `CONFIG_PATH=<mount root>` injected
  automatically (the Kubernetes ConfigMap directory convention), overridable in
  the manifest.
- **Secrets** land at `/run/secrets/<name>`, root-owned, read-only, on a tmpfs.
- The rendered compose references the **content-addressed** Docker object
  (`<name>_<sha8>` for both configs and secrets), so editing a config never
  mutates a running container: the next deploy swaps the object, and rotating a
  secret changes the rendered hash so reconcile redeploys on its own.
- Config content may itself reference `config()`, `config_path()`, `secret()` and
  `secret_path()`; a cross-stack reference is refused naming the owner, and
  config→config cycles are depth-limited.
- Config file permissions are `0444` root-owned on a read-only mount; secret files
  are `0400` root-owned. Neither is world-readable.

### `expose`

```yaml
expose:
  port: 8080                 # required — container-side port (1..65535)
  host: api.${app}.${domain} # FQDN — required UNLESS external is set (raw publish)
  external: 80               # optional — also publish the port to the swarm ingress
                             # (target=port, published=external, TCP). Empty = only
                             # reachable through Traefik.
  mode: ingress              # optional — ingress (default) or host
                             # (host → one port per node, e.g. node-local exporters)
  aliases:                   # optional — extra hostnames → own Traefik routers
    - api.customer.com
  cors_disabled: false       # optional — opt out of the CORS middleware
```

- `host` and each `alias` must look like a hostname (at least one dot). `host` is
  only required when `external` is not set: `expose` with `external` but no `host`
  publishes the raw swarm port with **no** Traefik router and no routing-network
  membership (this is how the platform stack publishes the OTel collector's
  node-local OTLP endpoint, and how Traefik itself binds 80/443).
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

> The HTTP shorthand probes `127.0.0.1` (not `localhost`) to avoid IPv6/IPv4 mismatches inside containers. It uses the service's `expose.port`, falling back to the first published `ports` target.

**Full form** (Compose passthrough):

```yaml
healthcheck:
  test: ["CMD", "curl", "-f", "http://localhost/"]
  interval: 30s
  timeout: 5s
  retries: 3
  start_period: 20s    # optional grace period before failures count
```

Rules:
- Shorthand `type` and explicit `test` are mutually exclusive.
- Full form requires `test` if `interval`/`timeout`/`retries` are set.
- `type` must be `pg_isready`, `http`, or empty.
- `start_period` (optional, both forms) is a Docker duration string (e.g. `20s`).
- An `http` shorthand with no derivable port (no `expose.port`, no published `ports`) is **omitted with a warning** rather than shipping a port-0 probe.

### `update`

Swarm rolling-update policy. Defaults apply even when `update` is omitted:

```yaml
update:
  parallelism: 1       # default 1
  delay: 10s           # default 10s
  order: start-first   # default; or stop-first
```

Services that mount **volumes** are treated as stateful: pmcluster automatically uses `order: stop-first` (a start-first rollout races the old container's shutdown against the new start — e.g. old Postgres deletes the freshly written `postmaster.pid` and the replacement immediately shuts down) and, when `placement` is empty, pins them to a **storage node**. The pin resolution order is: the per-stack pin written by `pmcluster stack move <stack> --to <node>` (the `stack_pin_<stack>` setting — outranks everything) → a deterministic round-robin across the `storage_nodes` cluster setting (FNV-1a, 32-bit, over the stack name; the leader is auto-added, and a worker is never a candidate: storage nodes are managers) — and with no storage nodes nothing is pinned, so any node will do. Each deployed service carries an `io.pmcluster.node` label naming its pinned node (visible in the console Services table and `pmcluster service ps`). No manifest change needed.

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

Substitution is applied to: `domain`, `registry`, `version`, `repo_url`, `env_file`, the top-level `secrets` list, and each service's `image`, `command`, `entrypoint`, `env` values, `volumes`, `binds`, `extra_hosts`, `configs`, `labels` values, `logging.driver`/`logging.options`, `user`, `restart_delay`, `healthcheck.start_period`, `expose.host`, and `expose.aliases`. (The top-level `volumes:` map is declared verbatim and is **not** interpolated.)

Example: `image: ${registry}/${app}:${version}` → `ghcr.io/acme/donation-campaign:latest`.

---

## Strict YAML

Unknown keys are rejected at every level — both top-level and inside a service. A typo like `replica:` instead of `replicas:` fails the deploy rather than being silently ignored. Validation errors are path-prefixed (e.g. `services.api.image: required`).

---

## What the translator injects

You never write these by hand; pmcluster adds them:

- **Networks** — a private per-app overlay declared as `net` (Docker Swarm prefixes it with the stack name, so the deployed network is `<app>_net` — never `<app>_<app>-net`, which would repeat the app name and push long names over Docker's 63-char limit), plus `traefik-net` and `monitoring-net` for exposed services. Setting the app-level `networks` **replaces** the private overlay — the services join the named external networks and reach each other by fully-qualified DNS instead.
- **Secrets** — every referenced secret is declared `external: true` at the top level (they must exist in Swarm first). Secrets that live in pmcluster's DB store are referenced by their **content-addressed** Swarm name `<name>_<sha256-first-8-hex>` — the container mount path stays `/run/secrets/<name>`, but a rotation mints a new Swarm object instead of touching the immutable in-use one (see [storage-and-databases.md](storage-and-databases.md)).
- **Volumes** — named volumes are auto-collected from service mounts (no top-level declaration) and declared with `driver: local` plus `driver_opts {type: none, o: bind, device: /var/stack/data/<app>/<name>}` so every volume — named or host bind — is forced under the volume root (default `/var/stack/data`, configurable via `volume_root` / `setup --volume-root`). Host binds are relocated to `<root>/<app>/<basename>`. See [`docs/storage-and-databases.md`](storage-and-databases.md). Volumes declared in the top-level `volumes:` map are the exception — they pass through verbatim and are never relocated.
- **Labels** — `service`, `application`, `environment`, and `version` on every service; `io.pmcluster.platform=true` on every service of a `platform: true` stack; `io.pmcluster.node=<hostname>` on services with a resolved node pin; `io.pmcluster.skip_filelog=true` when `skip_filelog: true`; and `io.pmcluster.rendered_hash=<sha256>` — the content hash of the whole-stack label-free render, stamped on every service **after a successful apply** so drift detection can compare the live Swarm against a fresh render (a failed deploy leaves the hash unstamped, forcing the next sync to retry).
- **Traefik** (exposed services) — router/service names scoped `<app>-<service>`; `entrypoints=websecure`, `tls=true`, load-balancer port, `traefik.docker.network=traefik-net`, the `tls.certresolver=letsencrypt` label on ACME clusters (never on BYO-cert clusters), and the CORS middleware.
- **Restart policy** — `any` by default (a long-running service whose container exits cleanly — e.g. a SIGTERM during a Docker upgrade or a graceful shutdown — is marked *Complete* by Swarm and, under `on-failure`, is **never** replaced, leaving it at 0/1 until an operator force-updates it; `any` restarts it instead); `none` for `run_once` (a `run_once` service with `depends_on` gets `on-failure` with `max_attempts: 3`).
- **Deploy** — `replicas` (an explicit `replicas: 0` — scale to zero — is rendered verbatim, not collapsed to the default of 1), `mode: global`, `placement` constraints (the storage-pin order above), and `update_config` defaults (`stop-first` for volume-holding services).
- **`depends_on` list** — emitted for compose parity; startup ordering is enforced by the control plane (see [`depends_on`](#depends_on)) — no wrapper is injected.
- **`LOG_LEVEL`** — every service of an app stack gets `LOG_LEVEL` in its environment, so an app that logs at INFO by default is quietened without editing each service. The value is, in order: the manifest's `log_level`, the cluster's `app_log_level` setting, then `warn`. **An explicit `LOG_LEVEL` in the service's own `env:` wins and is never overwritten** — the platform supplies a default, not a policy. The var is written into the IR's env and rendered with the rest of `environment:`, so ordering and `$`-escaping stay with the writer; the manifest YAML is never string-patched. Platform stacks (`platform: true`) are not injected — their components (Traefik, Parseable, SeaweedFS, the OTel collector) set verbosity with their own flags.
- **`$`-escaping** — every user-derived string value (env, command, entrypoint, labels, volumes, healthcheck paths, CORS regexes) is written with literal `$` doubled to `$$`, so `docker stack deploy`'s compose interpolation cannot corrupt a literal `$` (a secret value `p@ss$word` reaches the container intact).

The output is a `version: "3.9"` Compose file applied with `docker stack deploy`.

> **Async applies.** Deploys through the HTTP API and webhooks validate, record the revision and return `202 Accepted` immediately; the swarm apply (with its per-level health waits) continues in the background — failures surface on the stack's `last_error`, the webhook delivery row and the deploy telemetry. The CLI `pmcluster deploy` waits synchronously.

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
    placement: node-01        # stateful → auto-pinned; a hostname here overrides (not manager/worker)
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
      # the collector publishes node-local OTLP (host mode :4318) and serves
      # gRPC :4317 / HTTP :4318 on monitoring-net as observability_otel-collector:
      OTEL_EXPORTER_OTLP_ENDPOINT: http://observability_otel-collector:4318
      OTEL_SERVICE_NAME: donation-campaign-api
    healthcheck: { type: http, path: /health }
    update: { parallelism: 1, delay: 10s, order: start-first }
```

Deploy it:

```bash
pmcluster deploy ./donation-campaign.yaml             # apply
pmcluster deploy ./donation-campaign.yaml --version v1.2.3   # override version
```

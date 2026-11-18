<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim. docs/dsl.md is the canonical manifest reference. -->

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

backup_before_deploy: true     # optional — take a snapshot backup before deploy
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
  ADMIN_PASS: secret_path(my_app_db_password)  # stored secret → mount path /run/secrets/...
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
- Validation fails on malformed references, on `secret_path(name)` env refs that aren't mounted in the service `secrets:` array, and at deploy time if the named secret/config doesn't exist. `settings()` and the plural `secrets()` were removed (v0.2.224/v0.2.225) and now fail as malformed. Multi-line content can't be injected into env — file-mount it via the `secrets:` array instead.

### Validation Rules

- Strict YAML — unknown keys are rejected at every level.
- `app` must match `^[a-z][a-z0-9_-]{0,62}$`; `env`, `domain`, `services` are required.
- `image` is required per service.
- `replicas` and `run_once` are mutually exclusive.
- `expose` requires `port` (1..65535) and `host`.
- `healthcheck.type` must be `http`, `pg_isready`, or empty (full form).
- `update.order` must be `start-first` or `stop-first`.

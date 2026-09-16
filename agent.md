# agent.md — Poor Man's Stack (pmcluster)

This document is the authoritative map of the codebase. An agent should be able
to work on any part of the system after reading this file, without reading the
source first. It covers: what the system is, the frameworks in use, the
architecture, the repository layout, each package's responsibility, the key
runtime flows, where to change things for common tasks, and the verification
loop.

---

## 1. What this is

Poor Man's Stack is a self-hosted, single-node-friendly deployment platform
built on **Docker Swarm**. A single Go binary called `pmcluster` is the control
plane: it bootstraps a Swarm cluster, provisions the infrastructure services
(Traefik, Portainer, OpenObserve, OTel collector, backups), accepts application
deployments through a YAML DSL, and manages TLS, secrets, configs, API keys,
webhooks and backups — via a CLI, a REST API, and an operator console.

A second Go binary, `pmcluster-edge`, runs as a Swarm service and is the public
origin for the whole platform: it is both an **operator console** (gin + HTMX)
and a **smart reverse proxy** (rate limits, in-flight shield, auto-ban) that
forwards the daemon's API and webhooks.

Production reference: manager node `root@82.165.128.237` (Docker Swarm, 2
nodes), domain `nextrum-sy.com`, console at `https://pmcluster.nextrum-sy.com/`.

---

## 2. Frameworks and key libraries

| Area | Choice | Notes |
|---|---|---|
| Language | Go 1.25 | module `github.com/hazemarian/poor-man-stack/pmcluster` (root: `pmcluster/` dir) |
| CLI | `spf13/cobra` | all `pmcluster` subcommands; `cobra.Command` tree built in `init()` funcs |
| Config | `spf13/viper` | global `--config` flag → `config.Config` |
| Daemon HTTP API | `go-chi/chi/v5` | REST router in `internal/server`; JSON via `writeJSON`/`writeErr` |
| Edge console + proxy | `gin-gonic/gin` | `cmd/edge/main.go`; console routes + `NoRoute` proxy |
| Edge proxy internals | `go-chi/chi/v5` + `net/http/httputil.ReverseProxy` | `internal/edgeproxy` |
| Console server-side UI | gin + HTMX (hx-* attributes) + Go `html/template` fragments | `internal/ui` |
| DB | SQLite via `modernc.org/sqlite` (pure Go, no CGO) | daemon's `~/.pmcluster/data.db` |
| Secrets at rest | AES-256-GCM via `golang.org/x/crypto` | `internal/credentials` (key: `~/.pmcluster/.encryption_key`) |
| Password hashing | `argon2id` (daemon users) + `bcrypt` (console users) | `golang.org/x/crypto` |
| Rate limiting | `golang.org/x/time/rate` | `internal/edgeproxy/ratelimit.go` |
| Logging | `rs/zerolog` | `internal/logger` |
| Observability | OpenTelemetry SDK (metrics/traces) | `internal/telemetry`, OTLP to the in-cluster OTel collector |
| Container control | Docker SDK `github.com/docker/docker/client` | `internal/docker` (swarm stacks/secrets/configs) |
| YAML | `sigs.k8s.io/yaml` (strict) + `gopkg.in/yaml.v3`-style round-trips | DSL + Traefik dynamic config |
| Tests | stdlib `testing` + `httptest` | see §10 |

---

## 3. Architecture overview

```
                 Internet
                    │  :443
                    ▼
             Traefik (HTTPS ingress)   infra_traefik
                    │
        ┌───────────┼───────────────────────┐
        ▼           ▼                       ▼
  App stacks   pmcluster-edge (:8042)   Portainer (infra)
  (deployed    ┌────────────────────┐     (read/operate UI)
   apps)       │ operator console   │
               │  (gin + HTMX)      │
               │ smart reverse proxy│
               │  rate-limit/shield │
               └────────┬───────────┘
                        │ host.docker.internal:9090
                        ▼
               pmcluster daemon (host binary, NOT a container)
               REST API /api/* + POST /webhook/<source>
               └── SQLite ~/.pmcluster/data.db
               └── docker SDK → Swarm services/secrets/configs
               └── AES-GCM key ~/.pmcluster/.encryption_key

    Observability: apps → OTLP :4317 → otel-collector (global) → OpenObserve
    Backups:       offen/docker-volume-backup agents (volumes + control plane)
```

- **Two overlay networks**: `traefik-net` (app traffic via Traefik) and
  `monitoring-net` (telemetry). Apps that `expose` get both; every app also
  gets a private `<app>-net` overlay.
- **Traefik** is the single HTTPS entry point. Its dynamic config
  (`pmcluster_traefik_dynamic_vNNN` Docker config) holds middlewares
  (`admin-auth`, `cors-default`) and the TLS certificates list.
- **pmcluster daemon** is a host process (`systemd` on Linux, `brew services`
  on macOS) listening on `127.0.0.1:9090` (configurable via `listen_addr`).
  The edge reaches it through `host.docker.internal:host-gateway`.
- **Certificates**: cluster's own cert + each per-host (bring-your-own) cert
  are stored as **versioned Swarm secrets** (`cert_vNNN/key_vNNN`,
  `hostcert-<host>_vNNN/hostkey-<host>_vNNN`) with metadata rows in the
  `site_certs` table. Nothing is read from the manager's filesystem except the
  cluster cert's local copy under `~/.pmcluster/config/site/`.

---

## 4. Repository layout

```
repo root (this dir)
├── README.md                  # user-facing docs
├── docs/
│   ├── webhook.md             # CI webhook integration guide
│   └── dsl.md                 # DSL reference
├── install.sh                 # curl|bash installer (binary + systemd + auto cluster up/update)
├── skills/poor-man-stack-deploy/SKILL.md   # deployment skill for agents
├── .github/workflows/
│   ├── pmcluster.yml          # build gate: lint + build + unit + fast e2e
│   ├── swarm-e2e.yml          # informational real-swarm e2e (never blocks)
│   └── release.yml            # on v* tags: binaries + edge image (GHCR)
└── pmcluster/                 # THE GO MODULE
    ├── cmd/pmcluster/main.go  # daemon binary entrypoint
    ├── cmd/edge/main.go       # edge console+proxy binary entrypoint
    ├── migrations/            # SQL migrations 0001..0009 (schema_version-tracked)
    ├── e2e/                   # swarm e2e tests (//go:build e2e)
    ├── docs/openapi.yaml      # REST API spec
    └── internal/              # all packages (§5)
```

---

## 5. Package-by-package map (`pmcluster/internal/`)

### cmd/pmcluster + internal/cli — the CLI surface
`cobra.Command` tree. Every command registers itself in its file's `init()`.
Key commands (all in `internal/cli/`):
- `cluster up|status|down|update` (`cluster.go`) — bootstrap/apply/teardown.
- `deploy <manifest.yaml>` (`deploy.go`) — the DSL deploy entry point.
- `stack list|show`, `rollback`, `logs`, `backup`, `node`, `registry`,
  `credentials`, `user`, `webhook`, `api-key`, `tls`, `secret`, `config`,
  `serve`, `version`, `completion`, `init`.
- The `serve` command (`serve.go`) wires the daemon: opens store, docker
  client, deploy service, and calls `server.New` — **this is where new daemon
  dependencies are injected**.
- `config.Config` (from `internal/config`) carries `DBPath`, `ConfigDir`,
  `EncryptionKeyPath`, `ListenAddr`, `Domain`.

### internal/api
Small HTTP handlers shared by the daemon (`/health`), plus the deploy payload
types used by API/webhook/CLI.

### internal/server — the daemon REST API (chi)
Bearer-authenticated `/api/*` router built in `New(Deps)`.
`Deps` (in `server.go`) holds: `Lookup` (auth), `Docker`, `Store`,
`DeployService`, `Cipher`, `BackupTrigger`, `HostCerts`, `SiteCert`.
Services mounted under `/api`:
- `apikeys.go` — GET/POST `/api_keys`, DELETE `/api_keys/{id}`.
- `webhooks.go` — GET/POST `/webhooks`, DELETE `/webhooks/{source}`.
- `secrets.go` — GET/POST `/secrets` (`?scope=`/`?stack=` filters),
  PUT/DELETE `/secrets/{name}` (edit value / delete), GET `/secrets/{name}/value`
  (decrypt + reveal).
- `configs.go` — CRUD `/configs` (`?scope=`/`?stack=` filters),
  `/configs/{name}/versions`, `/configs/{name}/rollback`.
- `update.go` — POST `/api/update` (runs `cluster update` on the daemon;
  `Deps.Update` closure wired in `cli/serve.go`).
- `rendered.go` — GET `/api/cluster/rendered` (stored rendered platform
  configs, snapshotted into the DB by every `cluster update`; read-only;
  `Deps.Rendered` wired with the store).
- `tls.go` — GET/PUT/DELETE `/tls/hosts[/{host}]` (per-host certs, DB-backed).
- `sitecert.go` — GET/PUT `/tls/site` (cluster's own cert).
- `stacks.go` (`internal/api`) — GET/POST `/stacks`, GET `/stacks/{name}`,
  GET `/stacks/{name}/revisions/{rev}`, POST `/stacks/{name}/rollback`,
  **DELETE `/stacks/{name}`** (full teardown via `deploy.Service.Undeploy`:
  services, volumes, mounted secrets, record + configs/secrets).
- Also backups, nodes, me, cluster/info.

Every service follows the same shape: a struct with dependencies + a
`Mount(r chi.Router)` method; handlers return JSON via `writeJSON`/`writeErr`.

### internal/store — SQLite persistence
`Store` wraps `modernc.org/sqlite`. `Store.Open(dbPath)` applies embedded
migrations (each in its own transaction, tracked in `schema_version` —
exactly-once, idempotent). Repositories by file:
- `users.go` — daemon users + v2 API tokens (`pmc_<id>_<secret>`), argon2id.
- `credentials.go` — platform credentials, AES-GCM ciphertext.
- `stacks.go` — stack records + revisions (unix-ts revisions, `NextFreeRevision`
  collision avoidance, `DeleteStack` cascades revisions via FK and removes the
  stack's service-scope configs + secrets).
- `webhooks.go`, `registries.go`, `backups.go`, `settings.go`
  (key/value), `configs.go` + `secrets.go` (migration 0008, DSL-backed; rows
  carry `scope` (cluster|service) + `stack` (owning stack, service scope);
  `ListConfigs`/`ListSecrets` filter by `(scope, stack)`, `UpdateSecret`
  replaces a value in place), `sitecerts.go` (migration 0009, cert metadata
  for main + per-host).

### internal/auth — bearer-token auth
`Bearer(lookup)` middleware; `auth.User{ID, Name}` on context.
Token format: `pmc_<8-hex-token-id>_<base64url-secret>`; lookup is indexed by
token id (no O(N) argon2 scan).

### internal/credentials — AES-GCM
`Cipher` encrypts/decrypts secrets at rest with the key from
`~/.pmcluster/.encryption_key` (32 bytes, generated at init).

### internal/cluster — cluster orchestration (the core)
`UpDeps`/`UpdateDeps`/`SiteCertDeps`/`HostCertsDeps` bundle `Store, Cipher,
Docker, Deployer, Provisioner` (+ `Stdout` where verbose).
- `up.go` — `cluster up`: idempotent bootstrap; renders 4 embedded stacks in
  order `infra → edge → observability → backup`, provisions OpenObserve
  user+token, waits for health, saves settings.
- `update.go` — `cluster update`: content-aware re-apply. Loads TLS state +
  domain + OO credential; re-materializes cert/key secrets; renders OTel +
  Traefik dynamic + edge configs; re-deploys only stacks whose content moved
  (configs are versioned `*_vNNN` and compared by hash label).
- `down.go` — teardown (removes stacks, secrets, configs, networks).
- `secrets.go` — `EnsureVersionedSecret`, `EnsureVersionedSecretFromFile`,
  `EnsureSecret`, `RandomPassword`; versioning via `pmcluster.data_hash` label
  (Docker secrets are write-only on inspect — hash labels are the ONLY way to
  compare content).
- `templates.go` — `RenderInput` + `LoadComposeFile` with `[[ ]]` delimiters
  (so offen's `{{ .Node.ID }}` survives) and `${DOMAIN}`-style substitution;
  `RenderTraefikDynamic` + `appendHostCertSecrets`; `EdgeImageFor()` (edge
  image pinned to the release version tag).
- `embeds/` — the embedded compose templates: `infra-stack.yml` (Traefik +
  Portainer), `edge-stack.yml`, `observability-stack.yml` (OpenObserve + OTel),
  `backup-stack.yml` (volume + control-plane backup agents),
  `traefik-dynamic.yml`, `otel-collector-config.yml`.
- `sitecert.go` — `ApplyCert`/`RemoveCert`/`GetSiteCert`/`PersistedDomain`/
  `warnSiteCertExpiry` (one flow for main + per-host certs).
- `hostcerts.go` — `HostCertEntry`, `hostSecretBase`, `loadHostCertEntries`,
  `RefreshHostCerts`.
- `tlscerts/` — `Validate` (hostname), `ParseAndCheck` (PEM pair validation
  + SAN/CN coverage). No filesystem management (removed).
- `health.go` — `WaitHealthyStacks` polls the bundled services.
- `credentials.go` — platform credential specs (edge_admin, portainer, OO…).

### internal/manifest + internal/dsl — the deployment DSL
Pipeline: `Parse` (strict YAML, unknown keys rejected) → `Interpolate`
(`${app} ${env} ${version} ${registry} ${domain}` + `${env:VAR}`) → `Validate`
(required fields, mount rules) → `Translate` (→ Compose v3.9, auto-injects
networks, Traefik labels, cors middleware, healthchecks, restart/update
policies). `env` values may reference DB-backed values:
`config(<name>)` (injects content) and `secrets(<name>)` (resolves to
`/run/secrets/<name>` — requires the secret to be in the service's `secrets:`
array). `translate.go` takes an optional `EnvResolver` (deploy.Service wires a
`StoreConfigResolver` from `internal/deploy/resolver.go`).

### internal/deploy — deploy service
`deploy.Service` = single engine behind CLI deploy, `/api/stacks`, and
webhooks. `Deploy(ctx, payload)` → parse/interpolate/validate/translate →
`docker stack deploy` → records revision (unix timestamp + NextFreeRevision
collision avoidance). `Rollback(ctx, stack, revision)`. `Undeploy(ctx, stack)`
= full teardown: `docker stack rm`, then remove the stack's named volumes
(`VolumeList` by `com.docker.stack.namespace` label) and the Swarm secrets its
services mount (`StackSecretNames`), then `Store.DeleteStack` (stack row +
revisions via FK cascade + the stack's service-scope configs/secrets) — backs
the console Delete button and `DELETE /api/stacks/{name}`.
`Resolver` field for config()/secrets() env refs.

### internal/webhook — CI webhook receiver
`POST /webhook/{source}`. Auth = HMAC-SHA256 over
`timestamp_decimal + raw_body` (headers `X-Pmcluster-Timestamp` ±5min window,
`X-Pmcluster-Signature: sha256=<hex>`). Every failure → generic 401.
Body: `DeployPayload{app_name, version, manifest, repo_url?}`.

### internal/docker — Docker/Swarm client wrapper
Thin wrapper over the Docker SDK. Key surface: `ServiceList/Inspect`,
`SecretList/Create/Remove`, `StackSecretNames` (secrets a stack's services
mount, by `com.docker.stack.namespace` label), `ConfigList/Create`,
`VolumeList/Remove` (stack volumes by namespace label), `StackDeploy`
(`docker stack deploy -c -` with `--detach --resolve-image=always
--with-registry-auth`), `ForceUpdateService` (retries on Swarm "update out of
sequence"), network operations, container logs. `docker.New()` may return nil
client on error (callers must nil-check).

### internal/edgeproxy — smart reverse proxy
`New(config)` → chi router: per-real-IP token-bucket rate limits (separate
`/api` vs `/webhook` buckets, `/health` exempt), in-flight semaphore → 503,
request timeouts + body caps, auto-ban after N trips → 403 (`Blocklist`),
real-IP extraction from trusted XFF (`RealIP`). `config.go` reads env
(`API_RATE`, `WEBHOOK_RATE`, `MAX_CONCURRENT`, `BAN_THRESHOLD`, …).

### internal/ui — operator console (gin + HTMX)
`App` (in `ui.go`) wires: session auth (bcrypt, cookie), `store.Store` on the
`edge_pmui-data` volume, `pmapi.Client` (talks to the daemon API), views
renderer + fragment templates. Controllers in `controllers/` (`overview`,
`stacks`, `stackconfigs`, `webhooks`, `apikeys`, `tls`, `backups`, `deploy`,
`settings`, `auth`). Routes: `/healthz` answered locally, `/login`, `/setup`,
`/` console pages, everything else reverse-proxied to the daemon.
Templates in `views/templates/` (`app.html` shell + `frag_*.html`).
`pmapi/` = generated-style REST client for the daemon API (models + methods).

Console organization: **cluster-scope** configs + secrets (the platform's own
templates/secrets) live on the **Settings** page, which also has the
**Apply to swarm** button (`POST /api/update`) that re-runs `cluster update`
so edits reach the swarm side. **Service-scope** configs + secrets for a stack
live on `/stacks/<name>/config` (reached via the **Config** button in the
stacks list); create forms prefill the `<stack>_` name prefix and record the
owning stack. Secret values are revealed on demand with a confirmation prompt
(`/secrets/reveal/:name`-style routes) and editable via `PUT /api/secrets/{name}`.
The stacks list has a per-row **Delete** button (confirmation prompt) that
fully tears the stack down (`DELETE /api/stacks/{name}` →
`deploy.Service.Undeploy`: services, named volumes, mounted secrets, record +
its service-scope configs/secrets).

### internal/telemetry + internal/openobserve + internal/backup + internal/logger + internal/buildinfo
- `telemetry`: OTel SDK init (metrics/traces → OTLP :4318 collector).
- `openobserve`: thin OO API client (`EnsureUser`, `EnsureIngestionToken`).
- `backup`: `LocalTrigger` (spawns the volume-backup container).
- `logger`: zerolog setup. `buildinfo`: `Version`/`Commit` vars (ldflags).

### migrations/ + internal/store/migrations.go
0001 init (users, sessions), 0002 credentials, 0003 stacks/revisions,
0004 webhooks/registries, 0005 backups, 0006 token_index, 0007 settings,
0008 configs/config_versions/secrets, 0009 site_certs, 0010 stack columns on
configs + secrets (service-scope rows carry their owning stack). `runMigrations`
applies each once (schema_version table), each in its own transaction.

---

## 6. Key runtime flows

### Bootstrap
`install.sh` → `pmcluster init` (seeds `~/.pmcluster/`, key, admin user) →
`pmcluster cluster up --domain <d> [--cert/--key | --acme-email]`
(deploys infra→edge→observability→backup) → daemon runs via systemd/brew
(`pmcluster serve`).

### Upgrading the platform
Push a `v*` tag → `release.yml` publishes binaries + edge image
(`ghcr.io/nextrum-sy/pmcluster-edge:<v>` + `:latest`). On the node:
`curl -fsSL .../install.sh | VERSION=vX bash` → installs the new binary,
restarts the daemon, and auto-runs `pmcluster cluster update` (because a
config exists). `cluster update` re-renders the embedded edge-stack (image
pinned to the new version) and any changed configs. **Never** use `docker
service update --image ...` manually — the edge image must stay in lock-step
with the binary.

### Deploying an app
DSL manifest → `pmcluster deploy m.yaml` OR `POST /api/stacks` OR webhook
(HMAC-signed CI). All three hit `deploy.Service.Deploy`. The translator emits
compose with Traefik labels (`Host(<expose.host>)`, entrypoints websecure,
tls, middleware cors-default) so the app is served at
`https://<expose.host>/`.

### TLS management
- Cluster's own cert: `pmcluster tls site set|show` / PUT `/api/tls/site` /
  console. Applies via `cluster.ApplyCert` with `refresh=true`: writes local
  copy to `<configDir>/site/`, re-points TLS state, runs Update →
  `cert_vN/key_vN` secrets → Traefik dynamic config vN → infra redeploy.
- Per-host certs: `pmcluster tls hosts add|list|remove` / `/api/tls/hosts` /
  console. Same flow (`ApplyCert`), secrets `hostcert-<host>_vN`/
  `hostkey-<host>_vN`, row in `site_certs`, rendered as extra
  `tls.certificates` entries referencing `/run/secrets/...`.
- Expiry warnings: console flags certs ≤30 days out; `cluster update` prints
  ⚠ lines.

### Secrets & configs
`pmcluster secret create|list|show|verify|delete` and
`pmcluster config create|list|get|edit|history|rollback` (DB-backed, AES-GCM
encrypted, sha256 hashes displayed, version history + rollback; `create`
takes `--scope` and `--stack`). Referenced from the DSL env via
`config(name)` / `secrets(name)`. Console: cluster-scope values live on the
**Settings** page (edit + **Apply to swarm** via `POST /api/update`);
service-scope values live on `/stacks/<name>/config`. Hashes shown; values
revealed on demand with confirmation, and editable in place.

### Edge proxy behavior
Public origin → edge container → rate-limit per client IP → shield →
auto-ban → forward to daemon. The console UI is served by the same process.

---

## 7. Where to change things (common tasks)

| Task | Where |
|---|---|
| Add a CLI command | new file in `internal/cli/`, register in `init()`; add OpenAPI row if it has an API twin |
| Add a REST endpoint | `internal/server/<name>.go` (struct + `Mount`), wire in `server.New` + `cli/serve.go` Deps |
| Add a console page | `internal/ui/controllers/<name>.go`, route in `internal/ui/ui.go`, template `views/templates/frag_<name>.html`, nav in `app.html`, pmapi method in `internal/ui/pmapi/` |
| Add a DB table | new `migrations/00NN_*.sql` + `internal/store/<name>.go` repo + tests |
| Change compose the platform deploys | `internal/cluster/embeds/*.yml` (+ `templates.go` `[[ ]]`/`${}` substitution) |
| Change the edge proxy policy | `internal/edgeproxy/*.go` + env defaults in `embeds/edge-stack.yml` |
| Change the DSL schema | `pkg/dsl/types.go`, then `internal/manifest/{validate,translate,interpolate}.go`, docs in `docs/dsl.md` |
| Add a platform credential | `internal/cluster/credentials.go` spec + `internal/store/credentials.go` |
| Change cert flow | `internal/cluster/sitecert.go` (ApplyCert/RemoveCert) + `server/{tls,sitecert}.go` |
| Change auth model | `internal/auth/`, `internal/store/users.go`, token minting in `internal/server/apikeys.go` |

---

## 8. Conventions

- **Comments**: only standard Go doc comments (package docs + exported
  identifiers). No inline/paragraph commentary. Documented via `golangci-lint`
  (revive `package-comments`, `var-naming`).
- **JSON**: snake_case keys; times as RFC3339 UTC strings (`rfTime` helper in
  `internal/server`); one-time secrets/tokens returned exactly once.
- **Errors**: sentinels in `internal/store` (`ErrXNotFound`, `ErrXExists`);
  services map them to HTTP statuses. Wrapped with `%w`.
- **Content-awareness**: configs/secrets are versioned `*_vNNN` and compared
  by `pmcluster.data_hash` label — never re-mint what didn't change.
- **Lint** (`pmcluster/.golangci.yml`): errcheck, govet, ineffassign,
  staticcheck, unused, misspell, gocritic, revive.
- **Tests**: stdlib only; `httptest` for servers; e2e gated with
  `//go:build e2e`.

---

## 9. Verification loop

From `pmcluster/`:
```
go build ./...
go vet ./...
go vet -tags e2e ./e2e/
go test ./...                # ~26 packages
golangci-lint run ./...
gofmt -l .                   # must be empty
```
CI mirrors this: `pmcluster.yml` (gate), `swarm-e2e.yml` (informational real
swarm), `release.yml` (tags → binaries + edge image). Release flow for the
node: tag → `install.sh | VERSION=vX bash` → verify daemon `/health`, edge
image pin, console 302, `pmcluster tls site show`.

---

## 10. Production facts (as of v0.2.33)

- Node `root@82.165.128.237`, SSH key `~/.ssh/pmcluster_ed25519`, `rg` NOT
  installed (use `grep`), `sqlite3` available.
- Daemon `v0.2.33`, edge pinned `:v0.2.33`, domain `nextrum-sy.com`.
- Console login: admin / `wfO1C3yY1CGVLfWEnKA-BJHMWUmoqMl$4Kb0`
  (that's the `edge_admin` credential).
- `site_certs` rows: `nextrum-sy.com` (cert_v041/key_v041) +
  `idlebbookfair.com` (hostcert-idlebbookfair-com_v001/hostkey-..._v001).
- Live Traefik dynamic config `pmcluster_traefik_dynamic_v043` references
  those secrets.
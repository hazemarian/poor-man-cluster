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
(Traefik, OpenObserve, OTel collector, backups), accepts application
deployments through a YAML DSL, and manages TLS, secrets, configs, API keys,
webhooks and backups — via a CLI, a REST API, and an operator console.

A second Go binary, `pmcluster-edge`, runs as a Swarm service and is the public
origin for the whole platform: it is both an **operator console** (gin + HTMX)
and a **smart reverse proxy** (rate limits, in-flight shield, auto-ban) that
 forwards the daemon's API and webhooks.

Production reference: a manager node running Docker Swarm (N nodes), a
public domain with wildcard TLS, console at `https://pmcluster.<domain>/web/`,
and optionally SSO via GitHub (oauth2-proxy on `https://sso.<domain>`).

---

## 2. Frameworks and key libraries

| Area | Choice | Notes |
|---|---|---|
| Language | Go 1.25 | module `github.com/hazemarian/poor-man-cluster/pmcluster` (root: `pmcluster/` dir) |
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
  App stacks   pmcluster-edge (:8042)   OpenObserve
  (deployed    ┌────────────────────┐     (observability)
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

    SSO:        oauth2-proxy (sso stack, sso.<domain>) → GitHub OAuth
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
├── skills/poor-man-cluster-deploy/SKILL.md   # deployment skill for agents
├── .github/workflows/
│   ├── pmcluster.yml          # build gate: lint + build + unit + fast e2e
│   ├── swarm-e2e.yml          # informational real-swarm e2e (never blocks)
│   └── release.yml            # on v* tags: binaries + edge image (GHCR)
└── pmcluster/                 # THE GO MODULE
    ├── cmd/pmcluster/main.go  # daemon binary entrypoint
    ├── cmd/edge/main.go       # edge console+proxy binary entrypoint
    ├── migrations/            # SQL migrations 0001..0014 (schema_version-tracked)
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
- `setup` (`setup.go`) — interactive wizard: collect cluster config then hand
  off to `cluster up` (fresh install) or `cluster update` (existing cluster).
- `service list|ps|tasks|logs|restart|exec` (`service.go`) — per-service
  operations that replace Portainer.
- `stack list|show`, `rollback`, `logs`, `backup` (`create|list|browse|restore`),
  `node`, `registry`, `credentials`, `user`, `webhook` (`add|list|remove|deliveries`),
  `tls`, `secret`, `config`, `cluster` (`settings|get|set`), `usage`,
  `serve`, `version`.
- The `serve` command (`serve.go`) wires the daemon: opens store, docker
  client, deploy service, and calls `server.New` — **this is where new daemon
  dependencies are injected**.
- `backend.go` — the CLI backend factory AND the **single local/remote
  switchpoint**. Every data command resolves its service through a
  `backendX(cmd)` helper that returns the **local** adapter (each domain
  package owns its own, e.g. `configs.NewLocal`) or the **remote** adapter
  (`internal/remote`, one shared REST-client family) when
  `--api-url`/`PMCLUSTER_API_URL` is set.
- Remote mode: persistent flags `--api-url` (+ `--api-token`, default
  `PMCLUSTER_API_TOKEN`) let the CLI run off-node against the daemon API.
  Commands with a REST twin (`config`, `secret`, `webhook`, `user`, `backup`,
  `deploy`, `stack`, `rollback`, `tls`) work remotely; bootstrap/repair
  commands (`init`, `cluster *`, `credentials *`, `node`, `registry`, `logs`,
  `serve`) are local-only (they need the node's SQLite + Docker socket).
- `config.Config` (from `internal/config`) carries `DBPath`, `ConfigDir`,
  `EncryptionKeyPath`, `ListenAddr`, `Domain`.

### Domain packages — one bounded context per package
Each domain owns its model types, port (interface), local adapter and REST
handler, all in one package. Consumers (CLI, daemon API, webhook, console)
depend only on the port, never on a concrete adapter, so any port can be
backed by the local core or by the daemon REST API without changing the
consumer. Ports by package:
- `stacks` — the deploy engine + read side. Ports: `Deployer`
  (`Deploy`/`Sync`/`Rollback`/`Undeploy`) and `Reader` (`Get`/`List`/`Revisions`);
  `Service` is the concrete engine (satisfies both), `Local` the read-side
  adapter.
- `cluster` — `Service` (`Up`, `Update`, `Down`, `Status`) and
  `CredentialsService` (platform credential CRUD/rotate).
- `configs` — `Service` (config CRUD + list filters + `SetRendered`/
  `ListRendered`).
- `secrets` — `Service` (CRUD + `Reveal`, `Update`).
- `certs` — `Service` (site + per-host TLS: `SiteCert`, `ApplyHostCert`
  (+refresh), `RemoveHostCert`(+refresh), `GetSiteCert`, `List`, `MainDomain`).
- `webhooks` — `Service` + `SourceReader` (decrypted HMAC secret material).
- `apikeys` — `Service` (edge-user + self-delete guards live here) +
  sentinels (`ErrEdgeUserProtected`, `ErrSelfDelete`).
- `backups` — `Service` (`Trigger`/`List`/`ListForStack`/`ListFiles`/`Restore`) + sentinel
  `ErrTriggerNotConfigured`.
- `services` — `Service` (`Reader` + `Ops` + `Logs`): list swarm services,
  task history, log tailing, forced restart, non-interactive exec. Replaces
  Portainer.

### Domain local adapters (on-node)
Each domain package owns its on-node adapter (wrapping the store, cipher,
docker client, or cluster package). Constructors: `configs.NewLocal(st)`,
`secrets.NewLocal(st, cipher)`, `certs.NewLocal(...)`,
`webhooks.NewLocal(st, cipher)`, `apikeys.NewLocal(st)`,
`backups.NewLocal(st, trigger)`, `cluster.NewService()` /
`cluster.NewCredentials(...)`, `services.Local{Docker: dc}`, and `stacks`
builds `&stacks.Service{...}` directly (its `Local` covers the read side).

### internal/remote — remote adapters (HTTP)
The off-node implementations of the domain ports, talking to the daemon REST
API. `Client{http,base,tok}` + `do()` (Bearer, 8MB cap) + `mapError` (status +
body → the store/domain sentinels, so callers can `errors.Is`).
`NewStacks` + `NewDeploy` (stacks), `NewConfigs`, `NewSecrets` (`Get` =
list-then-find; `Reveal` via `/secrets/{name}/value`), `NewWebhooks`,
`NewAPIKeys`, `NewBackups`, `NewTLS`, `NewServices`. `SetRendered` returns an
error ("internal cluster-update operation not available over the remote API").
`cluster.Service`/`CredentialsService` have **no** remote adapters yet.

### internal/refs — shared config()/secrets() reference language
Leaf package used by BOTH `internal/manifest` (DSL env values) and
`internal/cluster` (platform template refs). Provides:
- `RefResolver` interface: `ResolveConfig`/`ResolveSecret`.
- `ReplaceRefs(ctx, text, r)` — inline scanner rewrites every
  `config(name)`/`secrets(name)` in compose text.
- `ParseEnvRef(v)` — whole-value env parser (`env: KEY: config(x)`).
- `MalformedEnvRef(v)` — detects typos (e.g. `config(foo` without closing paren).
- `SecretMountPath(name)` — returns `/run/secrets/<name>`.
Imports: refs ← manifest, refs ← cluster (the `renderRefResolver` in
templates.go), refs ← stacks/resolver.go is NOT a direct import (the
StoreConfigResolver implements manifest.EnvResolver, not refs.RefResolver).

### internal/workflow — named-step runner
`Step{Name,Run}` + `Workflow{out,steps}` (`NewWorkflow(out)`, `Add`, `Run`
prints `▶ <name>` and wraps the first error with the step name). Shared by
`cluster up` (10 steps), `cluster update` (7 steps), `deploy` (5), `rollback`
(3) and `undeploy` (3) — the step lists are the public progress output.

### internal/api
Infra-only endpoints shared by the daemon: `/health`, `/api/me`, `/api/cluster/info`, `/api/nodes`. Domain REST surfaces live inside the domain packages (see below).

### internal/server — the daemon composition root (chi)
Bearer-authenticated `/api/*` router built in `New(Deps)`. **`server` holds no
domain logic** — it only assembles. `Deps` (in `server.go`) holds the **ports**:
`DeployService stacks.Deployer`, `Configs configs.Service`, `Secrets
secrets.Service`, `Webhooks webhooks.Service` + `WebhookSources
webhooks.SourceReader`, `APIKeys apikeys.Service`, `Backups backups.Service`,
`HostCerts *certs.HTTP` + `SiteCert *certs.HTTP` (the cert REST handlers),
`Update *UpdateService` (cluster update via the `cluster.Service` port),
`Services services.Service`, plus
the two infra fields `Lookup` (auth) and `Docker`. `New()` mounts the domain
handlers itself — each domain package exports its own `Mount` methods, e.g.:
- `apikeys.HTTP{Svc}` → `Mount(r)` = GET/POST `/api_keys`, DELETE `/api_keys/{id}`.
- `webhooks.HTTP{Svc}` → GET/POST `/webhooks`, DELETE `/webhooks/{source}`.
- `webhooks.DeliveriesHTTP{Svc}` → GET `/api/webhooks/{source}/deliveries`.
- `webhooks.Receiver{Sources, Deploy}` → POST `/webhook/{source}` (HMAC
  receiver; needs `WebhookSources` + `DeployService`).
- `secrets.HTTP{Svc}` → GET/POST `/secrets` (`?scope=`/`?stack=` filters),
  PUT/DELETE `/secrets/{name}`, GET `/secrets/{name}/value` (reveal).
- `configs.HTTP{Svc}` → CRUD `/configs` (`?scope=`/`?stack=` filters),
  `/configs/{name}/versions`, `/configs/{name}/rollback`, **and**
  GET `/api/cluster/rendered` (stored rendered platform configs).
- `backups.HTTP{Svc}` → GET/POST `/api/backups` + stack-scoped
  `/api/stacks/{name}/backups` + GET `/api/backups/{id}/files` +
  POST `/api/backups/{id}/restore`.
- `certs.HTTP{Svc}` → `MountHosts(r)` (GET/PUT/DELETE `/api/tls/hosts[/{host}]`)
  + `MountSite(r)` (GET/PUT `/api/tls/site`).
- `stacks.HTTP{Deploy, Read, Backups}` → GET/POST `/api/stacks`,
  GET `/api/stacks/{name}`, GET `/api/stacks/{name}/revisions/{rev}`,
  POST `/api/stacks/{name}/rollback`, POST `/api/stacks/{name}/sync`,
  **DELETE `/api/stacks/{name}`** (full
  teardown via `stacks.Service.Undeploy`: services, volumes, mounted secrets,
  record + configs/secrets).
- `services.HTTP{Svc}` → GET `/api/services`,
  GET `/api/services/{stack}`, GET `/api/services/{stack}/{service}/tasks`,
  GET `/api/services/{stack}/{service}/logs?tail=N`,
  POST `/api/services/{stack}/{service}/restart`,
  POST `/api/services/{stack}/{service}/exec` (body `{argv:[...]}`).
- `settings.HTTP{Svc settings.Service}` → GET `/api/cluster/settings` +
  PUT `/api/cluster/settings` (12 allowlisted keys; atomic; no redeploy).
- `usage.HTTP{Svc usage.Service}` → GET `/api/usage`.

All handlers follow the same shape: struct with a port dep + `Mount(r chi.Router)`;
JSON via `writeJSON`/`writeErr` (`server/write.go`).

### internal/store — SQLite persistence
`Store` wraps `modernc.org/sqlite`. `Store.Open(dbPath)` applies embedded
migrations (each in its own transaction, tracked in `schema_version` —
exactly-once, idempotent). Repositories by file:
- `users.go` — daemon users + v2 API tokens (`pmc_<id>_<secret>`), argon2id.
- `credentials.go` — platform credentials, AES-GCM ciphertext.
- `stacks.go` — stack records + revisions (unix-ts revisions, `NextFreeRevision`
  collision avoidance, `DeleteStack` cascades revisions via FK and removes the
  stack's service-scope configs + secrets). `StackRevision` carries
  `RenderedHash` (sha256 of rendered compose) for no-op deploy detection.
- `webhooks.go`, `registries.go`, `backups.go`, `settings.go`
  (key/value), `configs.go` + `secrets.go` (migration 0008, DSL-backed; rows
  carry `scope` (cluster|service) + `stack` (owning stack, service scope);
  `ListConfigs`/`ListSecrets` filter by `(scope, stack)`, `UpdateSecret`
  replaces a value in place), `sitecerts.go` (migration 0009, cert metadata
  for main + per-host).
- `configs.go` — `ConfigRow` carries `RenderedHash` (sha256 of last
  rendered snapshot). `SetRendered(ctx, name, content)` stores
  `rendered_content` + `rendered_hash` + `rendered_at`. `ConfigHash(content)`
  computes sha256 hex. `ListRenderedConfigs` returns configs with a
  rendered snapshot.

### internal/auth — bearer-token auth
`Bearer(lookup)` middleware; `auth.User{ID, Name}` on context.
Token format: `pmc_<8-hex-token-id>_<base64url-secret>`; lookup is indexed by
token id (no O(N) argon2 scan).

### internal/credentials — AES-GCM
`Cipher` encrypts/decrypts secrets at rest with the key from
`~/.pmcluster/.encryption_key` (32 bytes, generated at init).

### internal/cluster — cluster orchestration (the core)
`UpDeps`/`UpdateDeps`/`SiteCertDeps`/`HostCertsDeps` bundle `Store, Cipher,
Docker, Deployer` (+ `Stdout` where verbose).
- `up.go` — `cluster up`: idempotent bootstrap, run as a **workflow of 10
  named steps** (preflight → sync platform configs → networks → TLS →
  credentials → render configs → deploy stacks → wait health →
  snapshot rendered configs → persist install state). Renders embedded
  stacks, waits for health, saves settings. **Init-only**: errors
  "cluster already initialised" if domain/TLS state already persisted.
- `update.go` — `cluster update`: content-aware re-apply, run as a **workflow
  of 7 named steps**. Loads persisted install state + SSO settings (self-heals
  missing `sso_cookie_secret` via `CredentialsManager.Ensure`). Loads OO
  credentials, re-materializes cert/key secrets, renders OTel + Traefik
  dynamic + edge configs. Content-aware per-stack reconcile: hashes freshly
  rendered compose against `store.ConfigHash(fresh)` vs
  `configs.rendered_hash` — deploys only changed stacks (order:
  observability → infra → edge → backup, + sso when enabled). `SetRendered`
  all; drift-prune (`pruneStackServices`) removes services dropped from a
  compose.
- `down.go` — teardown (removes stacks, secrets, configs, networks).
- `secrets.go` — `EnsureVersionedSecret`, `EnsureVersionedSecretFromFile`,
  `EnsureSecret`, `RandomPassword`; versioning via `pmcluster.data_hash` label
  (Docker secrets are write-only on inspect — hash labels are the ONLY way to
  compare content).
- `templates.go` — `RenderInput` + `LoadComposeFile` with `[[ ]]` delimiters
  (so offen's `{{ .Node.ID }}` survives) and `${DOMAIN}`-style substitution;
  `RenderTraefikDynamic` + `appendHostCertSecrets`; `EdgeImageFor()` (edge
  image pinned to the release version tag). `ConfigFileNames` is 7
  (`infra-stack.yml`, `observability-stack.yml`, `backup-stack.yml`,
  `edge-stack.yml`, `sso-stack.yml`, `otel-collector-config.yml`,
  `traefik-dynamic.yml`). `readConfigFile` resolution = DB
  cluster/template/current-version row → embedded (NO disk files; DB is source
  of truth). `SyncClusterConfigs`/`SyncPlatformConfigs` k8s-style reconcile
  (create missing / refresh stale / preserve current-version rows).
  `EnsureConfig`/`EnsureVersionedSecret` hash data (no labels).
  `renderRefResolver` implements `refs.RefResolver` so platform templates use
  the same `config(name)`/`secrets(name)` syntax as the DSL.
- `embeds/` — the embedded compose templates: `infra-stack.yml` (Traefik),
  `edge-stack.yml`, `observability-stack.yml` (OpenObserve + OTel),
  `backup-stack.yml` (volume + control-plane backup agents),
  `sso-stack.yml` (oauth2-proxy), `traefik-dynamic.yml`,
  `otel-collector-config.yml`.
- `sitecert.go` — `ApplyCert`/`RemoveCert`/`GetSiteCert`/`PersistedDomain`/
  `warnSiteCertExpiry` (one flow for main + per-host certs).
- `hostcerts.go` — `HostCertEntry`, `hostSecretBase`, `loadHostCertEntries`,
  `RefreshHostCerts`.
- `health.go` — `WaitHealthyStacks` polls the bundled services.
- `credentials.go` — platform credential specs (`edge_admin`,
  `openobserve_admin`, `traefik_dashboard`, `sso_cookie_secret`).
  `CredentialsManager` with `Bootstrap`, `Ensure`, `Rotate`. `Ensure` is
  idempotent: creates only when missing (used by `cluster update` to self-heal
  `sso_cookie_secret`).
- `stacks.go` — `StackDeployer` interface + `dockerCLIDeployer` (shells out
  to `docker stack deploy -c -`). `pruneStackServices` removes services that
  belong to a stack but are no longer in the compose (drift-prune).

### internal/manifest + pkg/dsl — the deployment DSL
Pipeline: `Parse` (strict YAML, unknown keys rejected) → `Interpolate`
(`${app} ${env} ${version} ${registry} ${domain}` + `${env:VAR}`) → `Validate`
(required fields, mount rules, malformed ref detection) → `TranslateWithResolver`
(→ Compose v3.9, auto-injects networks, Traefik labels, cors middleware,
healthchecks, restart/update policies). `env` values may reference DB-backed
values: `config(<name>)` (injects content) and `secrets(<name>)` (resolves to
`/run/secrets/<name>` — requires the secret to be in the service's `secrets:`
array). Reference resolution goes through `internal/refs` (shared package).
`TranslateWithResolver` takes an `EnvResolver` (the stacks engine wires a
`StoreConfigResolver` from `internal/stacks/resolver.go`).

### internal/stacks — the deploy engine (deploy + read side)
`stacks.Service` = single engine behind CLI deploy, `/api/stacks`, and
webhooks; it **implements `stacks.Deployer`** and runs its methods as
named workflows (`deploy` 5 steps, `rollback` 3, `undeploy` 3 — via
`internal/workflow`). `Deploy(ctx, payload)` → parse/interpolate/validate/
translate → `docker stack deploy` → records revision (unix timestamp +
NextFreeRevision collision avoidance). `Rollback(ctx, stack, revision)`.
`Undeploy(ctx, stack)` = full teardown: `docker stack rm`, then remove the
stack's named volumes (`VolumeList` by `com.docker.stack.namespace` label) and
the Swarm secrets its services mount (`StackSecretNames`), then
`Store.DeleteStack` (stack row + revisions via FK cascade + the stack's
service-scope configs/secrets) — backs the console Delete button and
`DELETE /api/stacks/{name}`.
`Sync(ctx, stackName)` re-translates the stored source manifest against the
DB and **skips the deploy** when the rendered compose hashes identically to the
latest revision's `rendered_hash` — a no-op when nothing changed.
`Resolver` field for config()/secrets() env refs (via `StoreConfigResolver`).
`Local{Store}` is the read-side adapter (implements `Reader`).

### internal/services — service ops domain
Whitelisted per-service operations (replacing Portainer). Port interfaces:
`Reader` (List, Tasks), `Ops` (Restart, Exec), `Logs` (Logs) — bundled into
`Service` interface. `Local{Docker}` implements all three via the Docker SDK.
`http.go` serves the REST surface under `/api/services` (list, per-stack list,
tasks, logs, restart, exec). `remote.NewServices(rc)` for off-node access.
No raw Docker passthrough — every operation is a fixed, code-reviewed SDK call.

### internal/webhooks — webhook sources + HMAC receiver
`Receiver{Sources webhooks.SourceReader, Deploy stacks.Deployer}` answers
`POST /webhook/{source}`. Auth = HMAC-SHA256 over
`timestamp_decimal + raw_body` (headers `X-Pmcluster-Timestamp` ±5min window,
`X-Pmcluster-Signature: sha256=<hex>`). Every failure → generic 401.
Body: `DeployPayload{app_name, version, manifest, repo_url?}`.
The receiver reads the HMAC secret via the `SourceReader` port (never touches
store/cipher directly). `Local{Store,Cipher}` implements both `Service`
(source management) and `SourceReader`.

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
`App` (in `ui.go`) wires: session auth (HMAC-signed cookies, bcrypt), the
console's own `store.Store` (separate SQLite DB for users + settings),
`pmapi.Client` (talks to the daemon API), views renderer + fragment templates.
Controllers in `controllers/` (`overview`, `stacks`, `stackconfigs`,
`services`, `webhooks`, `apikeys`, `tls`, `backups`, `deploy`, `settings`,
`users`, `auth`). Routes: `/healthz` answered locally, `/login`, `/setup`,
`/` console pages, everything else reverse-proxied to the daemon.
Templates in `views/templates/` (`app.html` shell + `frag_*.html`).
`pmapi/` = generated-style REST client for the daemon API (models + methods).

RBAC roles (`admin > operator > viewer`):
- **viewer**: all GET page/fragment routes (read-only console).
- **operator**: mutations (sync/rollback/remove, service ops, backups, deploy
  submit, configs/secrets edits, tls, webhooks).
- **admin**: API keys, settings save/apply, and the users CRUD.

Auth middleware: `Require()` + `RequireRole(minRole)`. `LoginDisabled` mode
(EDGE_LOGIN_DISABLED=true): every request passes through as a synthetic admin
(the Traefik admin-auth gate protects `/web/*` in this mode).

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
`stacks.Service.Undeploy`: services, named volumes, mounted secrets, record +
its service-scope configs/secrets). External nav links to
`https://observ.<domain>` + `https://traefik.<domain>/dashboard/`.

### internal/ui/store — console's own persistence
Separate SQLite DB (`pmcluster-ui.db`) for users + settings. `User` carries a
`Role` column (`admin`/`operator`/`viewer`); pre-RBAC databases are migrated
via `ensureRoleColumn` (adds column + promotes first user to admin).
Methods: `ListUsers`, `GetByID`, `CountAdmins`, `UpdateUser`, `DeleteUser`,
`CreateUser(role)`, `SetPassword`, `CreateEnvUser`.

### internal/telemetry + internal/backups + internal/logger + internal/buildinfo
- `telemetry`: OTel SDK init (metrics/traces → OTLP :4318 collector).
- `backups`: `trigger.go` holds `LocalTrigger` (spawns the volume-backup
  container) + `RecordOutcome` metrics; the domain package is described above.
- `logger`: zerolog setup. `buildinfo`: `Version`/`Commit`/`Date` vars
  (ldflags); `buildinfo.Resolve()` used by `--version`.

### internal/ARCHITECTURE.md
One-page dependency-rule + domain-inventory doc; read it before touching the
package layout.

### migrations/ + internal/store/migrations.go
0001 init (users, sessions), 0002 credentials, 0003 stacks/revisions,
0004 webhooks/registries, 0005 backups, 0006 token_index, 0007 settings,
0008 configs/config_versions/secrets, 0009 site_certs, 0010 stack columns on
configs + secrets (service-scope rows carry their owning stack), 0011
rendered_configs (rendered_content + rendered_at), 0012
rendered_on_configs (adds rendered columns to configs), 0013
rendered_hash (rendered_hash column on configs for change detection), 0014
rendered_hash_stacks (rendered_hash on stack_revisions for no-op sync).
`runMigrations` applies each once (schema_version table), each in its own
transaction.

---

## 6. Key runtime flows

### Bootstrap
`install.sh` → `pmcluster init` (seeds `~/.pmcluster/`, key, admin token) →
`pmcluster setup` (interactive wizard: domain, TLS, SSO, edge login) →
`pmcluster cluster up` (deploys infra→edge→observability→backup, +sso when
enabled) → daemon runs via systemd/brew (`pmcluster serve`).
`cluster up` is init-only: it errors "cluster already initialised" if
domain/TLS state is already persisted — ongoing reconcile is `cluster update`.
`setup` fallback: `cluster up`/`cluster update` with no data invoke the wizard.

### Upgrading the platform
Push a `v*` tag → `release.yml` cross-compiles 4 tarballs + SHA256SUMS +
builds/pushes `ghcr.io/hazemarian/pmcluster-edge:<v>` + `:latest`. On the node:
`curl -fsSL .../install.sh | VERSION=vX bash` → installs the new binary,
restarts the daemon, and auto-runs `pmcluster cluster update` (because a
config exists). `cluster update` re-renders the embedded edge-stack (image
pinned to the new version) and any changed configs. **Never** use `docker
service update --image ...` manually — the edge image must stay in lock-step
with the binary.

### Deploying an app
DSL manifest → `pmcluster deploy m.yaml` OR `POST /api/stacks` OR webhook
(HMAC-signed CI). All three hit `stacks.Service.Deploy`. Pipeline:
Parse → Interpolate → Validate → TranslateWithResolver (resolves
config()/secrets() from DB) → RecordDeploy (revision w/ rendered_hash) →
DeployStack + drift-prune. The translator emits compose with Traefik labels
(`Host(<expose.host>)`, entrypoints websecure, tls, middleware cors-default)
so the app is served at `https://<expose.host>/`.
Sync re-translates the stored source manifest and **skips** when the rendered
compose hashes identically to the latest revision's `rendered_hash`.

### SSO (single sign-on)
When SSO is enabled via `pmcluster setup --sso-enabled`:
- The SSO stack (oauth2-proxy) is deployed alongside the platform stacks.
- Traefik's `/web/*` and `observ.<domain>` routers use a `forwardAuth`
  middleware pointing at the oauth2-proxy sidecar (`sso.<domain>`, `/oauth2/*`
  resolves on every gated host).
- The OAuth provider is GitHub, restricted to an optional org.
- The edge console's own login is disabled (EDGE_LOGIN_DISABLED=true);
  RBAC roles are still enforced when it is enabled.
- Cookie secret is a managed credential (`sso_cookie_secret`); `cluster update`
  self-heals it when missing via `CredentialsManager.Ensure`.

Without SSO: Traefik admin-auth basicAuth (`htpasswd` from
`admin_credentials` secret) gates `/web/*` + `observ.<domain>` + traefik
dashboard.

### Remote CLI (off-node)
Set `--api-url https://pmcluster.<domain>` (or `PMCLUSTER_API_URL`) plus
`--api-token pmc_...` (or `PMCLUSTER_API_TOKEN`). Commands then run against
the daemon API instead of the local store/docker: `pmcluster --api-url ... \
--api-token ... config list`, `deploy`, `stack list`, `rollback`, `secret ...`,
`webhook ...`, `user create`, `backup create`, `tls ...`, `service ...`.
Bootstrap/repair commands stay local.

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
  warning lines.

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
| Add a CLI command | new file in `internal/cli/`, register in `init()` via `rootCmd.AddCommand()`; add OpenAPI row if it has an API twin |
| Add a REST endpoint | the domain package's `http.go` (struct + `Mount`), wire in `server.New` (Deps) + `cli/serve.go` |
| Add a console page | `internal/ui/controllers/<name>.go`, route in `internal/ui/ui.go` Mount (viewer GET / operator POST), template `views/templates/frag_<name>.html`, nav in `app.html`, pmapi method in `internal/ui/pmapi/` |
| Add a DB table | new `migrations/00NN_*.sql` + `internal/store/<name>.go` repo + tests |
| Add a platform config | `ConfigFileNames` in `templates.go` + embed file in `internal/cluster/embeds/` + sync logic in `SyncClusterConfigs` |
| Change compose the platform deploys | `internal/cluster/embeds/*.yml` (+ `templates.go` `[[ ]]`/`${}` substitution + `refs.ReplaceRefs`) |
| Change the edge proxy policy | `internal/edgeproxy/*.go` + env defaults in `embeds/edge-stack.yml` |
| Change the DSL schema | `pkg/dsl/types.go`, then `internal/manifest/{validate,translate,interpolate}.go`, docs in `docs/dsl.md` |
| Add a platform credential | `internal/cluster/credentials.go` spec + `internal/store/credentials.go` |
| Change cert flow | `internal/certs/` (port + `local.go` wrapping `internal/cluster/sitecert.go`) |
| Change auth model | `internal/auth/`, `internal/store/users.go`, token minting in `internal/apikeys/local.go` |
| Change console auth | `internal/ui/middleware/session.go` (Require/RequireRole) + `traefik-dynamic.yml` + embeds conditionals |
| Add a config()/secrets() reference target | `internal/refs/refs.go` (RefResolver), `internal/cluster/templates.go` (configNameAliases / secretNameAliases maps) |

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
  Platform stack deploys compare `store.ConfigHash(fresh render)` against
  `configs.rendered_hash` — skip when identical.
- **Lint** (`pmcluster/.golangci.yml`): errcheck, govet, ineffassign,
  staticcheck, unused, misspell, gocritic, revive.
- **Tests**: stdlib only; `httptest` for servers; e2e gated with
  `//go:build e2e`.

---

## 9. Verification loop

From `pmcluster/`:
```
make vet
make build
go test -race -count=1 ./...
golangci-lint run
```
E2e tests:
```
go test -timeout 10m -tags=e2e -count=1 ./e2e/...
```
CI workflows: `pmcluster.yml` (lint+build+unit+smoke e2e) gates main;
`swarm-e2e.yml` (non-blocking real-swarm e2e); `release.yml` on v* tags
cross-compiles 4 tarballs + SHA256SUMS + builds/pushes pmcluster-edge image
to `ghcr.io/hazemarian/pmcluster-edge`.

Release flow for the node: tag → `install.sh | VERSION=vX bash` → verify
daemon `/health`, edge image pin, console 302 → `/web/`, `pmcluster tls site show`.

---

## 10. Production facts (reference deployment)

These mirror a typical live installation — operators should substitute their
own values (manager node, domain, GitHub org, root admin email).

- Manager node `root@<manager-ip>` (SSH key `~/.ssh/pmcluster_ed25519`), `rg` NOT
  installed (use `grep`), `sqlite3` available.
- Daemon pinned to the release tag, edge pinned to matching release tag,
  domain `<domain>`.
- Console at `https://pmcluster.<domain>/web/`.
- SSO live: GitHub OAuth (org `<github-org>`), oauth2-proxy on
  `https://sso.<domain>`, `EDGE_LOGIN_DISABLED=true`.
- OpenObserve root admin email: `admin@<domain>` (derived from domain).
  Auth = root admin email:password (NOT provisioning API / ingestion token).
- `site_certs` rows: `<domain>` + per-host certs for extra subdomains.
- Traefik auth gates `/web/*` + `observ.<domain>` + traefik dashboard
  (basicAuth when SSO disabled, forwardAuth via oauth2-proxy when enabled).

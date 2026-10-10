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
    Backups:       offen agents (hourly, storage nodes) + in-cluster SeaweedFS
                   store (S3 :8333 routing-mesh / WebDAV :7333) + offsite S3
```

- **Two overlay networks**: `traefik-net` (app traffic via Traefik) and
  `monitoring-net` (telemetry). Apps that `expose` get both; every app also
  gets a private per-app overlay (declared `net`, deployed as `<app>_net` —
  skipped when the manifest sets app-level `networks`).
- **Traefik** is the single HTTPS entry point: `mode: global` **pinned to
  managers** (the swarm provider lists services through the local Docker
  socket, which only a manager can read) and fronted by the ingress routing
  mesh from every node (80/443 published ingress). Its dynamic config
  (`pmcluster_traefik_dynamic` Docker config, content-addressed name) holds
  middlewares (`admin-auth`, `cors-default`, `sso-auth`) and the TLS
  certificates list.
- **pmcluster daemon** is a host process (`systemd` on Linux, `brew services`
  on macOS) listening on `0.0.0.0:9090` — all interfaces, so the edge reaches it
  via `host.docker.internal:9090` (configurable via `listen_addr`).
  The edge reaches it through `host.docker.internal:host-gateway`.
- **Certificates**: cluster's own cert + each per-host (bring-your-own) cert
  are stored as **content-addressed** Swarm secrets
  (`cert_<sha8>/key_<sha8>`, `hostcert-<host>_<sha8>/hostkey-<host>_<sha8>`)
  with metadata rows in the `site_certs` table. Nothing is read from the
  manager's filesystem except the cluster cert's local copy under
  `~/.pmcluster/config/site/`.

---

## 4. Repository layout

```
repo root (this dir)
├── README.md                  # user-facing docs
├── agent.md                   # this file
├── docs/
│   ├── webhook.md             # CI webhook integration guide
│   ├── dsl.md                 # DSL reference
│   ├── storage-and-databases.md / network-topology.md / control-loop-design.md
│   ├── security-and-improvements.md / improvements.md
│   └── test-reports/          # per-test-case field reports (historical)
├── harden-host.sh             # one-shot host hardening (ufw, DOCKER-USER, fail2ban)
├── install.sh                 # curl|bash installer (binary + systemd + auto cluster up/update)
├── skills/poor-man-cluster-deploy/SKILL.md   # deployment skill for agents
├── .github/workflows/
│   ├── pmcluster.yml          # build gate: lint + build + unit + fast e2e
│   ├── swarm-e2e.yml          # informational real-swarm e2e (never blocks)
│   └── release.yml            # on v* tags: binaries + edge image (GHCR)
└── pmcluster/                 # THE GO MODULE
    ├── cmd/pmcluster/main.go  # daemon binary entrypoint
    ├── cmd/edge/main.go       # edge console+proxy binary entrypoint
    ├── migrations/            # SQL migrations 0001..0025 (schema_version-tracked)
    ├── e2e/                   # swarm e2e tests (//go:build e2e)
    ├── docs/                  # openapi.yaml, service-ops-design.md, restore-design.md,
    │                          # console-i18n-contract.md, redesign/ (concept mockups)
    └── internal/              # all packages (§5)
```

---

## 5. Package-by-package map (`pmcluster/internal/`)

### cmd/pmcluster + internal/cli — the CLI surface
`cobra.Command` tree. Every command registers itself in its file's `init()`.
Key commands (all in `internal/cli/`):
- `cluster up|update|reset|status|down` (`cluster.go`) — bootstrap / content-aware
  self-healing reconcile / Swarm rebuild-from-DB (`--restore` extracts a
  purge-backup first) / health summary / teardown (`--purge` writes a
  restorable `purge-backup-<ts>.tar.gz`, then deletes the local store).
- `deploy <manifest.yaml>` (`deploy.go`) — the DSL deploy entry point (7-step
  workflow; `--app/--version/--repo/--file` overrides). On a first node with
  no Swarm, `cluster up`/`setup` run `docker swarm init` transparently
  (`swarm.go` `ensureSwarmInitialized`, `--swarm-advertise-addr`).
- `setup` (`setup.go`) — interactive wizard: collect cluster config (domain,
  TLS, Traefik admin user, SSO, volume root, backup store placement, storage
  failover when offsite S3 is set) then hand off to `cluster up` (fresh
  install) or `cluster update` (existing cluster).
- `service list|ps|tasks|logs|restart|exec` (`service.go`) — per-service
  operations that replace Portainer. `list`/`ps` print a `STATE` column
  (`PAUSED`/`PAUSED: <error>`/`UPDATING`/`-`) from the swarm update status and
  a `PLATFORM` column; `--platform` filters to platform-managed services.
- `stack list|show|badge|move|ack` + `rollback` (`deploy.go`) — `badge`
  prints README status-badge markdown; `move --to <node>` relocates a
  stateful stack (backup → transit → pin → redeploy); `ack` acknowledges a
  storage failover.
- `backup create|list|browse|restore` (`backup.go`) — restore is
  volume-scoped (`--volume <name>[/<vol>]`), `--from-s3` forces the offsite
  bucket, and a stack-scoped restore is **routed to the owning node** via the
  store-transit mover (`restoreRouteForStack`).
- `node list|join-token|promote|demote` (`node.go`) — storage-node lifecycle
  (`storage_nodes` setting + `pmcluster.storage` label; the leader cannot be
  demoted).
- `secret create|edit|list|show|verify|delete|heal` (`secret.go`) — `edit`
  mirrors the new value to a content-addressed swarm secret
  (`swarmSecretMirrored`); `heal` verifies every secret decrypts, repairs
  stale hashes and re-materializes missing swarm mirrors.
- `config create|list|get|edit|history|rollback|delete` (`config.go`),
  `credentials list|show|rotate` (atomic: new secret first, old stays
  mounted, self-heal rebuilds missing mounts), `registry add|list|remove`,
  `user create (--stack)|list|remove`, `webhook add|list|remove|deliveries`,
  `tls hosts … / tls site show|set`, `cluster settings list|get|set`
  (`settings_usage.go`; 29-key allowlist, secret-ish keys masked), `usage`,
  `logs`, `serve`, `version`.
- `join` (`join.go`) — swarm join + local state init + daemon unit + hostname
  management; flags `--copy-registry-creds <host>` (ssh-fetch + merge the
  manager's `~/.docker/config.json`), `--verify-registry-pull <image>`,
  `--storage-node` (stamps the `pmcluster.storage` label; the leader adopts
  it into `storage_nodes` on the next update).
- Tailnet option (M3, `tailscale.go`) — opt-in `--tailscale` (+
  `--tailscale-auth-key` / `$PMCLUSTER_TAILSCALE_AUTH_KEY`) on `join` and
  `cluster up`/`setup`: brings the node onto a private WireGuard tailnet via the
  `tailscale` CLI and advertises the tailnet IPv4 to the Swarm, so node-to-node
  traffic (2377/7946/4789 + storage ports) needs no firewall rules. Failing
  loudly on tailnet error (explicit opt-in; silent public-IP fallback would
  defeat the purpose).
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
  (`Deploy`/`DeployAsync`/`Sync`/`Rollback`/`Undeploy`/`MoveWithOptions`/
  `RestoreArchiveToNode`) and `Reader` (`Get`/`List`/`Revisions`);
  `Service` is the concrete engine (satisfies both), `Local` the read-side
  adapter.
- `cluster` — `Service` (`Up`, `Update`, `Down`, `Status`) and
  `CredentialsService` (platform credential CRUD/rotate).
- `configs` — `Service` (config CRUD + list filters + `SetRendered`/
  `ListRendered`); the local adapter mirrors every write into a
  content-addressed swarm config (`mirrorSwarmConfig`).
- `secrets` — `Service` (CRUD + `Reveal`, `Update`) + `Heal` (verify/repair
  every secret: decrypt, stale hash, missing swarm mirror).
- `settings` — `Service` (Get/Update over the fixed 29-key allowlist;
  `ApplyLogLevel`/`ApplyStorageNodes` hooks let the daemon apply live).
- `certs` — `Service` (site + per-host TLS: `SiteCert`, `ApplyHostCert`
  (+refresh), `RemoveHostCert`(+refresh), `GetSiteCert`, `List`, `MainDomain`).
- `webhooks` — `Service` + `SourceReader` (decrypted HMAC secret material).
- `apikeys` — `Service` (edge-user + self-delete guards live here) +
  sentinels (`ErrEdgeUserProtected`, `ErrSelfDelete`). Minted keys are
  **operator**-tier (`CreateUserWithRole`).
- `backups` — `Service` (`Trigger`/`List`/`ListForStack`/`Browse`/`Restore`) +
  sentinel `ErrTriggerNotConfigured`. Discovery indexes BOTH the local
  archive dir and the in-cluster store (`listS3Objects`, prefix `backup-`,
  control-plane archives excluded); `s3.go` is a stdlib SigV4 client
  (signs `x-amz-date` — required by SeaweedFS).
- `services` — `Service` (`Reader` + `Ops` + `Logs`): list swarm services,
  task history, log tailing, forced restart, non-interactive exec. Replaces
  Portainer. Each summary carries the `platform` flag, the pinned `node`,
  `RunOnce`, `ImageCreated` and `UpdateState/UpdateError` diagnostics.

### Domain local adapters (on-node)
Each domain package owns its on-node adapter (wrapping the store, cipher,
docker client, or cluster package). Constructors: `configs.NewLocal(st)`
(+`Docker` for swarm mirroring), `secrets.NewLocal(st, cipher)`,
`certs.NewLocal(...)`, `webhooks.NewLocal(st, cipher)`,
`apikeys.NewLocal(st)`, `backups.NewLocal(st, trigger)` (or a `&backups.Local`
built directly with ArchiveDir/RetentionDays/S3 for discovery + offsite),
`settings.NewLocal(st)` (+`ApplyLogLevel`/`ApplyStorageNodes` hooks wired by
the daemon), `cluster.NewService()` / `cluster.NewCredentials(...)`,
`services.Local{Docker: dc}`, and `stacks`
builds `&stacks.Service{...}` directly (its `Local` covers the read side).

### internal/remote — remote adapters (HTTP)
The off-node implementations of the domain ports, talking to the daemon REST
API. `Client{http,base,tok}` + `do()` (Bearer, 8MB cap) + `mapError` (status +
body → the store/domain sentinels, so callers can `errors.Is`).
`NewStacks` + `NewDeploy` (stacks), `NewConfigs`, `NewSecrets` (`Get` =
list-then-find; `Reveal` via `/secrets/{name}/value`), `NewWebhooks`,
`NewAPIKeys`, `NewBackups`, `NewTLS`, `NewServices`, `NewSettings`,
`NewUsage`. `SetRendered` returns an
error ("internal cluster-update operation not available over the remote API").
`cluster.Service`/`CredentialsService` have **no** remote adapters yet.

### internal/refs — shared config()/secrets()/settings()/secret()/config_path() reference language
Leaf package used by BOTH `internal/manifest` (DSL env values) and
`internal/cluster` (platform template refs). Provides:
- `RefResolver` interface: `ResolveConfig`/`ResolveSecret`.
- Optional capability interfaces: `SettingsResolver` (`settings(name)`),
  `ConfigPathResolver` (`config_path(name)` — default mount path `/etc/<name>`),
  `SecretValueResolver` (`secret(name)` — the secret's VALUE as-is, vs
  `secrets(name)` which names a file mount).
- `ReplaceRefs(ctx, text, r)` — inline scanner rewrites every reference kind
  in compose text (unknown kinds fail loud).
- `ParseEnvRef(v)` — whole-value env parser (`env: KEY: config(x)`).
- `MalformedEnvRef(v)` — detects typos (e.g. `config(foo` without closing paren).
- `SecretMountPath(name)` — returns `/run/secrets/<name>`.
- `FindAll(text)` — every `config(...)`/`secrets(...)` reference in a text,
  deduped per kind, in source order. Used by the usage graph: the DSL
  resolves `config()` into env CONTENT at translation time, so the true
  reference graph is read from the SOURCE DSL manifest via
  `FindAll(source_yaml)`.
Imports: refs ← manifest, refs ← cluster (the `renderRefResolver` in
templates.go), refs ← stacks/resolver.go is NOT a direct import (the
StoreConfigResolver implements manifest.EnvResolver, not refs.RefResolver).

### internal/workflow — named-step runner
`Step{Name,Run}` + `Workflow{out,steps}` (`NewWorkflow(out)`, `Add`, `Run`
prints `▶ <name>` and wraps the first error with the step name). Shared by
`cluster up`, `cluster update` (self-healing: swarm-ID wipe detection,
network/credential re-ensure, DB-index repair, storage-label sync),
`cluster reset`, `deploy` (7 steps), `rollback` (4) and `undeploy` (3) — the
step lists are the public progress output and are recorded on each deploy
revision (`payload_json` envelope).

### internal/api
Infra-only endpoints shared by the daemon: `/health`, `/api/me`, `/api/cluster/info`, `/api/nodes` (each node reports its `storage` flag) and `POST/DELETE /api/nodes/{hostname}/storage` (promote/demote, `api.NodeStorageHandler`). Domain REST surfaces live inside the domain packages (see below).

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
  GET `/api/cluster/rendered` (stored rendered platform configs —
  `auth.RequireRole(RoleOperator)` gated: they embed root credentials).
- `backups.HTTP{Svc}` → GET/POST `/api/backups` + stack-scoped
  `/api/stacks/{name}/backups` + GET `/api/backups/{id}/files` +
  POST `/api/backups/{id}/restore`.
- `BadgeMount(r, st)` → public no-auth GET+HEAD `/api/public/badge/{stack}`,
  `/api/public/badge/{stack}/services`, `/api/public/badge/{stack}/{service}`
  (flat SVG status badges for READMEs). Reads **only** the `stack_status` DB
  snapshot the reconcile loop writes — never live Docker; an unacknowledged
  `stack_failover` marker dominates with an amber `failover` badge.

### internal/reconcile — the control loop (L1)
`reconcile.go`: `Reconciler{Store, Docker, DeployService *stacks.Service,
Services services.Service, Update func(ctx, cluster.UpdateDeps,
cluster.UpdateInput) (*cluster.UpdateResult, error), UpdateDeps, UpdateInput,
Log zerolog.Logger, Interval time.Duration, FailoverMove func(ctx, stack,
target) error, mu sync.Mutex, runID int64}` + throttled per-stack error/pause
state.
- `RunOnce(ctx)` — one pass (TryLock: one at a time): (a) platform reconcile
  via `Update` (re-render + hash-compare + existence check + redeploy drifted),
  (b) `ListStacks` → per-stack: **storage-node outage pause** (skip when the
  pinned node is down; optional `storage_failover` auto-move via
  `MoveWithOptions{FromS3:true}` with a 5-min cooldown + failover marker) else
  `DeployService.Sync` (no-op when the rendered hash matches AND
  `stackdrift.InSync` says the live swarm matches), (c) `snapshotHealth` writes
  `SetStackStatus` per stack (status + per-service map) and prunes stale
  `stack_status` rows. Emits OTLP spans (`pmcluster.reconcile` root +
  `pmcluster.reconcile.platform` child), `pmcluster.reconcile.total`,
  `pmcluster.storage.node.down` / `.failover.total` / `.failover.disabled`
  metrics; never restarts services on health — only reports (the failover
  move is the deliberate exception).
- Sync errors and "storage node down" warnings are **throttled**: logged once
  per distinct error string / down-node (state change) plus a once-per-hour
  WRN heartbeat; the stack's status is marked `error` with the parse error.
- `Loop(ctx)` — event-driven via `runtime.Client.Events` (Swarm events,
  2s debounce; a closed event channel disables that case) + an `Interval`
  safety ticker (default 60s from the `reconcile_interval` setting; 0
  disables). Wired in `serve.go` on the leader: `WatchSwarmLeadership`
  channel starts/stops it (workers/standby managers never run it).
- `stack_status` table (migration 0022): `stack_status(stack_name PK,
  status, services JSON, updated_at)` — the DB health snapshot badges + the
  console read. `stack_failover` (migration 0024): the unacknowledged
  failover marker (from/to/at/acked) behind the amber badge, `stack ack`
  and the console banner.
- `certs.HTTP{Svc}` → `MountHosts(r)` (GET/PUT/DELETE `/api/tls/hosts[/{host}]`)
  + `MountSite(r)` (GET/PUT `/api/tls/site`).
- `stacks.HTTP{Deploy, Read, Backups}` → GET/POST `/api/stacks`,
  GET `/api/stacks/{name}`, GET `/api/stacks/{name}/revisions/{rev}`,
  POST `/api/stacks/{name}/rollback`, POST `/api/stacks/{name}/sync`,
  **DELETE `/api/stacks/{name}`** (full
  teardown via `stacks.Service.Undeploy`: services, volumes, mounted secrets,
  record + configs/secrets).
- `services.HTTP{Svc}` → GET `/api/services` (each row carries a `platform`
  flag from the `io.pmcluster.platform` label),
  GET `/api/services/{stack}`, GET `/api/services/{stack}/{service}/tasks`,
  GET `/api/services/{stack}/{service}/logs?tail=N`,
  POST `/api/services/{stack}/{service}/restart`,
  POST `/api/services/{stack}/{service}/exec` (body `{argv:[...]}`), plus
  GET `/api/services/{stack}/{service}/exec/ws` (the interactive TTY relay —
  see the terminal design in `docs/service-ops-design.md` §8).
- `settings.HTTP{Svc settings.Service}` → GET `/api/cluster/settings` +
  PUT `/api/cluster/settings` (29 allowlisted keys; atomic; no redeploy;
  secret-ish keys masked for non-admin bearers).
- `usage.HTTP{Svc usage.Service}` → GET `/api/usage`.
- `api.NodeStorageHandler` → POST/DELETE `/api/nodes/{hostname}/storage`
  (storage-node promote/demote, wired in `server.go`).

All handlers follow the same shape: struct with a port dep + `Mount(r chi.Router)`;
JSON via `writeJSON`/`writeErr` (`server/write.go`).

### internal/store — SQLite persistence
`Store` wraps `modernc.org/sqlite`. `Store.Open(dbPath)` applies embedded
migrations (each in its own transaction, tracked in `schema_version` —
exactly-once, idempotent). Repositories by file:
- `users.go` — daemon users + v2 API tokens (`pmc_<id>_<secret>`), argon2id,
  `role` tier (admin/operator/viewer, migration 0025).
- `credentials.go` — platform credentials, AES-GCM ciphertext.
- `stacks.go` — stack records + revisions (unix-ts revisions, `NextFreeRevision`
  collision avoidance, `DeleteStack` cascades revisions via FK and removes the
  stack's service-scope configs + secrets; `last_error` JSON error history,
  `RecordStackError`). `StackRevision` carries `RenderedHash`
  (sha256 of rendered compose — **stamped only after a successful apply**) for
  no-op deploy detection, plus the `payload_json` envelope (payload + step
  list).
- `settings.go` — key/value cluster settings (`GetSettingDefault`).
- `secrets.go`/`configs.go` — DB-backed secrets/configs (migration 0008;
  `scope` (cluster|service) + `stack`; `SwarmSecretName`/`SwarmConfigName`
  derive the **content-addressed** `<name>_<sha256-first-8>` swarm object
  names; `SecretHash`/`ConfigHash` are the sha256 fingerprints;
  `UpdateSecret` replaces a value in place), `sitecerts.go` (migration 0009,
  cert metadata for main + per-host), `backups.go` (audit rows + discovered
  archives, filename unique index), `webhooks.go` (sources + deliveries with
  `retries`), `registries.go`, `stackfailover.go` (migration 0024 marker),
  `stackstatus.go` (migration 0022 snapshot).
- `configs.go` — `ConfigRow` carries `RenderedHash` (sha256 of last
  rendered snapshot). `SetRendered(ctx, name, content)` stores
  `rendered_content` + `rendered_hash` + `rendered_at`. `ConfigHash(content)`
  computes sha256 hex. `ListRenderedConfigs` returns configs with a
  rendered snapshot.

### internal/auth — bearer-token auth
`Bearer(lookup)` middleware; `auth.User{ID, Name, Stack, Role}` on context.
Token format: `pmc_<8-hex-token-id>_<base64url-secret>`; lookup is indexed by
token id (no O(N) argon2 scan; non-`pmc_`/short garbage is fast-rejected
before any scan). `Stack` scopes a token to one app stack:
`internal/server/scope.go` `stackScopeGuard` (mounted right after `auth.Bearer`
in the `/api` route group) restricts a scoped token to its own-stack
`/api/stacks/{name}` + `/api/services/{stack}` routes and a matching
`POST /api/stacks` deploy; everything else under `/api` returns
`403 {"error":"token scoped to stack <x>"}` (fail-closed). Unscoped tokens
(`stack=''`) are unchanged. Scope is set with `pmcluster user create --stack <s>`.
`Role` is the daemon tier (admin > operator > viewer; empty ranks as viewer):
`RequireRole(min)` chi middleware gates role-sensitive routes (rendered
configs = operator; settings values are masked for non-admins by the settings
HTTP layer).

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
  `traefik-dynamic.yml`) — the five stack files are DSL manifests
  (`app.platform: true`) that flow through the SAME manifest pipeline as app
  stacks; the two config files render through `refs.ReplaceRefs`.
  `readConfigFile` resolution = DB
  cluster/template/current-version row → embedded (NO disk files; DB is source
  of truth). `SyncClusterConfigs`/`SyncPlatformConfigs` k8s-style reconcile
  (create missing / refresh stale / preserve current-version rows).
  `EnsureConfig`/`EnsureVersionedSecret` hash data (no labels) and mint
  **content-addressed** names with GC of stale `<base>_*` objects.
  `renderRefResolver` implements the `refs.RefResolver` surface
  (+ `SecretValueResolver` etc.) so platform templates use the same
  `config(name)`/`secrets(name)`/`settings(name)`/`secret(name)` syntax as
  the DSL. `loadObjectStore` builds the in-cluster SeaweedFS store config
  from the `seaweedfs_admin` credential; `pickStoreNode` pins it to a
  non-storage, non-platform node (or the leader with `backup_store_on=leader`).
- `embeds/` — the embedded platform DSL manifests: `infra-stack.yml`
  (Traefik, global on managers), `edge-stack.yml`, `observability-stack.yml`
  (OpenObserve + OTel), `backup-stack.yml` (offen agents + SeaweedFS store),
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
ONE pipeline for everything (app stacks AND the five platform stacks):
`Parse` (strict YAML, unknown keys rejected; `platform: true` is refused in
user deploys by `stacks.Deploy`) → `Interpolate`
(`${app} ${env} ${version} ${registry} ${domain}` + `${env:VAR}`) → `Validate`
(required fields, mount rules, malformed ref detection) → `BuildIR`
(→ the backend-neutral `IR`; resolves env refs through the `EnvResolver`)
→ `ComposeWriter.Write` (→ Compose v3.9; auto-injects networks, Traefik
labels, cors middleware, healthchecks, restart/update policies, storage-pin
constraints, standard + `io.pmcluster.*` labels; escapes literal `$` as `$$`
in every user-derived value). `env` values may reference DB-backed values:
`config(<name>)` (injects content), `secrets(<name>)` (resolves to
`/run/secrets/<name>` — requires the secret to be in the service's `secrets:`
array), `settings(<name>)` (a live cluster setting) and `secret(<name>)`
(the value itself). `configs:` entries are `config_path(<name>)` file mounts
(default path `/etc/<name>`). Reference resolution goes through
`internal/refs` (shared package). `BuildIR` takes an `EnvResolver` (the
stacks engine wires a `StoreConfigResolver` from
`internal/stacks/resolver.go`, which implements all the resolver
capabilities — swarm-first reads with DB rebuild-on-missing).

**`ir.go`** is the backend-neutral IR (services, resolved env, volumes,
secrets, configs, ports, binds, labels, logging, resources, mode, restart,
constraints, expose, healthcheck, replicas as `*int` (0 = scale-to-zero
survives), depends_on; `IR.Subset(level)` keeps the top-level
Platform/Networks/Configs/PlainVolumes for per-level renders). **`Writer`**
is the port; `ComposeWriter` the only implementation today.

**`compose_writer.go`** builds the Compose v3.9 from the IR. Notable
behaviors: named volumes declared with bind `driver_opts` under the volume
root (top-level `volumes:` map entries pass through verbatim); `depends_on`
rendered as a plain list (stack deploy rejects the map/condition form);
stateful-aware defaults — a service mounting volumes gets
`update: order: stop-first` and is auto-pinned via `PinNode`/`SecretNames`/
`ConfigNames`/`ExtraLabels` writer inputs (`ExtraLabels` stamps
`runtime.RenderedHashLabel`); `secretExternalName`/`configExternalName`
(stacks) map logical names to the content-addressed swarm objects;
`escapeCompose` doubles literal `$`. A `run_once` service with `depends_on`
gets `restart_policy on-failure max_attempts 3` so a transient failure
retries; the startup ORDERING itself lives in the control plane
(`stacks.applyToSwarm` topo-sorts `manifest.ServiceLevels` and deploys
level-by-level — no wrapper is injected into any rendered artifact).

### internal/stacks — the deploy engine (deploy + read side)
`stacks.Service` = single engine behind CLI deploy, `/api/stacks`, and
webhooks; it **implements `stacks.Deployer`** and runs its methods as
named workflows (7-step `deploy`, 4-step `rollback`, 3-step `undeploy` — via
`internal/workflow`). `Deploy(ctx, payload)` → parse/interpolate/validate/
BuildIR → resolve placements (`PinResolver`: per-stack `stack_pin_<stack>` →
`storage_nodes` round-robin → `PinNode`) → `renderStamped` (render twice:
label-free to derive the content hash, then stamped with
`io.pmcluster.rendered_hash`) → record revision (hash EMPTY up front) →
ensure volume dirs → `applyToSwarm` (ordered depends_on levels; single-level =
full deploy + prune + force-update) → **stamp the rendered hash only after a
successful apply**. `DeployAsync` is the 202 path (validation + revision
synchronous, swarm apply detached). `app_name` conflict guard: one app name
per `repo_url`.
`Rollback(ctx, stack, revision)` re-translates the stored SOURCE with the
current translator (never re-deploys stale rendered YAML) as a NEW revision
(`rollback_of` marker in `payload_json`).
`Undeploy(ctx, stack)` = full teardown: `docker stack rm`, then remove the
stack's named volumes (`VolumeList` by `com.docker.stack.namespace` label) and
the Swarm secrets its services mount (`StackSecretNames`), then
`Store.DeleteStack` (stack row + revisions via FK cascade + the stack's
service-scope configs/secrets) — backs the console Delete button and
`DELETE /api/stacks/{name}`.
`Sync(ctx, stackName)` re-translates the stored source manifest against the
DB, **skips the deploy** when the rendered compose hashes identically to the
latest revision's `rendered_hash` AND `stackdrift.InSync` confirms the live
swarm matches — a no-op when nothing changed; a manual `docker service
update` or half-applied deploy re-applies.
`MoveWithOptions` (move.go) + `RestoreArchiveToNode` (move_store.go): the
store-transit mover (one-shot swarm service pulls the archive object from the
in-cluster store via `host.docker.internal:8333`, rclone + tar, verifies the
stack's subtree before wiping the target) with an ephemeral-HTTP fallback;
`Ack` clears the failover marker. `Resolver` field for the env refs (via
`StoreConfigResolver`). `Local{Store}` is the read-side adapter (implements
`Reader`).

### internal/services — service ops domain
Whitelisted per-service operations (replacing Portainer). Port interfaces:
`Reader` (List, Tasks), `Ops` (Restart, Exec), `Logs` (Logs) — bundled into
`Service` interface. `Local{Docker}` implements all three via the Docker SDK.
`http.go` serves the REST surface under `/api/services` (list, per-stack list,
tasks, logs, restart, exec). `remote.NewServices(rc)` for off-node access.
No raw Docker passthrough — every operation is a fixed, code-reviewed SDK call.
Each listed service also carries diagnostics: `ImageCreated` (unix seconds the
local image was built; drives the console's stale-image pill), and
`UpdateState`/`UpdateError` (Swarm `UpdateStatus.State/Message` — surfaces a
paused rollout with the failing task's error in the console).

### internal/webhooks — webhook sources + HMAC receiver
`Receiver{Sources webhooks.SourceReader, Deploy stacks.Deployer}` answers
`POST /webhook/{source}`. Auth = HMAC-SHA256 over
`timestamp_decimal + raw_body` (headers `X-Pmcluster-Timestamp` ±5min window,
`X-Pmcluster-Signature: sha256=<hex>`). Every failure → generic 401.
Body: `DeployPayload{app_name, version, manifest, repo_url?}`.
The receiver reads the HMAC secret via the `SourceReader` port (never touches
store/cipher directly). `Local{Store,Cipher}` implements both `Service`
(source management) and `SourceReader`.
Deploys are retried on transient failure: `webhooks/retry.go`
`RetryDeployer` (default 2 retries, 30s apart; `Receiver.MaxRetries`/
`RetryDelay` configurable, negative disables). The retry phase runs on
`context.WithoutCancel` under a ~4-minute `deployPhaseBudget` (the chi
`middleware.Timeout(30s)` is shorter than the retry window), and the delivery
row records `retries=N` (accepted if a retry recovers, else
`server_error, retries=2, error=<final>`); one request always yields exactly
one delivery row (migration `0019_webhook_delivery_retries.sql`).

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
`App` (in `ui.go`) wires: session auth (HMAC-signed cookies with a persisted
random secret, bcrypt, `Secure` flag on HTTPS), the console's own
`store.Store` (separate SQLite DB for users + settings; **in-memory** when
login is disabled — the swarm deployment runs the edge stateless, no
`pmui-data` volume), `pmapi.Client` (talks to the daemon API), views renderer
+ fragment templates, and a global **CSRF Origin/Referer guard** on
state-changing routes. Controllers in `controllers/` (`overview`, `stacks`,
`stackconfigs`, `services`, `platform`, `nodes`, `webhooks`, `apikeys`,
`tls`, `backups`, `deploy`, `settings`, `users`, `usage`, `inventory`,
`terminal`, `auth`, `sso_bridge`). Routes: `/healthz` answered locally,
`/login`, `/setup`, `/logout` (SSO-aware sign-out), `/lang/:code`, `/`,
console pages, everything else reverse-proxied to the daemon. Templates in
`views/templates/` (`app.html` shell + `frag_*.html`); htmx is vendored
locally; i18n dictionaries in `views/i18n` (see
`docs/console-i18n-contract.md`). Brand identity: `static/brand.css`
(`--brand-*` tokens, six palettes + dark/light themes), `frag_logo.html`
(combination mark), `frag_preferences.html` (theme + palette picker,
persisted before first paint).

RBAC roles (`admin > operator > viewer`):
- **viewer**: all GET page/fragment routes (read-only console) — including
  the Platform page and the rendered-config LIST.
- **operator**: mutations (sync/rollback/move/ack/remove, service ops +
  terminal, backups, deploy submit, configs/secrets edits + reveal, tls,
  webhooks, node promote/demote) and the rendered-config CONTENT modal.
- **admin**: API keys, settings save/apply, the users CRUD.

Auth middleware: `Require()` + `RequireRole(minRole)`. `LoginDisabled` mode
(EDGE_LOGIN_DISABLED=true): every request passes through as a synthetic admin
(the Traefik admin-auth gate protects `/web/*` in this mode) and no user rows
exist.

Console organization: **cluster-scope** configs + secrets (the platform's own
templates/secrets) live on the **Settings** page, which also has the
**Apply to swarm** button (`POST /api/update`) that re-runs `cluster update`
so edits reach the swarm side. **Service-scope** configs + secrets for a stack
live on `/stacks/<name>/config` (reached via the **Config** button in the
stacks list); create forms prefill the `<stack>_` name prefix and record the
owning stack. Secret values are revealed on demand with a confirmation prompt
and editable via `PUT /api/secrets/{name}`. The stacks list has a per-row
**Delete** button (confirmation prompt) that fully tears the stack down
(`DELETE /api/stacks/{name}` → `stacks.Service.Undeploy`). Platform stacks
are hidden from the stacks list; their services live on `/web/platform`
(+ a separated panel on the Services page). The Overview page carries the
nodes table with the storage pill + promote/demote. External nav links to
`https://observ.<domain>` + `https://traefik.<domain>/dashboard/`.

### internal/ui/store — console's own persistence
Separate SQLite DB (`pmcluster-ui.db`) for users + settings. `User` carries a
`Role` column (`admin`/`operator`/`viewer`); pre-RBAC databases are migrated
via `ensureRoleColumn` (adds column + promotes first user to admin).
Methods: `ListUsers`, `GetByID`, `CountAdmins`, `UpdateUser`, `DeleteUser`,
`CreateUser(role)`, `SetPassword`, `CreateEnvUser`.

### internal/telemetry + internal/backups + internal/logger + internal/buildinfo
- `telemetry`: OTel SDK init (metrics/traces → OTLP :4318 collector).
  `metrics.go` adds alerting counters/gauges with lazy OTel instruments +
  an injectable `Sample`/`SetSink` sink (hermetic tests, no exporter needed):
  `pmcluster.webhook.requests.total{source,status}` (receiver exit paths),
  `pmcluster.services.paused{scope}` + `pmcluster.services.stale_images{scope}`
  (image age > 30d; emitted from the services list handlers),
  `pmcluster.reconcile.total{stack,status}` (one per `stacks.Sync` run),
  `pmcluster.storage.node.down{node}` / `pmcluster.storage.failover.total` /
  `pmcluster.storage.failover.disabled` (storage HA alerting).
  Existing `pmcluster.backups.total{kind,status}` +
  `pmcluster.backup.last_unix{kind,status}` +
  `pmcluster.deploys.total{stack,status}` + `pmcluster.deploy.duration`
  cover backup/deploy observability.
- `backups`: `trigger.go` holds `LocalTrigger` (WAL checkpoint →
  `docker exec backup_volume-backup backup`, one bounded retry) +
  `RecordOutcome` metrics; `s3.go` is the stdlib SigV4 S3 client (GET +
  ListObjectsV2, signs `x-amz-date`); the domain package is described above.
- `logger`: zerolog setup (log_level cluster setting applied live).
  `buildinfo`: `Version`/`Commit`/`Date` vars (ldflags);
  `buildinfo.Resolve()` used by `--version`.

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
rendered_hash_stacks (rendered_hash on stack_revisions for no-op sync), 0015
source_file, 0016 webhook_deliveries, 0017 users_last_used, 0018
backup_filename, 0019 webhook_delivery_retries, 0020 api_key_stack, 0021
stack_last_error, 0022 stack_status, 0023 secret_swarm_rev, 0024
stack_failover, 0025 users_role.
`runMigrations` applies each once (schema_version table), each in its own
transaction.

---

## 6. Key runtime flows

### Bootstrap
`install.sh` → `pmcluster init` (seeds `~/.pmcluster/`, key, admin token) →
`pmcluster setup` (interactive wizard: domain, TLS, SSO, edge login, volume
root, backup store placement, storage failover) → `pmcluster cluster up`
(deploys infra→edge→observability→backup, +sso when enabled; on a first node
with no Swarm it runs `docker swarm init` first — `swarm.go`) → daemon runs
via systemd (`pmcluster serve`).
`cluster up` is init-only: it errors "cluster already initialised" if
domain/TLS state is already persisted — ongoing reconcile is `cluster update`.
`setup` fallback: `cluster up`/`cluster update` with no data invoke the
wizard; a joined-but-unpromoted standby manager is recognized and skipped.

### Control plane failover
`serve` waits for swarm leadership (15s poll; workers/standby managers idle).
At startup — BEFORE the store opens — `ensureControlPlaneFresh` restores from
the newest Raft-replicated `pmcluster_state_*` Docker config (key split into
`pmcluster_state_key_*`); on promotion the leader only publishes a fresh
snapshot (`controlplane.Kit`). Alongside the reconcile loop the leader runs
`kit.Loop` (5-min change-detection snapshot; 500 KiB config ceiling; keeps 2).

### Upgrading the platform
Push a `v*` tag → `release.yml` cross-compiles 4 tarballs + SHA256SUMS +
builds/pushes `ghcr.io/hazemarian/pmcluster-edge:<v>` + `:latest`. On the node:
`curl -fsSL .../install.sh | VERSION=vX bash` → installs the new binary,
restarts the daemon, and auto-runs `pmcluster cluster update` (because a
config exists). `cluster update` is self-healing: it re-renders the embedded
stacks (edge image pinned to the new version), re-ensures networks and
credential secrets, rebuilds missing configs/secrets from the DB index,
redeploys stacks missing from the swarm, detects a wiped Swarm (swarm_id
change), and syncs storage-node labels. **Never** use `docker
service update --image ...` manually — the edge image must stay in lock-step
with the binary.

### Deploying an app
DSL manifest → `pmcluster deploy m.yaml` OR `POST /api/stacks` OR webhook
(HMAC-signed CI). All three hit `stacks.Service.Deploy` (API/webhook via
`DeployAsync` → 202, swarm apply in the background). Pipeline:
Parse → Interpolate → Validate → BuildIR (resolves config()/secrets()/
settings()/secret() refs from the swarm-first resolver) → resolve placements
→ renderStamped (hash → `io.pmcluster.rendered_hash` label) → RecordDeploy
(revision, hash EMPTY) → ensure volume dirs → applyToSwarm (ordered
depends_on levels; drift-prune once) → stamp the rendered hash on success.
The translator emits compose with Traefik labels
(`Host(<expose.host>)`, entrypoints websecure, tls, middleware cors-default,
certresolver on ACME clusters) so the app is served at
`https://<expose.host>/`.
Sync re-translates the stored source manifest and **skips** only when the
rendered compose hashes identically to the latest revision's `rendered_hash`
AND the live swarm still matches (`stackdrift.InSync`).

### Backups & storage failover
Hourly offen agents on the storage nodes snapshot the whole volume root and
double-write each archive (WebDAV → in-cluster SeaweedFS store; AWS_* →
offsite `backup_s3_*` when configured); the control-plane agent archives
`~/.pmcluster` daily. Discovery indexes the local archive dir + the store
into `backup list`. A stack-scoped `backup restore` routes to the owning
node via the store-transit mover (`RestoreArchiveToNode`; `--from-s3` pulls
the offsite bucket instead). When a pinned storage node goes down, the
reconcile loop pauses the stack's sync — or, with `storage_failover=true`,
moves it to a healthy alternate (S3 restore of the FAILED node's newest
archive, 5-min cooldown) and leaves the failover marker (amber badge) until
`stack ack` / move-back.

### SSO (single sign-on)
When SSO is enabled via `pmcluster setup --sso-enabled`:
- The SSO stack (oauth2-proxy) is deployed alongside the platform stacks.
- Traefik's `/web/*` and `observ.<domain>` routers use a `forwardAuth`
  middleware pointing at the oauth2-proxy sidecar (`sso.<domain>`, `/oauth2/*`
  resolves on every gated host).
- The OAuth provider is GitHub, restricted to an optional org (and optional
  repos).
- The edge console's own login is disabled (EDGE_LOGIN_DISABLED=true); the
  console shell renders an `/oauth2/sign_out` link (clears the oauth2-proxy
  session) instead of its own logout form.
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
  content-addressed `cert_<sha8>/key_<sha8>` secrets (mounted at their
  versioned `/run/secrets/...` paths, matching the Traefik dynamic config) →
  infra redeploy.
- Per-host certs: `pmcluster tls hosts add|list|remove` / `/api/tls/hosts` /
  console. Same flow (`ApplyCert`), content-addressed
  `hostcert-<host>_<sha8>`/`hostkey-<host>_<sha8>` secrets, row in
  `site_certs`, rendered as extra `tls.certificates` entries referencing
  `/run/secrets/...`.
- Expiry warnings: console flags certs ≤30 days out; `cluster up`/`update`
  print warning lines.

### Secrets & configs
`pmcluster secret create|edit|list|show|verify|delete|heal` and
`pmcluster config create|list|get|edit|history|rollback|delete` (DB-backed,
AES-GCM encrypted, sha256 hashes displayed, version history + rollback;
`create` takes `--scope` and `--stack`). Referenced from the DSL env via
`config(name)` / `secrets(name)` / `settings(name)` / `secret(name)`; mounted
as files via `configs: [config_path(name)]`. **Swarm-first,
content-addressed**: every write mirrors a `<name>_<sha8>` swarm object (the
DB is the index and holds the value; `cluster update` rebuilds missing
objects; translation reads values from the swarm). Rotating a secret mints a
new object — the old, in-use one stays mounted. Console: cluster-scope
values live on the **Settings** page (edit + **Apply to swarm** via
`POST /api/update`); service-scope values live on `/stacks/<name>/config`.
Hashes shown; values revealed on demand with confirmation, and editable in
place.

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
- **Content-awareness**: configs/secrets are **content-addressed**
  (`<name>_<sha256-first-8>`, `store.SwarmConfigName`/`SwarmSecretName`) —
  identical content reuses the object, never re-mint what didn't change; the
  DB row is the index (and holds the value for rebuild-on-missing). Platform
  stack deploys compare `store.ConfigHash(fresh render)` against
  `configs.rendered_hash` (skip when identical) AND the live swarm against
  the `io.pmcluster.rendered_hash` label (`stackdrift.InSync`).
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

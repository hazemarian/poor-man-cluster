# 02 — Architecture

Everything in this chapter is cited to code (`pmcluster/internal/...`) or to
the operational documents it agrees with. Where the code and an older document
disagree, the disagreement is flagged (see also Chapter 07).

## 1. The desired-vs-actual model

```
┌────────────┐   render    ┌──────────────┐   docker stack deploy   ┌─────────┐
│  DSL YAML  │ ──────────► │ intermediate │ ──────────────────────► │  Swarm  │
│ (user or   │             │     IR       │      (actual)           │ (actual)│
│  platform) │             └──────────────┘                         └─────────┘
└────────────┘                      ▲                                     │
                                    │                                     │
                              desired state                         observed state
                                    │                                     │
                              ┌─────┴──────┐                              │
                              │  SQLite    │ ◄──── reconcile loop ────────┘
                              │ (desired)  │        (leader only)
                              └────────────┘
```

- **Desired state** lives in `~/.pmcluster/data.db` — the stack table
  (including each stack's latest `source_yaml`), revisions, the settings
  table, managed credentials (AES-256-GCM), users, webhook sources and
  deliveries, and the `stack_status` health snapshot
  (`pmcluster/internal/store/store.go:36-68`).
- **Actual state** is the Swarm: services, networks, secrets, configs, node
  labels.
- **The bridge** is the reconciler (`pmcluster/internal/reconcile/reconcile.go`)
  on the leader, and the deploy pipeline (`pmcluster/internal/stacks/deploy.go`)
  for explicit deploys.

The store is opened with `journal_mode(DELETE)` deliberately
(`pmcluster/internal/store/store.go:40-49`) — see DEC-07 for why WAL was
reverted, and BUG-004 for the corruption that forced it.

### Composition roots

The binary has exactly two places where adapters are wired: the CLI
(`internal/cli/backend.go` — the only local/remote switch) and the daemon HTTP
server (`internal/server.Deps`). Domain packages (`apikeys`, `webhooks`,
`secrets`, `configs`, `backups`, `certs`, `stacks`, `services`, `cluster`)
depend only on ports, never on transport
(`pmcluster/internal/ARCHITECTURE.md:8-24`). Cluster lifecycle and credential
rotation have **no REST endpoints at all** — they only run where the Docker
socket is (`pmcluster/internal/ARCHITECTURE.md:73-84`).

## 2. The reconcile loop

`pmcluster/internal/reconcile/reconcile.go` — leader only, one pass at a time.

| Property | Implementation |
|---|---|
| Leader-only | `serve.go` starts the loop from the `WatchSwarmLeadership` channel and cancels it on loss (`pmcluster/internal/cli/serve.go:277-304`). |
| One pass at a time | `RunOnce` takes `r.mu.TryLock()` and returns immediately if a pass is in flight (`reconcile.go:176-180`). |
| Triggers | Docker events (2 s debounce) + a `reconcile_interval` ticker (default 60 s; `0` disables) — `docs/control-loop-design.md:12-16`. |
| Three passes | `runPass` (`reconcile.go:205`) — (a) platform reconcile via `cluster.Update`, (b) app-stack drift sync via `stacks.Sync`, (c) health snapshot into `stack_status` (`reconcile.go:507`). |
| Error policy | A failing pass records its error on the span but does **not** stop the remaining passes — a broken platform pass still gets a health snapshot (`reconcile.go:202-204`). |
| Traces/metrics | One root span `pmcluster.reconcile` with a `pmcluster.reconcile.platform` child; `pmcluster.reconcile.total` counter (`reconcile.go:187-196`, `:37`). |

### 2.1 Drift detection — two signals, one label

1. **Stored-vs-fresh render.** Each stack stores a `rendered_hash` of what
   pmcluster last intended. A sync re-translates the stored `source_yaml` and
   compares; on a match, `stacks.Result.Changed` is `false` and nothing is
   deployed (`pmcluster/internal/ARCHITECTURE.md:108-110`).
2. **Fresh-vs-live.** When the hashes match, `stackdrift.InSync`
   (`pmcluster/internal/stackdrift/stackdrift.go:72`) still inspects the live
   Swarm: every deployed service carries the label
   `io.pmcluster.rendered_hash=<sha256>`
   (`pmcluster/internal/runtime/...:337`,
   `docs/dsl.md:316`). A manual `docker service update`, a half-applied deploy
   or a wiped service therefore re-applies even though the stored hash is
   unchanged.

The critical invariant, born from BUG-009 and BUG-018: **the label is stamped
only after a successful apply.** A failed deploy leaves the hash unstamped, so
the next pass sees drift and retries
(`docs/control-loop-design.md:47-49`, CHANGELOG v0.2.165).

### 2.2 Storage-node outage pause and opt-in failover

Pass (b) consults node health (`reconcile.go:378`). If a stack's pinned
storage node is down (absent, not `ready`, or not `active`), the stack's sync
is **paused** — deploying onto a node that cannot serve the data would
recreate it against empty binds. The pause is logged once per state change
plus an hourly heartbeat (`reconcile.go:340`, `:162-167`), never per pass
(BUG-020's log-spam class).

With `storage_failover=true` **and** offsite backups configured, the loop
instead calls `tryStorageFailover` (`reconcile.go:423`), which moves the stack
to a healthy alternate storage node, restoring the *failed node's* newest
archive via the store-transit mover, with a 5-minute per-stack cooldown
(`reconcile.go:170-172`). The move leaves a failover marker: the badge reads
`failover` (amber) until `pmcluster stack ack` or a move back clears it
(`docs/storage-and-databases.md:92-99`). Without the setting the loop only
alerts (`pmcluster.storage.node.down` /
`pmcluster.storage.failover.disabled`).

### 2.3 Worker and standby-manager behaviour

A worker daemon logs once —
`this node is a swarm worker — standing by (no control loop)`
(`pmcluster/internal/cli/leader.go:52`, `:156`) — and stays idle. A standby
manager logs `not the swarm leader — standing by (failover standby)` and polls
every 15 s (`leader.go:74`). Both behaviours exist because pre-v0.2.176 the
manager-only reconcile ran on workers and spammed `The swarm does not have a
leader …` / `not a manager` errors every ~5 s (BUG-020, BUG-033).

## 3. The control plane (Raft survivor kit)

Alongside the reconciler the leader runs `controlplane.Kit.Loop` — a 5-minute
tick plus an immediate snapshot on promotion — that publishes the
failover-survivor kit into two Docker config families
(`pmcluster/internal/controlplane/state.go:15-16`, `:48`):

- `pmcluster_state_<unixnano>` — a tar.gz of `data.db`, `config.yaml` and the
  `config/` tree;
- `pmcluster_state_key_<unixnano>` — the `.encryption_key` bytes alone.

**The key/ciphertext split is deliberate**: a restore refuses a state config
whose key config is missing, so key + ciphertext never share one blob
(`docs/improvements.md:20`). Swarm's own Raft store replicates the configs to
every manager; the snapshot only fires when a kit file changed since the
newest config, prunes to the newest two, and fails loudly above Docker's
500 KiB config ceiling (`docs/control-loop-design.md:72-81`).

**Restore is startup-only.** A promoted daemon never restores under its own
live-opened SQLite connection — `Restore` truncates `data.db`, which would
split-brain the daemon (stale page-cache rows vs. a clobbered file). Instead
`serve` restores from the newest `pmcluster_state_*` **before the store is
opened**, only when the local DB is missing or older than the snapshot, and
repairs a missing `.encryption_key` even when the DB is current
(`pmcluster/internal/cli/serve.go:286-292`, `docs/control-loop-design.md:83-92`).
On promotion the leader only *publishes* (`serve.go:293-295`).

**Degradation.** A worker node or a leaderless swarm makes snapshot/restore
no-ops that retry later rather than crash-looping the daemon
(`controlplane/state_test.go:459-540`, BUG-020/BUG-021).

## 4. Storage

### 4.1 Node-local volume roots

Every container volume is forced under a single host directory
(`volume_root`, default `/var/stack/data`). Named volumes are declared with
`driver_opts {type: none, o: bind, device: <root>/<app>/<name>}`; host binds
are relocated to `<root>/<app>/<basename>`; raw `binds:` entries (docker.sock,
sockets, device paths) are emitted verbatim
(`docs/storage-and-databases.md:191-199`, `docs/dsl.md:315`). A per-node
**volume-repair loop** in every daemon recreates missing bind-source
directories for the stateful services Swarm placed on that node — it runs on
standby managers and workers too (BUG-026 and its three follow-ups).

### 4.2 Placement resolution order

`PinResolver.ResolvePlacement` (`pmcluster/internal/stacks/placement.go:75`)
implements:

1. an explicit `placement: <hostname>` in the manifest (always wins);
2. the per-stack pin written by `pmcluster stack move` — the
   `stack_pin_<stack>` setting (`placement.go:183`);
3. a deterministic **round-robin** across the `storage_nodes` setting, keyed by
   FNV-1a of the stack name (`placement.go:154-161`) — so the same stack
   always lands on the same node;
4. `platform_node` (which defaults to the leader) as the single-node fallback.

Stateful services (those with a volume) also get `update.order: stop-first`
automatically — a start-first rollout races the old container's shutdown
against the new start (old Postgres deletes the freshly written
`postmaster.pid`) — `docs/dsl.md:256`.

### 4.3 The storage-node model

User directive (2026-10-02, `ses_f015e7886ffebPQLfTXFCjrRJx`):

> make sure when joining a node to cluster we need to add a flag in cli to
> select it as storage node
> by default main node is storage node
> service should had a label in which node it is running and that should be
> displayed in the UI
> make sure worker node could be storage node not only leader are storage nodes

Implemented as: `pmcluster join --storage-node` stamps the swarm-visible
`pmcluster.storage` node label; workers qualify; the leader's next
`cluster update` **adopts** any labeled-but-unlisted node into `storage_nodes`
(BUG-019a's fix); every deployed service carries `io.pmcluster.node=<hostname>`
so the console and `pmcluster service ps` show where stateful work runs.

And the invariant that closed BUG-024:

> edge should be only with leader so api cli webhook goes through it and leader
> is aways storage node so we have the container maybe the bug that leader was
> not storage and that should not be allowed
> cli should fail this is not a storage node
> …
> make sure you prevent leader from not being a storage
> and if a manager was not a storage node and got the leader ship it should be
> promoted to storage and the fail leader to worker

So: `cluster up` seeds the leader's hostname into `storage_nodes`; every
`cluster update` re-adds the current leader; on leadership change the new
leader is promoted and the former leader demoted (v0.2.170/v0.2.171,
`docs/storage-and-databases.md:69-81`).

## 5. Backups

Three components, all rendered from the platform DSL manifest
`pmcluster/internal/cluster/embeds/backup-stack.yml`:

### 5.1 The agents

`offen/docker-volume-backup`, `mode: global` constrained to
`pmcluster.storage`-labeled nodes when `storage_nodes` is set (one agent per
storage node), or a single replica pinned to the platform node otherwise
(`backup-stack.yml:31`). Schedule is the `backup_cron` setting, **default
hourly** (`0 * * * *`) — `pmcluster/internal/cluster/cluster_state.go:455` —
so the newest archive is never more than an hour old. Retention is
`backup_retention_days` (default 15). A second agent archives the control
plane (`~/.pmcluster`: `data.db` + `.encryption_key` + `config.yaml`) daily at
03:00 with a 30-day retention.

### 5.2 The in-cluster store (SeaweedFS)

A single SeaweedFS process serving **both** the S3 API (:8333) and WebDAV
(:7333) in one container (`backup-stack.yml:86-97`):

- **S3 :8333 is published on the routing mesh**, so it is reachable at
  `127.0.0.1:8333` on *every* node — the leader daemon (failover restore,
  `backup restore --from-s3` discovery, store indexing) and the mover read it
  with no cross-node host ports (DEC-11).
- **WebDAV :7333 is the write leg**: offen's WebDAV backend creates the bucket
  path on first PUT (the S3 API does not auto-create buckets).
- **Pinned to a non-storage, non-platform node** — the durable copy
  deliberately lives in a different failure domain than both the data and the
  platform stack (`docs/storage-and-databases.md:260-268`).
- Credentials come from the `seaweedfs_admin` managed credential (S3 identity
  via env, auto-promoted to admin; no config file, no bootstrap).
- The daemon indexes the store (S3 ListObjectsV2, `backup-` prefix,
  control-plane archives excluded) alongside the local archive dir, so
  `pmcluster backup list` sees archives other nodes uploaded (BUG-025).

### 5.3 The offsite double-write

When `backup_s3_*` are configured, each agent writes **every archive twice**:
to the store via WebDAV and to the offsite target via offen's native S3
backend (`AWS_S3_BUCKET_NAME`, `AWS_S3_BUCKET_LOOKUP: path` — BUG-028 —
`docs/storage-and-databases.md:274-284`). There is **no replicator service**.
The rclone sidecar that used to do this was removed in v0.2.172 after the user
asked:

> why we have rclone/rclone ?
> if seaweed do not do it by default the backup container can already ship the
> backup twice once to s3 and once to internal

### 5.4 The store-transit mover and routed restore

`pmcluster stack move <stack> --to <node>`: fresh backup → transit the stack's
subtree to the target → write `stack_pin_<stack>` → re-deploy. The transit
path on a store-configured cluster is a one-shot Swarm service pinned to the
target that pulls the archive **object** from the store via
`host.docker.internal:8333`, verifies the tarball actually contains the stack's
data, and unpacks `<stack>/` into the target's volume root
(`pmcluster/internal/stacks/move_store.go:90`, `:178`, `:221`). Storage
failover uses the same mover but pulls the *failed node's* newest archive and
never triggers a fresh backup first (BUG-031 fixed a case where it restored
another node's archive — an empty database).

`backup restore` is routed to the **owning node** through the same mover;
when routing is impossible it fails loudly rather than writing into the wrong
disk (BUG-032). `--from-s3` forces the fetch from the offsite bucket.

### 5.5 Disaster recovery

`cluster down --purge` first writes a restorable
`purge-backup-<ts>.tar.gz` (data.db + .encryption_key + config.yaml) and keeps
it; `pmcluster cluster reset --restore <archive>` extracts it back before
rebuilding the Swarm side from the DB
(`docs/storage-and-databases.md:317-324`).

## 6. Networking and ingress

Two planes (`docs/network-topology.md:7-18`): the **public** plane (browsers →
Traefik 80/443, DNS) and the **cluster** plane (Raft 2377, gossip 7946, VXLAN
4789, daemon 9090, store 8333) which must never be reachable from the
internet.

**Traefik is a `mode: global` service constrained to `node.role == manager`**
(`pmcluster/internal/cluster/embeds/infra-stack.yml:23-32`). This is a fix for
a real production outage, not a preference: Traefik's Swarm provider lists
services through the local Docker socket, which only a manager can read; a
global replica that landed on a worker had **no routers at all**, and the
ingress routing mesh intermittently forwarded requests to that routerless
replica — `pmcluster.<domain>` 404'd on ~half of requests
(`infra-stack.yml:24-31`, TC7 BUG-014).

The **ingress routing mesh** still fronts the gateway from *every* node:
80/443 are published in `ingress` mode, so any node that receives traffic
forwards it to a Traefik task on a manager. That is why workers can stay in
an LB's backend pool even though they run no Traefik — see DEC-12.

**TLS** is either operator-supplied (`cluster up --cert/--key`, replicated as
versioned Swarm secrets) or Let's Encrypt ACME. Per-host customer certs
(`pmcluster tls hosts`) render into the Traefik dynamic config
(`traefik-dynamic.yml`). ACME behind a load balancer is **not solvable from
inside the cluster** — see Chapter 07.

**The edge.** `pmcluster-edge` (`internal/edgeproxy`) is the only public path
to the daemon: it applies per-IP rate limits (200 req/s on `/api/*` with a
burst of 400; 40 req/s on `/webhook/*`), a 1 MB body cap rejected with 413
(`edgeproxy/shield.go:66`), and an automatic IP ban list
(`edgeproxy/config.go:123-129`). The daemon itself listens on `0.0.0.0:9090`
so the edge container can reach it over `host.docker.internal`; `harden-host.sh`
denies 9090 to the WAN.

**The auth gate.** Two modes: Traefik `basicAuth` (htpasswd) or GitHub SSO via
oauth2-proxy (org/repo-restricted), gating `/web/*` and `observ.<domain>`.
When the gate owns authentication (`edge_login_disabled=true`), the console
runs as a **synthetic admin** in stateless mode (no `pmui-data` volume,
in-memory store, no user rows) — `docs/security-and-improvements.md:174-177`.

## 7. Observability

The `observability` platform stack: a global OTel collector (logs/metrics/
traces from every node and container), OpenObserve behind the same auth gate
at `observ.<domain>`, and an `openobserve-auto-auth` Traefik middleware that
injects the root credential — **only ever** paired with the admin-auth/SSO
gate (`docs/security-and-improvements.md:154-158`). pmcluster exports its own
signals over OTLP: `pmcluster.reconcile.total`, `pmcluster.services.paused`,
`pmcluster.services.stale`, `pmcluster.storage.node.down`,
`pmcluster.storage.failover.disabled`, `pmcluster.events.*`.

Alerting is deliberately delegated: "for alerting we will use openobserv no
need for anything" (user directive, 2026-09-17).

## 8. The DSL and the single render pipeline

```
Parse (strict YAML) → Interpolate (${…}) → Validate → BuildIR → ComposeWriter (v3.9)
```

- `manifest.Parse` (`internal/manifest/parse.go:19`) — unknown keys are
  rejected at every level; errors are path-prefixed.
- `manifest.BuildIR` (`internal/manifest/translate.go:129`) — the neutral IR.
  This is the writer seam: a future Helm/Terraform backend reads the same IR
  (`docs/improvements.md:72-88`).
- `manifest.TranslateIR` (`translate.go:115`) → `ComposeWriter`.
- Platform stacks are the same thing plus a Go-template pre-pass:
  `cluster.LoadComposeFile` (`internal/cluster/templates.go:504`).

**Reference language** (`internal/refs`, a leaf package
`pmcluster/internal/ARCHITECTURE.md:92-97`):

| Form | Meaning |
|---|---|
| `config(name)` | inject the DB config's **content** as an env value |
| `secret(name)` | inject the DB secret's **value** as an env value (env-only consumers, e.g. OpenObserve's `ZO_ROOT_USER_PASSWORD`) |
| `secrets(name)` | resolve to the mount path `/run/secrets/<name>` (must also be listed in `secrets:`) |
| `settings(name)` | inject a cluster setting's value (e.g. `volume_root`) |
| `config_path(name)` | mount a DB config as a file, referencing the content-addressed Swarm config `<name>_<sha8>` automatically |

There is no inline/substring interpolation inside a larger env value
(`docs/dsl.md:130-161`).

**Content addressing.** `EnsureVersionedSecret` / `EnsureConfig`
(`internal/cluster/secrets.go:105`, `:216`) name objects
`<baseName>_<sha8-of-data>`; identical content reuses the object, a change
mints a new one and GCs the old. Translate reads values swarm-first and
**rebuilds a missing object from the DB**; `cluster update` runs a
rebuild-on-missing repair pass; `pmcluster secret heal` verifies and
re-materialises every secret (v0.2.166).

**Escaping.** Every user-derived string in a rendered compose has its literal
`$` doubled to `$$` (`manifest.escapeCompose`,
`docs/dsl.md:321`) — otherwise `docker stack deploy`'s compose interpolation
silently truncates `p@ss$word` to `p@ss` (review finding H8, fixed v0.2.176).

**`depends_on`** is emitted for compose parity but enforced **in the control
plane**: the deploy pipeline topologically sorts services into levels and
deploys level by level, waiting for health between levels, then runs one
drift-prune pass with the full stack compose
(`docs/dsl.md:273-280`). This works for any image — no shell wrapper is
injected (the v0.2.110–112 wrapper approach is superseded).

## 9. Security model

| Layer | Mechanism | Cite |
|---|---|---|
| API tokens | Argon2id (`t=2, m=64MiB`), `pmc_<token_id>_<secret>` v2 format with indexed lookup; garbage bearers fast-rejected **before** any DB work | `internal/store/users.go:113-132` |
| Credential storage | AES-256-GCM in SQLite, key at `~/.pmcluster/.encryption_key` (0600), mirrored to content-addressed Swarm secrets | `docs/security-and-improvements.md:15` |
| Webhooks | HMAC-SHA256 over `timestamp‖body` (no separator), ±5 min window, constant-time compare, uniform 401, mandatory `repo_url`+`file` provenance | `docs/webhook.md:102-122` |
| Daemon RBAC | Bearer tiers admin > operator > viewer (migration 0025); rendered configs require operator; secret-ish settings masked for non-admins | `docs/security-and-improvements.md:179-183` |
| Console RBAC | Session roles admin > operator > viewer; synthetic-admin mode behind the gate | `docs/security-and-improvements.md:19` |
| CSRF | Origin/Referer host must match on state-changing POSTs; cookieless POSTs (curl/CLI) still allowed | `internal/ui/ui.go:132-136` |
| RealIP | `trustedRealIP` honours XFF/X-Real-IP only from trusted CIDRs | `internal/server/server.go:116`, `:278-281` |
| Sessions | Random persisted 32-byte session secret (never the built-in default), `Secure` flag on HTTPS, bcrypt >72 bytes rejected | `docs/security-and-improvements.md:24` |
| Edge | Rate limits + bans + 413 body caps | `internal/edgeproxy/` |
| Platform flag | `platform: true` refused in user manifests — a customer stack can never masquerade as a platform stack | `docs/dsl.md:45` |

## 10. Package map (a reading order)

| Question | Read |
|---|---|
| What does the daemon do on startup / promotion? | `internal/cli/serve.go`, `internal/cli/leader.go` |
| How does desired state become Swarm state? | `internal/manifest/` → `internal/stacks/deploy.go` |
| How is drift detected and repaired? | `internal/reconcile/`, `internal/stackdrift/` |
| Where does data live and who decides? | `internal/stacks/placement.go`, `internal/cluster/` |
| How is it backed up and moved? | `internal/backups/`, `internal/stacks/move.go`, `move_store.go` |
| How is the control plane survived? | `internal/controlplane/state.go` |
| What does the operator see? | `internal/ui/`, `internal/server/` |

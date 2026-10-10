# Control loop — architecture & extension design

Status: current implementation live (v0.2.132 → v0.2.179); this file describes
the loop as built AND the refactor target that makes it pluggable and able to
push events to an external stream for custom control loops.

## 1. How the loop works today (as-built)

```mermaid
flowchart LR
  subgraph Sources
    DE["Docker engine events<br/>runtime.Client.Events"] --> DEB["2s debounce"]
    TICK["reconcile_interval ticker<br/>(default 60s, 0=off)"] --> DEB
  end
  DEB -->|"trigger"| LOOP["Reconciler.Loop<br/>(leader only)"]
  LOOP -->|"TryLock — one pass at a time"| RUN["Reconciler.RunOnce"]
  RUN --> P1["Pass (a) platform reconcile"]
  RUN --> P2["Pass (b) app-stack drift sync"]
  RUN --> P3["Pass (c) health snapshot"]
  P1 -->|"re-render platform configs + hash compare<br/>+ live rendered-hash label check"| UPD["cluster.Update (content-aware)"]
  P2 -->|"re-translate latest source_yaml<br/>+ live drift check"| SYNC["stacks.Sync (no-op on hash match)"]
  P2 -->|"pinned storage node down"| PAUSE["pause sync (+ optional<br/>storage_failover auto-move)"]
  P3 -->|"derive healthy/in-progress/degraded/error"| SS[("stack_status DB")]
  SS --> BADGE["/api/public/badge/*<br/>(reads DB, never live Docker)"]
  RUN --> OBS["metrics pmcluster.reconcile.total<br/>traces pmcluster.reconcile / .platform<br/>info+debug logs"]
```

Key properties:

- **Leader-only.** `WatchSwarmLeadership` channel starts/stops the loop. Workers
  and standby managers never run it — a worker daemon logs "standing by (no
  control loop)" once and stays idle instead of re-running the doomed
  manager-only reconcile.
- **Event + tick.** Docker events arm a 2s debounce; the interval tick is the
  safety net (a closed event stream disables that case and falls back to the
  ticker). One pass at a time (TryLock skips).
- **Three passes per run.** (a) platform configs re-rendered + hash-compared +
  drifted stacks redeployed (no TLS/credential churn; `cluster.Update` is also
  existence-aware — a stack missing from the swarm is redeployed even on a
  matching hash); (b) each app stack's latest source re-translated and synced
  when the rendered hash drifted; (c) per-stack + per-service health written to
  `stack_status`, stale rows pruned.
- **Two drift signals.** The *stored* rendered hash (what pmcluster last
  intended) is compared against a fresh render — and when they match, the *live*
  Swarm is still checked via the `io.pmcluster.rendered_hash` label every
  deployed service carries: a manual `docker service update`, a half-applied
  deploy or a wiped service re-applies even though the stored hash is unchanged.
  The revision's rendered hash is stamped **only after a successful apply**, so
  a failed deploy always looks drifted to the next pass and is retried.
- **Storage-node outage pause.** When a stack's pinned storage node is down
  (absent, not `ready`, or not `active`), pass (b) skips that stack — deploying
  onto a node that cannot serve the data would recreate it against empty binds.
  The pause clears automatically when the node returns or the stack is moved.
  With `storage_failover=true` (and offsite backups configured) the loop instead
  picks a healthy alternate storage node and moves the stack there (S3/store
  restore, 5-minute per-stack cooldown, detached from the pass) — see
  [storage-and-databases.md](storage-and-databases.md).
- **Reports, never acts on health.** No auto-restart/force-update; crash-loop
  retry is Swarm's restart-policy job. The only actions the loop takes are
  drift re-applies and the opt-in storage failover.
- **Throttled failure logging.** A permanently broken stack (a sync that fails
  every pass) is logged and recorded **once per distinct error string** (plus a
  once-per-hour WRN heartbeat) and its status is marked `error` with the parse
  error; the same throttle applies to the "storage node down" pause warning
  (once per state change + hourly heartbeat).
- **Badges read the DB snapshot**, not live Docker — no flicker, no per-request
  swarm queries. An unacknowledged storage-failover marker dominates the badge
  ("failover", amber) until an operator `stack ack`s it or moves it back.

### Parallel leader loop: control-plane Raft snapshot (v0.2.138)

Alongside the reconciler, the leader runs `controlplane.Kit.Loop` (5-minute tick,
plus an immediate snapshot on promotion). It re-publishes the failover-survivor
kit (data.db, config.yaml, config/ tree — and the encryption key in a separate
config) into `pmcluster_state_*` / `pmcluster_state_key_*` Docker configs, which
Swarm's Raft store replicates to every manager. The snapshot only fires when a
kit file changed since the newest config (deploys, settings, secret/webhook
edits all land within one interval), prunes to the newest two, and fails loudly
when a payload would exceed Docker's 500 KiB config ceiling. It is independent
of `reconcile_interval` — a cluster with the reconcile loop disabled still keeps
its control plane replicated. See `internal/controlplane`.

**Restore is startup-only.** A promoted daemon never restores under its own
live-opened SQLite connection (Restore truncates data.db — the daemon would
serve stale page-cache rows while fresh readers see the clobbered file: a
split-brain). Instead, `serve` restores from the newest `pmcluster_state_*`
config **before the store is opened**, only when the local DB is missing or
older than the snapshot (a current DB — e.g. on shared storage — is never
clobbered), and repairs a missing `.encryption_key` even when the DB is
current. On promotion the leader only *publishes* a fresh snapshot. A worker
node or a leaderless swarm (quorum lost) degrades gracefully: snapshot/restore
become no-ops that retry later instead of crash-looping the daemon.

## 2. Extension goal

Two asks:

1. **Extendable passes** — adding a new reconcile behavior (health-gated
   rollback, alerting policy, registry GC...) must not touch the loop's core.
2. **External event stream** — everything the loop observes must be publishable
   to a separate stream (NATS / webhook / queryable log) so a third party can
   build their own control loop on top of pmcluster.

## 3. Target architecture

```mermaid
flowchart LR
  subgraph Sources
    DE["Docker events"] --> BUS["events.Bus"]
    TICK["ticker"] --> BUS
    MAN["manual: cluster reconcile / API"] --> BUS
  end
  BUS -->|"publish"| RE["Reconciler<br/>(leader only)"]
  RE -->|"RunOnce"| REG["Pass registry<br/>passes ordered by priority"]
  REG --> P1["platform pass"]
  REG --> P2["app-sync pass"]
  REG --> P3["health-snapshot pass"]
  REG --> PF["future passes (pluggable)"]
  BUS -->|"fan-out"| SINK["events.Sink(s)"]
  SINK --> NATS["NATS JetStream<br/>pmcluster.events.*"]
  SINK --> WH["webhook POST (optional)"]
  SINK --> LOG[("events table<br/>GET /api/events")]
  P1 --> UPD["cluster.Update"]
  P2 --> SYNC["stacks.Sync"]
  P3 --> SS[("stack_status DB")]
  SS --> BADGE["/api/public/badge/*"]
  RE --> OBS["metrics / traces / logs"]
```

### 3.1 Pluggable passes

```go
// internal/reconcile (refactor target)
type PassContext struct {
    Store        *store.Store
    Docker       runtime.Client
    DeployService *stacks.Service
    Services     services.Service
    Update       func(ctx context.Context, deps cluster.UpdateDeps, in cluster.UpdateInput) (*cluster.UpdateResult, error)
    UpdateDeps   cluster.UpdateDeps
    UpdateInput  cluster.UpdateInput
    Log          zerolog.Logger
}

type Pass interface {
    Name() string                       // "platform" | "app_sync" | "health_snapshot" | ...
    Priority() int                      // ordering within a run
    Run(ctx context.Context, pctx *PassContext) error
}
```

- `Reconciler` becomes a thin runner: build `PassContext` → sort passes by
  priority → run each inside the run span → record per-pass outcome.
- Existing three passes extracted as-is; new policies are one new file.
- Trace structure becomes `pmcluster.reconcile` root + `pmcluster.reconcile.<pass>`
  child per registered pass (replaces the hard-coded platform child span).

### 3.2 Event stream

```go
// internal/events (new package)
type Event = runtime.Event // reuse the neutral model

type Sink interface {
    Name() string
    Publish(ctx context.Context, e Event) error
}

type Bus struct {
    mu      sync.Mutex
    subs    []chan Event   // in-process subscribers (Reconciler, sinks)
    sinks   []Sink         // external delivery
    log     zerolog.Logger
}
func (b *Bus) Publish(e Event)          // non-blocking fan-out
func (b *Bus) Subscribe() <-chan Event  // buffered in-process channel
func (b *Bus) AddSink(s Sink)
```

Sink implementations (selected by `events_sink` setting):

| Sink | Transport | Config |
|---|---|---|
| `none` (default) | — | behavior identical to today |
| `db` | `events` table (migration) + `GET /api/events?since=` (public/Bearer) | `events_sink=db` |
| `webhook` | HTTP POST JSON `{type,action,stack,service,node,timestamp}` | `events_sink=webhook`, `events_url`, optional HMAC via `events_secret` |
| `nats` | JetStream subject `pmcluster.events.<stack>` (durable consumer-ready) | `events_sink=nats`, `events_nats_url`, `events_nats_subject` |

- `runtime.Client.Events` feeds the Bus; the Reconciler subscribes via
  `Bus.Subscribe()` (so custom loops could subscribe too without a daemon restart).
- Publishing is non-blocking + drop-on-full-buffer (control loop must never be
  slowed by a slow sink); each sink gets its own bounded buffer + retry.
- Sink errors are logged (debug) and telemetry-counted
  (`pmcluster.events.published{source,sink}` / `pmcluster.events.dropped`).

### 3.3 Wiring (serve.go)

- Build `events.NewBus(log)`; wire `docker.Events` stream into it.
- Reconciler gets `Bus` (replaces direct `Docker.Events` read) + pass list.
- On leader promotion: start loop (subscribes to Bus) + start configured sinks;
  on loss: stop both.

## 4. Migration path (no behavior change until a sink is configured)

1. Extract `internal/events` (Bus + Sink + db/webhook/nats sinks, `events_sink`
   setting + allowlist).
2. Refactor Reconciler onto the Pass interface (same three passes, same order,
   same logging/metrics/traces — golden tests stay green).
3. Route loop triggers through the Bus (in-process subscribe replaces direct
   Docker.Events read).
4. Wire sinks in serve.go behind the setting.
5. Docs: this file + CHANGELOG.

## 5. What stays the same

- Leader-only execution, one-pass-at-a-time, event+tick triggers, no
  auto-restart semantics (the one deliberate exception: the opt-in
  `storage_failover` move), DB-driven badges, throttled error history,
  deploy pipeline untouched (the loop only ever re-applies stored desired
  state through it).
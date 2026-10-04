# Changelog

Release history for **poor-man-cluster**. The RFC and the reference docs describe the
*current* state of the project; this file is the only place that tracks what changed
and when.

## v0.2.146 (2026-10-04)

- **fix(cli): secret rotation now works for live stacks (BUG-007).** Docker Swarm
  secrets are immutable and cannot be removed while a running service references
  them, so `pmcluster secret edit` silently updated only the DB — redeployed
  containers kept the old value with just a WARN line. Secrets now carry a
  `swarm_rev` counter (migration 0023); every edit bumps the revision and the CLI
  mirrors the new value into a **versioned swarm secret** `name_v<rev>`, leaving
  the in-use original untouched. The compose writer emits the versioned external
  name via a `SecretNames` resolver while the container mount path stays
  `/run/secrets/<logical-name>`. Verified empirically that `docker stack deploy`
  honors the `name:` override on external secrets.
- **fix(webhooks): retry policy is now live on the receive path (BUG-008).** The
  `MaxRetries`/`RetryDelay` wiring was dead code — `receiver.go` called
  `DeployAsync` directly and every delivery row recorded `retries:0`. The receiver
  now goes through `DeployAsyncWithRetries` under a phase budget; synchronous
  deploy failures are retried (2 retries, 30s apart) and the actual retry count is
  recorded on the delivery row and in the 502 body. Background (async) apply
  failures remain fire-and-forget and are surfaced via stack `last_error` and the
  reconcile loop, as before.
- **test:** store swarm_rev bump + `SwarmSecretName` cases, compose
  `name:` override rendering, receiver retry counts (502 body + delivery row),
  `DeployAsyncWithRetries` sync-phase retries.

## v0.2.145 (2026-10-04)

- **fix(cli): control-plane restore is startup-only — the leadership goroutine no
  longer restores over the live store (split-brain data-loss risk).** Found during
  Test Case 2 live testing right after the v0.2.144 WAL fix. On every Swarm
  leadership gain the daemon re-ran `ensureControlPlaneFresh` *after* `store.Open`,
  so `Restore` truncated `data.db` under the daemon's live SQLite connection:
  the daemon kept serving stale page-cache rows while fresh readers (CLI,
  `python3`) saw the clobbered file — the same split-brain symptom class as the
  WAL bug. Reproduction: leader daemon running → create a user via the CLI →
  restart the daemon → the user survives auth (stale cache) but is gone for fresh
  readers. **Fix:** restore now happens only at startup, before the store is
  opened (the standby/failover path). On leadership gain the daemon only
  *snapshots* the fresh control plane into the Raft-replicated configs. Verified
  live on the test node: CLI-created users now survive daemon restarts with full
  cross-process consistency (`integrity_check` = ok, all readers agree).

## v0.2.144 (2026-10-04)

- **fix(store): eliminate the multi-process WAL corruption bug (data-loss risk).**
  Found during Test Case 2 webhook/CLI feature testing. The control-plane DB was
  opened in `journal_mode=WAL` by both the long-running daemon and short-lived
  CLI processes (and any other reader, e.g. `python3`). When a second process
  wrote and closed its connection, SQLite deleted the `-wal`/`-shm` sidecars
  even though the daemon still held them open. The daemon then kept writing to
  the orphaned inode, producing a split-brain database: fresh CLI-created rows
  (users, tokens, settings) were invisible to the daemon (all auth 401s), index
  corruption appeared (`wrong # of entries in index idx_backups_started`), and
  `user list` failed with `database disk image is malformed`. Reproduction:
  open the store, have a second connection write+close, watch `data.db-wal`
  disappear while the first handle keeps it open. **Fix:** both stores (daemon
  `internal/store` and console `internal/ui/store`) now use `journal_mode=DELETE`
  — transactions commit directly to the main file, there are no long-lived
  sidecars to orphan, and a writer's close can never detach the DB from an open
  handle. `Store.WALCheckpoint` is now a no-op (nothing to flush). Regression
  test `TestMultiProcessNoWALSplitBrain` covers open/write/close/reopen
  cross-visibility and asserts no `-wal`/`-shm`/`-journal` sidecars ever exist.

## v0.2.143 (2026-10-04)

- **fix(cluster): `cluster update` now ensures the storage root directories exist.**
  Found during Test Case 2 (custom `volume_root`). `ensureStorageDirs` was only
  called from `cluster up` — changing `volume_root` after bring-up left the new
  root missing, so `backup_volume-backup` tasks were rejected with
  `bind source path does not exist`. `update.go` gained an explicit
  *"Ensuring storage root directories exist"* step.
- **fix(backup): `backup restore` default `--dest-root` now follows the cluster's
  `volume_root` setting** instead of the hardcoded `/var/stack/data`. Both the
  CLI (`restoreDestRootDefault`) and the REST API (`backups.HTTP.VolumeRoot`,
  wired from the store) resolve the setting, falling back to
  `manifest.DefaultVolumeRoot` when unset. BUG-002/BUG-003 from
  `docs/test-reports/TC2-backup-restore-s3-acme.md`.
- **docs: Test Case 2 report** — single-manager, Let's Encrypt ACME certs (all 3
  domains, Verify code 0), custom volume root `/srv/stack/data`, S3 upload +
  local/S3 restore round-trips proven (`docs/test-reports/TC2-backup-restore-s3-acme.md`).

## v0.2.142 (2026-10-04)

- **fix(cli): `WatchSwarmLeadership` re-emitted `leader-true` on every 15s poll.**
  Found during Test Case 1 on a live single-manager cluster. After emitting
  `leader=true` the function reset `current=false`, so every poll re-emitted
  `true` forever. The `serve` leadership goroutine reacted by cancelling and
  restarting the reconciler loop **and the control-plane snapshot loop every
  15s** — meaning `pmcluster_state_*` Raft snapshots silently never ran after
  startup (a data-loss window if the manager disk dies), plus repeated
  `local control-plane DB is current` restore spam.
  - Fix: the leader branch now sets `current, first = true, false` so a
    still-leader poll stays silent; the err/standalone branch was already correct.
  - Tests: `TestWatchSwarmLeadership_StableLeaderEmitsOnce` (one emit then
    silence over `2*leaderPollInterval`; fails without the fix) and
    `TestWatchSwarmLeadership_LeaderToNotLeaderReEmits` (race-safe leadership
    flip → `false` emit after the flip).
  - Verified live on `nxt-sw-3-w`: exactly one `became swarm leader` emit after
    restart and a fresh snapshot at the next 5-min tick.
- **Docs: first test report** `docs/test-reports/TC1-single-manager-bringup.md`
  (single-manager setup, multi-service deploy, logs/metrics, UI, control plane).

## v0.2.141 (2026-10-03)

- **Interactive `docker exec -it` over websocket (L3) — browser terminals.**
  The console now ships a real terminal into any service container:
  - **Daemon endpoint** `GET /api/services/{stack}/{service}/exec/ws`
    (`internal/server/execws.go`): Bearer-auth + stack-scope guard, runs an
    interactive TTY exec (`ContainerExecCreate{Tty}` + hijack) against the
    service's running task, and pumps frames — binary in = stdin, binary out
    = raw TTY output, `{"type":"resize",rows,cols}` text controls, and a
    final `{"type":"exit","code":N}` frame. `?cmd=` picks the program
    (default `sh`); `?rows=`/`?cols=` seed the size. Dispatched **outside**
    the `otelhttp`/status-span wrappers (neither implements
    `http.Hijacker`).
  - **New runtime port** `runtime.ServiceExecAttach` +
    `runtime.ExecStream` (Read/Write/Resize/Wait with live exit-code
    polling); Docker adapter does the exec hijack; `services.ExecAttacher`
    optional interface so the remote adapter is untouched.
  - **Console**: `GET /web/stacks/{name}/terminal?service=X` standalone
    xterm.js page (vendored, no CDN) and `.../terminal/ws` relay that
    bridges the browser connection to the daemon with the server-side API
    token (the browser never sees it). Operator role only. A **Terminal**
    button on every service row in the stack detail + services tables
    (Arabic labels included).
  - E2E-verified style tests: docker TTY attach (raw hijack), daemon WS
    handler (auth + frame pump + resize + exit frame), console relay round
    trip (browser→console→daemon→console→browser), page render. **Bug found
    by tests:** `execStream.done` channel was never initialized → Close
    panic; fixed.

## v0.2.140 (2026-10-03)

- **`pmcluster join --storage-node` — mark a joining node as a storage node.**
  New opt-in flag on `join`: after joining the Swarm, the node's hostname is
  added to the manager's `storage_nodes` cluster setting (reached over ssh), so
  round-robin placement can spread stateful stacks onto it too. **Workers
  qualify — storage is not leader-only.** Best-effort: if the ssh registration
  fails the join still succeeds and prints the manual
  `pmcluster cluster settings set storage_nodes=...` command to run instead.
- **The main node is a storage node by default.** `cluster up` now writes the
  leader's hostname into `storage_nodes` when the setting is empty, so a
  fresh cluster pins every stateful stack to the main node from day one.
  An operator-configured list is never clobbered.
- **Every service carries an `io.pmcluster.node` label** naming the node its
  placement pin targets (a resolved storage node / hostname pin; role-based and
  unconstrained services carry none). The console **Services** table, the
  **stack detail** services panel, the service card, and `pmcluster service ps`
  all display this node column — you can see where every stateful service runs
  at a glance (UI labels in English + Arabic).
- **Live `storage_nodes` refresh.** The daemon's settings hook hot-swaps the
  storage-node list when `storage_nodes` changes (no restart); the next
  reconcile re-renders and redeploys affected stacks.

## v0.2.139.1 (2026-10-03)

- **Fix: worker-node daemon crash at startup (control-plane restore).** The L2
  config-based restore passed the Docker client into the startup
  `ensureControlPlaneFresh` check; on a worker node Docker answers
  "This node is not a swarm manager", which is not the sentinel error the code
  expected, so the daemon treated it as fatal and exited (systemd FAILURE).
  `internal/controlplane` now detects the not-a-swarm-manager error and falls
  back to the tarball path (`Restore` → `ErrNoSnapshots`, `Snapshot` → no-op),
  so worker daemons start cleanly again. Deployed to the nextrum-sy-2 worker.

## v0.2.139 (2026-10-03)

- **`storage_nodes` setting + round-robin placement for stateful stacks (P3).**
  New cluster setting `storage_nodes` (comma-separated hostnames, e.g. via
  `pmcluster cluster settings set storage_nodes=node-a,node-b`). Stateful services
  (volume holders) with **no explicit placement** are now pinned deterministically
  across the listed nodes — FNV-1a of the stack name picks the node, so the same
  stack always lands on the same node and adding stacks never reshuffles existing
  ones. Explicit `placement:` (hostname / `manager` / `worker`) always wins; the
  `platform_node` setting remains the fallback when `storage_nodes` is empty.
  A change to `storage_nodes` changes the rendered hash, so the reconcile loop
  re-deploys affected stacks on the next pass.
- **Storage-outage pause in the control loop (P4).** The reconcile loop now builds
  a live node-health map (swarm `NodeList`: present, `Status==ready`,
  `Availability==active`) each pass and **skips syncing/deploying any stack whose
  storage pin is on a down node**, logging a warning ("storage node down; pausing
  app sync"). The pause clears automatically when the node returns — or when the
  stack is moved. This stops the loop from churning against a node that cannot
  serve its volumes.
- **`pmcluster stack move <stack> --to <node>` (P5).** Moves a stateful stack's
  storage to another node: triggers a whole-disk on-demand backup, restores the
  stack's `<stack>/` subtree on the target (locally when the target is this host,
  else via a one-shot swarm mover service pinned to the target that wget's the
  archive from an ephemeral HTTP server on the leader), writes the per-stack
  `stack_pin_<stack>` setting, and re-deploys so the new pin is rendered. The
  per-stack pin **outranks** the `storage_nodes` round-robin, so the reconcile
  loop never moves the stack back. Errors loudly on: unknown stack, no stateful
  storage, empty `--to`, missing backup trigger, a target that is absent or not
  ready/active in the swarm.
- **Path 1 is the HA answer (docs decision).** Replaced the "LINSTOR is the HA
  prerequisite" design directive with the Path 1 decision: pinned placement +
  backup/restore is the HA story (storage_nodes round-robin, outage pause,
  `stack move`), and LINSTOR/DRBD block replication is explicitly off the roadmap
  with its honest cost table preserved. Removed the L0 LINSTOR backlog item.

## v0.2.138 (2026-10-03)

- **Control-plane survivor kit via Raft-replicated Docker configs (L2).** The swarm
  leader now snapshots the failover-survivor kit (data.db with credential ciphertexts,
  settings, webhook secrets, token hashes, stack revisions; config.yaml; the rendered
  `config/` tree with TLS certs) into `pmcluster_state_<unixnano>` Docker configs. Swarm
  replicates configs to every manager through its own Raft store, so a promoted standby
  gets the latest control plane **without the tarball/rsync dependency**. **Security
  split:** the AES-GCM encryption key is written to a second config family
  (`pmcluster_state_key_<unixnano>`) so key and ciphertext never share one blob — a
  restore refuses a state config whose key config is missing, and the key never appears
  inside the data-kit payload.
  - **Freshness:** on `serve` startup and on leadership gain,
    `ensureControlPlaneFresh` restores from the newest state config when the local
    data.db is missing or older than the snapshot (and repairs a missing encryption key
    even when the DB is current). A local DB at least as fresh as the snapshot (the
    shared-storage/LINSTOR case) is never clobbered. Clusters without state configs fall
    back to the existing tarball archives.
  - **Snapshot cadence:** the leader publishes a snapshot immediately on promotion and
    re-checks every 5 minutes, re-snapshoting only when a kit file changed — so a deploy,
    settings change, secret rotation or webhook edit lands on all managers within one
    interval. Snapshots are pruned to the newest two per family.
  - **Guards:** payloads over the Docker config ceiling (500 KiB) fail loudly instead of
    half-writing; snapshots skip `logs/`; restored data.db is timestamped at the snapshot
    so repeated promotions never re-restore. `cluster down --purge` wipes the state
    configs with the rest of the managed footprint (the local SQLite survives by design,
    and the next leader re-snapshots it).
  - **Backup stack honors a custom storage root.** The embedded backup stack previously
    hardcoded `/var/stack/data` and `/var/stack/backup` as bind sources, so any
    non-default `volume_root` setting broke the volume/control-plane backup agents
    ("bind source path does not exist"). The compose template now renders `[[.VolumeRoot]]`
    and `[[.BackupDir]]` from the effective `volume_root` setting and the backup dir
    (default `/var/stack/backup`, overridable via the `PMCLUSTER_BACKUP_DIR` env var for
    hermetic/dev hosts that cannot create `/var/stack`).
  - **Snapshot interval overridable.** `PMCLUSTER_CONTROLPLANE_SNAPSHOT_INTERVAL` (seconds,
    default 300) tunes how often the leader re-checks the kit for changes — used by the
    e2e suite to exercise the snapshot loop at 1s.
  - **E2E verification.** New swarm-gated `TestControlPlaneSnapshotE2E` drives the whole
    loop against a live Swarm (Docker Desktop): both config families appear, the
    security split holds (data.db present, no `.encryption_key` entry, 32-byte key
    absent from the state payload), a store mutation is captured within one interval, and
    prune settles each family to at most two snapshots.

## v0.2.137 (2026-10-02)

- **Edge console is stateless in cluster mode (Path 1, P1).** With
  `EDGE_LOGIN_DISABLED=true` (the normal Swarm deployment, gated by Traefik
  admin-auth/sso-auth) the edge console now runs **completely without the
  `edge_pmui-data` volume**: its API link + token come from env vars
  (`PMCLUSTER_API_URL` / `PMCLUSTER_API_TOKEN`), the SQLite store opens
  in-memory (`:memory:`), the session is the synthetic admin, and nothing is
  persisted or written back. Standalone runs (login enabled) keep the local
  SQLite config file exactly as before. The edge stack template drops the
  volume declaration and mount when login is disabled. This removes one more
  stateful component from the cluster plane.

- **Tailscale option (M3).** `pmcluster join`, `pmcluster cluster up` and
  `pmcluster setup` gained opt-in `--tailscale` (+ `--tailscale-auth-key`, default
  `$PMCLUSTER_TAILSCALE_AUTH_KEY`). When set, the node is brought onto a private
  WireGuard tailnet via the `tailscale` CLI (`tailscale up` then `tailscale ip -4`)
  and the Swarm join/init advertises the **tailnet IPv4** — so node-to-node swarm
  traffic (2377/7946/4789 + any storage ports) needs no firewall rules between
  nodes. Explicit opt-in fails loudly on tailnet errors (a silent fallback to the
  public IP would defeat the purpose). No change when the flag is absent.

## v0.2.136 (2026-10-01)

- **Backup restore completion (M2).** `pmcluster backup restore <id>` gained two knobs and a resilient source story:
  - `--volume <name>` (and `volume` on `POST /api/backups/{id}/restore`) restores only one volume — matched as a full path segment inside the archive, so a per-volume restore never leaks entries from other volumes or stacks. The console's Restore form gained the volume field.
  - Restores are **local-first**: an archive still on the node's archive dir is restored from disk. When it is gone (pruned, or the node never held it) the archive is **fetched from the configured offsite S3 store** (`backup_s3_*` settings) — implemented with a minimal AWS SigV4 GET client (stdlib, path-style, R2-compatible; no new dependency). `--from-s3` forces the fetch even when a local copy exists. With no local archive and no S3 configured, the command fails loudly and says exactly where the archive lives (and that configuring `backup_s3_*` enables the fetch) instead of silently restoring nothing.
  - Refused control-plane archives, the `/backup/data` prefix strip and the path-escape guard all still apply to whichever source the bytes came from.

## v0.2.135 (2026-10-01)

- **Control-loop OTLP traces.** The reconcile loop now emits OpenTelemetry spans through the existing telemetry pipeline (visible in OpenObserve once the collector is connected): a `pmcluster.reconcile` root span per pass with the `run_id` attribute, and a `pmcluster.reconcile.platform` child span for the platform-render/hash-compare pass carrying `stacks_redeployed` when drift is applied. Errors are recorded on the span with a non-OK status. Span creation is lazy and no-op until `telemetry.Init` wires the global provider (same pattern as the deploy/rollback tracers).

## v0.2.134 (2026-10-01)

- **Control-loop logging.** The reconcile loop now logs `info` for its lifecycle (pass started/completed, skipped-when-in-flight) and every actionable result (drifted app stack synced with its new revision, platform stacks redeployed), and `debug` for the detail (per-service and per-stack status derivation with replicas/desired/update-state/run-once, each Swarm event that triggers a pass, debounce/safety-tick arming, stale-row pruning, and no-op "converged" syncs). `info` stays clean for day-to-day logs; `debug` shows exactly what the loop is doing.

## v0.2.133 (2026-10-01)

- **Swarm e2e for the control loop.** `TestControlLoopE2E` (PMCLUSTER_E2E_SWARM=1) drives a real single-node cluster through the loop: cluster up → `reconcile_interval=1` → serve → deploy a stack referencing a config → badge reads the DB snapshot and flips to `healthy` → edit the config (drift with no deploy trigger) → the loop auto-syncs it (revision advances) → scale the service to 0 → the badge flips to `degraded` → teardown. Covers loop snapshot writes, drift convergence, DB-driven badges, and non-restart semantics.

## v0.2.132 (2026-10-01)

- **Control loop (L1) — leader-only converge loop.** The Swarm-leader daemon now runs a reconcile loop (event-driven with a `reconcile_interval` safety tick, default 60s, `0` disables). Each pass: (a) re-renders the platform configs and re-deploys drifted stacks (the content-aware reconcile, no TLS/credential churn), (b) re-translates each app stack's latest source and syncs when the rendered hash drifted (no-op when unchanged), (c) writes a per-stack + per-service health snapshot to the new `stack_status` table. One pass at a time; the loop never restarts services — it only reports health.
- **Badges now read the DB snapshot, never live Docker.** `/api/public/badge/*` reads the `stack_status` snapshot the loop writes (healthy / in progress / degraded / error / unknown), so a single badge fetch no longer queries the swarm and the badge reflects converged state rather than per-request flicker. Unknown when no snapshot exists yet.
- **Leader channel.** `WatchSwarmLeadership` streams leadership changes; the daemon starts the reconcile loop and re-runs the control-plane restore check on promotion, stops the loop on leadership loss. The loop runs only on the leader node.
- **Docker events stream.** `runtime.Client.Events` subscribes to the engine's event stream (neutral `Event` type: type/action/stack/service/node/timestamp) — the loop's primary trigger, with the interval tick as the safety net.
- Migration `0022_stack_status.sql` (stack_status table: stack_name PK, status, services JSON, updated_at).

## v0.2.131 (2026-10-01)

- **Badge endpoints answer HEAD too.** The public badge routes only registered `GET`; chi fell HEAD requests through to the Bearer-protected `/api` group, which answered `401` — and GitHub's camo image proxy (and some image tools) preflight with HEAD, so the badge was refused and rendered broken in READMEs. All three badge routes (`/api/public/badge/{stack}`, `.../services`, `.../{service}`) now register both `GET` and `HEAD` with the same handler. Regression test added.

## v0.2.130 (2026-10-01)

- **Badge status word: `deployed` → `healthy`.** A service can be unhealthy *after* it deploys (crash-loop, a bug) — so the green state now reads **`healthy`**, not `deployed`, across the stack badge, the per-service badge, the combined `/services` badge, the CLI help, and the console copy text. Semantics unchanged: green = deployed *and* healthy; `in progress` / `degraded` / `error` / `unknown` as before.

## v0.2.129 (2026-10-01)

- **Combined stack + services badge.** `GET /api/public/badge/{stack}/services` returns ONE wide SVG showing the stack's main health first, then a segment per service (label = service name without the `stack_` prefix). Same status colors + no-auth + 60s cache. `pmcluster stack badge <stack> --services` prints the markdown; the console stack detail card gained a second copyable row for it. (Service-level badges from v0.2.128 ride along — both shipped in this release's binary.)

## v0.2.128 (2026-10-01)

- **Service-level status badges.** `GET /api/public/badge/{stack}/{service}` returns the same flat SVG scoped to one service (label `stack/service`): `in progress` while updating, `error` when paused (excluding completed one-shot jobs), `deployed`/`degraded`/`unknown`. Same no-auth + 60s cache as the stack badge.
- **Completed one-shot jobs no longer flag `error`.** The stack badge treated any service with a stale `update_state=paused` as failed — but Swarm leaves that marker behind when a `run_once` job finishes. A completed job (run_once, desired>0, 0 replicas) now reads as `deployed`. This is why the donation-campaign **prod** badge showed red despite a clean deploy history.

## v0.2.127 (2026-10-01)

- **Public stack-status badge for READMEs.** New unauthenticated endpoint `GET /api/public/badge/{stack}` returns a shields.io-style flat SVG reflecting the stack's live state: `deployed` (green), `in progress` (yellow — any service mid-update), `degraded` (red — under-replicated, excluding completed one-shot jobs), `error` (dark red — the current revision has a failed deploy outcome in the error history), or `unknown` (grey). Public on purpose: GitHub's README image proxy (camo) fetches it without credentials; cached 60s.
- **`pmcluster stack badge <stack>`** prints the ready-to-paste markdown line (`![<stack> status](https://pmcluster.<domain>/api/public/badge/<stack>)` — remote-mode origin, or the cluster's configured domain).
- **Copy badge from the console**: the stack detail page shows the markdown with a copy button (`data-copy`) when the console knows the cluster domain. EN/AR labels + hint added.

## v0.2.126 (2026-10-01)

- **Errors-only deploy history — no empty writes.** `RecordStackError` now records **failures only** (an empty error writes nothing); a clean deploy leaves the history untouched. The console's "Deploy failed" banner/pill is keyed to the **current revision**: `newestStackError(errors, currentRevision)` shows the newest failed outcome only when it belongs to the current revision — so after a successful redeploy the banner clears (the clean deploy has no new entry) while older failures remain in the history panel for debugging. The revision join (v0.2.125) still surfaces per-execution errors (`""` = applied cleanly, derived from absence in the history). Tests updated for failures-only semantics.

## v0.2.125 (2026-10-01)

- **Deploy outcomes joined to each deployment execution.** The stack-level error history (`stacks.last_error` JSON array, keyed by revision) is now JOINED into the revision views — no extra storage. `GET /api/stacks/{name}` revisions and `GET /api/stacks/{name}/revisions/{rev}` each carry an `error` field (the matching history entry for that revision; `""` = that execution applied cleanly). The console revision timeline shows a "Deploy failed" pill on failed executions and the revision detail (modal) page shows the failure banner — so a stack whose current revision is clean but a previous one failed shows both states side by side. Plumbing: `stacks.Revision.Error` (domain + remote + pmapi), `revRow.Error`/`revisionData.Error` (controllers), templates (`frag_stack.html` timeline pill, `frag_revision.html` banner). Test: `TestRevisionErrorJoin` (list + detail endpoints join per-revision outcomes; no per-revision storage).

## v0.2.124 (2026-10-01)

- **Stack deploy error history (JSON)**: `stacks.last_error` now holds a JSON array of deploy/apply outcomes, newest first, capped at 20 (`store.StackErrorEntry{revision, error, created_at}`, `RecordStackError`/`ListStackErrors`, `ParseStackErrors`). Every deploy/apply outcome — success and failure, sync and fire-and-forget — is prepended; a success entry has an empty `error` so the "Deploy failed" banner clears while the failure history is retained. The console stack detail page gains a **Deploy error history** panel (per-revision entries with rev id + timestamp); the stacks list pill still shows the newest failure. `/api/stacks` `last_error` is now an array. No new table (the v0.2.123 column is reused as the JSON store).

## v0.2.123 (2026-10-01)

- **Failed background deploys surface in the console**: new `stacks.last_error` column (migration 0021) records the most recent deploy/apply error and is cleared on the next successful apply. Set on both sync and fire-and-forget (webhook/API) failure paths, exposed through the daemon API → remote CLI → console as a red detail-page banner and a "Deploy failed" pill on the stacks list.

## v0.2.122 (2026-10-01)
- `stacks.Deployer` gained `DeployAsync`; `remote.Deploy` + `RetryDeployer` implement it. Receiver retry-on-transient-error (Q3, v0.2.113) is superseded — the apply is backgrounded once; `retryDeploy`'s unit tests remain.
- Tests: `DeployAsync` returns early while the apply lands in the background, validation errors never start an apply, receiver 202 + delivery semantics, scope-guard HTTP tables expect 202 on deploys, metric labels reflect acceptance-time recording. `recordingDeployer` fakes are now mutex-guarded.

## v0.2.121 (2026-10-01)

- **Deploy pipeline logs at `info`** — the whole deployment process is now visible at the default log level with the service names but without the aggressive per-level compose YAML: `deploy — starting pipeline`, `deploy — manifest parsed`, `deploy stack — deploying depends_on level`, `deploy stack — waiting for level to become healthy`, `deploy stack — level healthy`, `deploy stack — all levels healthy: one drift-prune pass`, `deploy — completed` (stack/revision/services), plus `rollback — completed` and the `sync — no drift` early-return. The full per-level `compose_yaml` stays `debug`-only.
- **Runtime log-level setting** — new `log_level` cluster setting (debug/info/warn/error). Loggers are built at the minimum level and gated by the zerolog process-global level, so changing the setting applies **live** to the running daemon (console, daily log file, and the OpenObserve OTLP writer). The console's **Settings → Cluster settings** page gained a `Log level` select; the daemon also applies the persisted value at startup. Invalid values are rejected at save time.
- Tests: info-vs-debug separation (YAML never leaks into info), settings log_level apply hook, UI select render + save.

## v0.2.120 (2026-10-01)

- **Ordered-deploy visibility**: the deploy pipeline now emits structured zerolog diagnostics at `debug` level — one record per `depends_on` level with the level index, the service names, and the **full per-level subset compose YAML** (`compose_yaml`), plus single-level full-deploy and final drift-prune records. They reach the CLI console (`PMCLUSTER_LOG_LEVEL=debug pmcluster deploy …`), the daemon's daily log file, and OpenObserve through the existing OTel writer whenever `log_level=debug`; `info` and above stay clean. The earlier plain-text `▶ deploy …` markers are gone. Zero-value logger stays a no-op (tests unaffected).

## v0.2.119 (2026-10-01)

- **`run_once` wait edge case fixed**: a one-shot job that failed an attempt then succeeded on a retry left both `[failed, complete]` task records — the ordered-deploy wait loop previously aborted the deploy on any failed record. The wait now ignores records superseded by newer attempts (any active task keeps waiting), and with no active task the **newest terminal task** decides (ties by start time): `complete` → ready, `failed`/`rejected` → deploy error, `shutdown`/`removed` → "did not complete". Six new wait-path tests.

## v0.2.123 (2026-10-01)

- **Failed background deploys surface in the console**: fire-and-forget deploys (v0.2.122) apply in the background, so the apply error was only visible in the logs. The stack row now records the last deploy/apply error (`stacks.last_error`, migration 0021) — set when a deploy or its background apply fails and cleared on the next successful apply. Exposed as `last_error` in the stacks API, remote CLI and console: the stack detail page shows a "Deploy failed" banner with the error, and the stacks list shows a red pill per failed stack. Covers every trigger (webhook, API, CLI, sync, rollback).

## v0.2.122 (2026-10-01)

- **Fire-and-forget deploys**: `DeployAsync` validates (parse → conflict check → interpolate → validate → translate → record revision) synchronously, then applies the swarm deploy in the background on a detached context. The webhook receiver and `POST /api/stacks` return **202** `{status:accepted, stack, revision}` immediately; validation errors still return 400/502 synchronously; the delivery row records `accepted` at acceptance time. The receiver-side retryer is superseded. Deploy order (`depends_on` levels), wait-for-healthy and drift-prune are unchanged.

## v0.2.121 (2026-10-01)

- **Info-level deploy logs**: the whole deployment pipeline logs at `info` with the service names (start, per-level deploy, waiting, level healthy, prune, completed); the per-level compose YAML stays `debug`-only. Structured zerolog records reach the CLI console, the daemon log file and OpenObserve.
- **Runtime log level**: new `log_level` cluster setting (console Settings → Cluster settings select, or `pmcluster cluster settings set log_level=debug`) applies live to the running daemon via the zerolog process-global — no restart. Loggers are built at the minimum level and gated by the global, so re-level works in both directions.

## v0.2.118 (2026-10-01)

- **`depends_on` now orders the deploy — no more rendered wait wrapper.** Docker Swarm parses and ignores compose `depends_on` (no dependency graph); the previous fix wrapped a dependent service's command/entrypoint in a POSIX `sh` loop (`getent`/`nc -z` probes). That wrapper was a runtime-ordering workaround encoded into a rendered artifact — it broke baked-entrypoint images, distroless images without `sh`, had no timeout, and made port/DNS assumptions. It is **removed**.
- **Control-plane ordered deploy**: the pipeline topologically sorts the stack's services into `depends_on` levels (`manifest.ServiceLevels`, Kahn's algorithm — cycles and unknown dependencies fail loudly before anything is created), renders each level as its own compose (`IR.Subset`), deploys level-by-level via `docker stack deploy` (`DeployStackNoPrune` — partial deploys never drift-prune), waits each level healthy before the next (long-running services until replicas ≥ desired; `run_once` jobs until the task completes — failures stop the deploy with the task error), then runs **one** drift-prune pass (`PruneStack`) with the full stack compose. Single-level stacks keep today's full `DeployStack` path unchanged.
- **Rollback re-translates the source**: `rollback` now re-translates the target revision's `source_yaml` with the current translator, records a fresh revision (new `rendered_hash`, `rollback_of` marker), and deploys through the ordered pipeline. Stored `rendered_yaml` is never executed (audit/display/hash only). A source that no longer validates under the current DSL fails loudly instead of silently deploying a stale translation. Deploy and sync already followed this invariant.
- New `StackDeployer` interface methods `DeployStackNoPrune` + `PruneStack` (production + all test fakes).
- Tests: `manifest.ServiceLevels`/`IR.Subset` (chain, fan-out, multi-level, cycle, unknown-dep, empty, subset recompute), ordered-deploy order + prune-once + cycle/unknown-dep rejection, rollback re-translate semantics.

## v0.2.115 (2026-09-30)

- Console: the **API Keys** nav link is now visible for admins when `EDGE_LOGIN_DISABLED=true` (it was previously bundled with the Users CRUD link, which correctly stays hidden — the API Keys page is admin-only but not login-UI-gated). Regression test added.

## v0.2.114 (2026-09-30)

- Usage graph now shows **linked configs** — the DSL resolves `config(name)` into env content at translation time, so rendered compose had no top-level `configs:` block and the usage page showed only secrets. The graph now unions `refs.FindAll(source_yaml)` (the authority for `config()`/`secrets()` refs) with rendered `configs:`/`secrets:` parsing.

## v0.2.113 (2026-09-30)

Quick wins from the improvement backlog (Q1–Q5; Q6 `deploy --compose` deferred):

1. **CLI UpdateStatus column** — `pmcluster service list` / `ps` gained a `STATE` column (`PAUSED`, `PAUSED: <error>`, `UPDATING`, `-`) from the service update status.
2. **Registry credentials on join** — `pmcluster join --copy-registry-creds <host>` ssh-fetches and merges the manager's `~/.docker/config.json`, and `--verify-registry-pull <image>` proves pulls work; both best-effort (the missing-auth silent-stale-image trap now has an explicit path).
3. **Webhook delivery retry** — deploy failures retry 2× every 30s (detached 4-minute budget, so the retry outlives the request), and delivery rows record `retries`; one request still yields exactly one delivery row.
4. **Per-stack API token scoping** — `pmcluster user create --stack <stack>` mints tokens that can only touch that stack's `/api/stacks/<name>` + `/api/services/<stack>` routes; everything else is `403 {"error":"token scoped to stack <x>"}`. Unscoped tokens unchanged. Migration 0020.
5. **OpenObserve alerting metrics** — the daemon now emits OTLP metrics: `pmcluster.webhook.requests.total{source,status}`, `pmcluster.services.paused{scope}`, `pmcluster.services.stale_images{scope}` (image > 30 days old), `pmcluster.reconcile.total{stack,status}` — for OO-side alerting.

## v0.2.112 (2026-09-30)

- Port-aware `depends_on` wait — the generated wait probe now connects to the dependency's service port (`nc -z`), not just its DNS name. Port resolution: `expose.port` wins, otherwise a well-known image default (postgres 5432, mysql/mariadb 3306, redis 6379, mongo 27017, nginx/httpd 80, rabbitmq 5672, elasticsearch 9200, memcached 11211), otherwise DNS-only.
- Wait scripts are `$`-escaped (`$$`) so `docker stack deploy` interpolation leaves them intact (the unescaped form broke the compose).

## v0.2.111 (2026-09-30)

- `depends_on` entrypoint wrap — services without an explicit `command:` get their ENTRYPOINT wrapped with the wait loop instead; the image's baked `CMD` flows through `$@`. Images whose runnable lives inside a baked entrypoint (e.g. Postgres) should declare an explicit `command:`.

## v0.2.110 (2026-09-30)

Five post-mortem hardening points:

1. **`depends_on` → real Swarm wait** — `docker stack deploy` ignores `depends_on`, so dependent services with an explicit command get a generated POSIX wait wrapper (`until getent hosts <dep> ...`), and `run_once` services with dependencies get `restart_policy: on-failure` (`max_attempts: 3`) instead of dying permanently.
2. **Stateful-aware defaults** — services that mount volumes automatically get `update: order: stop-first` (kills the Postgres `postmaster.pid` shutdown race) and auto-pin to the platform node (`platform_node` setting) when `placement` is empty. No manifest change needed.
3. **Registry auth + image freshness** — `pmcluster join` warns when no Docker registry credentials exist (missing `~/.docker/config.json` → stale cached images); the console shows `Image N days old` pills for stale local images.
4. **Paused updates surfaced** — Swarm's `UpdateStatus.State=paused` and the failure message are exposed through the services API and shown in the console (paused pill + hint), so updates that stall are no longer silent.
5. **Config/secret lifecycle** — `pmcluster secret edit` mirrors the new value to the Swarm secret (immutable secrets are removed and recreated); the inventory page shows which stacks reference each config/secret ("Referenced by").

## v0.2.109.1 (2026-09-30)

- Fix(ui): the stacks list no longer shows completed run-once jobs (e.g. a finished migration) as Degraded — completed jobs are excluded from the health totals.

## v0.2.109 (2026-09-30)

- Test coverage: CLI config/secret CRUD run-path tests (cli coverage 11.6% → 19.7%); pmapi client tests against a fake daemon (0% → 75.9%).

## v0.2.84 (2026-09-25)

- `pmcluster join --role worker|manager --token <token> --manager <host>:2377` — verify the
  joined role after `docker swarm join` and print quorum guidance.
- Two-manager production reference deployment (ubuntu Leader + pmcluster-worker Reachable).

## v0.2.83.1 (2026-09-24)

- Fix the systemd unit generated by the CLI: `ExecStart` must include the `serve`
  subcommand (a bare binary unit printed help and crash-looped).

## v0.2.83 (2026-09-24)

- **`pmcluster join`** — join the Swarm, initialise local control-plane state (no bootstrap
  user), install + start the daemon. The daemon is now started by the CLI:
  `cluster up`, `cluster update`, and `join` manage `/etc/systemd/system/pmcluster.service`;
  `install.sh` only installs the binary.
- `serve` waits for Swarm leadership before serving (standby on non-leader nodes);
  standalone/local mode serves immediately.

## v0.2.82 (2026-09-25)

- **Leader-aware daemon**: `serve` serves only on the Swarm leader (15s standby poll on
  non-leader managers). On promotion, the control-plane DB is restored from the newest
  `pmcluster-ctlplane-*.tar.gz` only when the local DB is missing or older — safe with
  shared storage (LINSTOR).

## v0.2.81 (2026-09-25)

- Console OpenObserve external link points at `observ.<domain>/sso-bridge` (one-click
  auto-login into OpenObserve).

## v0.2.80 (2026-09-25)

- **OpenObserve auto-login bridge** (`/sso-bridge` on the observ origin): a page served by
  the edge console that seeds OO's SPA session (`localStorage userInfo`, `pgdata` bypass)
  so OpenObserve opens logged-in behind the SSO/basic-auth gate.

## v0.2.79 (2026-09-25)

- Traefik `openobserve-auto-auth` now also injects the `auth_tokens` session cookie
  (30-day) — a deterministic base64url envelope built from the `openobserve_admin`
  credential — so OO's SPA does not redirect to its native login page.

## v0.2.78 (2026-09-25)

- Whole-disk (cluster-wide) backup runs count as **verified coverage** for every stack in
  the console's backup coverage table.

## v0.2.77.1 (2026-09-25)

- Restore: allow the archive root directory entry through the path-escape guard.

## v0.2.77 (2026-09-25)

- **Restore implemented**: `pmcluster backup restore <id>` extracts a SUCCEEDED backup.
  Stack-scoped runs restore under `<dest_root>/<stack>`; whole-disk scheduled runs restore
  to the volume root with the `/backup/data` prefix stripped; control-plane archives are
  refused into the volume root. Retention pruning (`backup_retention_days`, default 15)
  removes old rows **and** archive files.

## v0.2.76.1 (2026-09-25)

- **Backup discovery**: scheduled offen archives on disk (`/var/stack/backup`) are recorded
  into the backup list (migration 0018, unique partial index on `filename`) so the console
  and CLI show the full backup picture, including nightly runs with real archive paths.

## v0.2.76 (2026-09-25)

- Image display: digest trimmed from service tables; full ref shown on hover.

## v0.2.75 (2026-09-25)

- **Standalone stack detail page** (`/web/stacks/<name>`) with per-stack services: replica
  health, mode, updated, and Tasks/Logs/Restart actions.

## v0.2.74 (2026-09-25)

- Revision/backup IDs are displayed without thousands separators (`N0` template func);
  stack detail scrolls into view after navigation.

## v0.2.73 (2026-09-25)

- **Console v2** (PR #2): full EN/AR localization, calm theme, self-hosted IBM Plex fonts,
  RTL support, per-request language resolution. htmx stays on the CDN.

## v0.2.72 (2026-09-25)

- **Rebrand**: repository renamed to `hazemarian/poor-man-cluster`, MIT license, edge image
  now public at `ghcr.io/hazemarian/pmcluster-edge` (anonymous pulls work).

## v0.2.71 (2026-09-25)

- CLI/API audit completion: `backup browse|restore`, `webhook deliveries`,
  `cluster settings [get|set]`, `usage`; OpenAPI now documents 39 paths.

## v0.2.70 (2026-09-25)

- Fixed the console list pages: polling wrappers use a stable target + `innerHTML` swap so
  clicks are no longer swallowed and timers don't stack.

## v0.2.69 (2026-09-25)

- Fixed logs polling self-embedding (dedicated `servicelogs` fragment for 3s polls).

## v0.2.68 (2026-09-25)

- Settings + usage refactored into domain packages (`internal/settings`, `internal/usage`).

## v0.2.67 (2026-09-25)

- Console features: webhook delivery history, backup browse/restore, API token last-used,
  cluster settings edit surface, deploy pipeline view, secret/config usage graph, cert
  expiry badges.

## v0.2.66 (2026-09-25)

- Console SPA polish: active nav, loading indicator, toasts, live polling, list filters,
  revision timeline, log streaming, cluster status card.

## v0.2.65 (2026-09-25)

- **Deploy provenance**: `repo_url` + `source_file` required in webhook payloads and
  recorded per deploy (migration 0015).

## v0.2.64 (2026-09-25)

- `cluster up` creates the storage root directories (`/var/stack/data`, `/var/stack/backup`).

## v0.2.63 (2026-09-25)

- **Volume root**: every container volume — named or bind — is forced under one configurable
  host root (`volume_root`, default `/var/stack/data`) at `<root>/<app>/<name>`; the
  app-level `volumes:` block was removed from the DSL.

## v0.2.62 (2026-09-25)

- **Isolation refactor**: the Docker client moved behind a neutral port
  (`internal/runtime`); the DSL translation output moved behind a `Writer` interface
  (`internal/manifest`, ComposeWriter). Also fixed the `/web` PathPrefix trap that sent
  `/webhook/*` through the SSO gate.

## v0.2.61 (2026-09-25)

- SSO session expiry configurable (`--sso-cookie-expire`, default 1h).

## v0.2.60 (2026-09-25)

- SSO redirect finally fixed: `OAUTH2_PROXY_WHITELIST_DOMAINS` (plural).

## v0.2.59 (2026-09-25)

- `OAUTH2_PROXY_REVERSE_PROXY` so the post-login redirect returns to the original host.

## v0.2.58 (2026-09-25)

- Post-login landing returns to the original host; Traefik dashboard router matches the
  live gate.

## v0.2.57 (2026-09-25)

- `OAUTH2_PROXY_COOKIE_DOMAINS` (plural) fixes cross-host CSRF on the OAuth callback.

## v0.2.56 (2026-09-25)

- `/oauth2/*` resolves on every gated host; `certresolver=letsencrypt` labels are
  ACME-conditional (BYO-cert clusters).

## v0.2.55 (2026-09-25)

- SSO enabled on the live cluster; missing `sso_cookie_secret` self-heals on
  `cluster update`.

## v0.2.54 (2026-09-25)

- Console external links (OpenObserve, Traefik); oauth2-proxy on the `sso.<domain>`
  subdomain.

## v0.2.53 (2026-09-25)

- **SSO feature flag** (oauth2-proxy sidecar), interactive **`pmcluster setup`** wizard,
  OpenObserve root-admin ingestion (no provisioning API).

## v0.2.52 (2026-09-25)

- Users CRUD + RBAC (admin/operator/viewer) + `EDGE_LOGIN_DISABLED`.

## v0.2.51 (2026-09-25)

- **Auth gateway**: the console moves under `/web`, Traefik admin-auth (or SSO) gates
  `/web/*` + `observ.<domain>`; OpenObserve auto-auth header injection.

## v0.2.50 (2026-09-25)

- `rendered_hash` on stack revisions — `sync` skips no-op deploys.

## v0.2.49 (2026-09-25)

- `--version` flag.

## v0.2.48 (2026-09-25)

- `internal/refs` package — the shared `config()/secrets()` reference language.

## v0.2.47 (2026-09-25)

- Unified `config()/secrets()` references in the platform stack templates.

## v0.2.46.1 (2026-09-25)

- Wire the services domain into the daemon (`/api/services` routes).

## v0.2.46 (2026-09-25)

- **Config reconcile redesign**: DB-only configs, `rendered_hash` (migration 0013),
  init-only `cluster up`, hash-based `cluster update`, UI Sync button.

## v0.2.45 (2026-09-25)

- DB↔store config sync + drift-prune of services dropped from a compose.

## v0.2.44 (2026-09-25)

- Fix empty `volumes:` mapping when ACME is off.

## v0.2.43 (2026-09-25)

- **Service ops replace Portainer** (list/ps/tasks/logs/restart/exec).

## v0.2.42 (2026-09-25)

- Remote CLI mode (API URL/token via env or flags).
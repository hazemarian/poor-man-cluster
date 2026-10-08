# Changelog

Release history for **poor-man-cluster**. The RFC and the reference docs describe the

## v0.2.170 (2026-10-08)

Backups: the leader is always a storage node, the store is indexed, and no one-shot service.

- **The leader is always a storage node (BUG-024 policy).** `cluster update` re-adds the
  leader to `storage_nodes` when the setting omits it and stamps its `pmcluster.storage`
  label; `pmcluster node demote <leader>` is refused. The leader also runs the edge
  (console/API/webhooks), so the offen agent is always local and `pmcluster backup create`
  reaches it with a plain `docker exec`.
- `backup create` on a node with no offen agent now fails with an actionable message
  ("no offen backup agent on this node — it is not a storage node … run this on a storage
  node, or make this node one: pmcluster node promote <hostname>") instead of the generic
  "is the backup stack deployed?".
- **Reverted the one-shot backup service.** A manual trigger no longer creates an
  ephemeral swarm task: Docker has no remote exec, and with the leader guaranteed to be a
  storage node the local agent is always present, so no synthetic container is needed.
- `backup list`, restore and storage failover now see archives written by *other* nodes:
  the in-cluster store is indexed (S3 ListObjectsV2, prefix `backup-`, deduped, control-plane
  archives deliberately excluded) alongside the local archive directory.
- Tests: `TestUpdate_AddsLeaderToStorageNodes`, `TestRunNodeDemote_RefusesLeader`,
  `internal/backups/s3_list_test.go` (store discovery, pagination, store-only restore).

## v0.2.169.3 (2026-10-07)

The volume repair runs on standby managers too (BUG-026 follow-up 3).

- Root cause of the third miss: `pmcluster serve` on a non-leader manager **blocks in
  `waitForSwarmLeadership`** before it ever opens the store, so nothing registered after
  that point runs on a standby — the repair loop never started on nxt-sw-4-m (verified:
  no repair log line at all, `sfapp_db` still `Rejected`). The loop now starts right after
  the docker client, **before** the leadership wait, and reads an atomically published
  storage root (empty until the store is open, i.e. on a standby).
- Stack volumes no longer depend on that root: a volume mount is a local-driver bind whose
  `device` is pmcluster-generated, so the repair creates it directly (`runtime.Volume.Bind`
  marks it). Raw host binds are still restricted to the managed root.

## v0.2.169.2 (2026-10-07)

An empty `volume_root` row counts as unset (BUG-026 follow-up 2).

- `cluster up` persists `volume_root` as an **empty string** when the operator did not
  choose a directory, and `GetSettingDefault` returns the stored value — so both the
  startup storage-root creation and the volume repair compared against `""` and matched
  nothing, silently doing no work (verified live: no repair log line at all and `sfapp_db`
  still `Rejected` after the v0.2.169.1 deploy). Both now use a shared
  `localVolumeRoot(ctx, st)` helper: a blank setting falls back to
  `manifest.DefaultVolumeRoot` (`/var/stack/data`).

## v0.2.169.1 (2026-10-07)

Volume repair resolves the volume's host directory, not just raw binds (BUG-026 follow-up).

- v0.2.169's repair only looked at `bind` mounts, but a stateful stack volume is declared as
  a **named volume with a local-driver bind** (`driver_opts: {type: none, o: bind, device:
  <volume_root>/<app>/<name>}` in the rendered compose) — so the service mount is a `volume`
  mount whose source is just the volume name, and nothing got repaired (verified live:
  `sfapp_db` stayed `Rejected` on nxt-sw-4-m after the v0.2.169 deploy, while the leader had
  the directory). The repair now inspects each `volume` mount and uses the volume's `device`
  when it is under the volume root.
- `runtime.Client` gained `VolumeInspect` (name → driver, device, mountpoint).

## v0.2.169 (2026-10-07)

Storage directories are a node-local responsibility (BUG-026) — no remote mkdir, no
manual intervention.

- **The daemon creates the storage roots it owns at startup.** `pmcluster serve` now
  `mkdir -p`s the configured `volume_root` (the directory the operator selected, default
  `/var/stack/data`) plus the backup directory before it does anything else. The setting is
  read only when the store already exists, so a not-yet-initialised node still gets the
  familiar `run pmcluster init` error instead of a silently created empty database.
- **Every daemon repairs the volume directories of the stateful services the Swarm placed
  on it** (new `internal/cli/volume_repair.go`, at startup and every 30 s, leader or not).
  A pass lists the services, keeps the ones whose `io.pmcluster.node` pin is this hostname,
  and `mkdir -p`s any missing bind source under the volume root — then force-updates the
  services it repaired so the Swarm retries the tasks it had rejected. This fixes the case
  the earlier local-CLI step could not: the deploying host is not necessarily the node a
  stateful stack is pinned to, and the Swarm refuses to start a task whose bind source is
  missing (`failed to populate volume: mount /var/stack/data/sfapp/db_data: no such file or
  directory`, reported live in TC10-B). Services without a node pin (role-based or global —
  the platform agents) are left to `cluster up`/`update`; a worker (which cannot list
  services) stays silent and retries on the next tick.
- `runtime.ServiceInspectResult` now carries the task-template mounts (`runtime.Mount`) so
  the repair can see a service's bind sources.

## v0.2.168.3 (2026-10-07)

Node-loss recovery, take two: the platform services that actually got stuck now recover too
(BUG-023 follow-up).

- v0.2.168.2 changed the **default** restart policy for replicated services to `any`, but the
  four services that hung at 0/1 in the TC10-A failover campaign — `edge_pmcluster-edge`,
  `backup_volume-backup`, `backup_control-plane-backup`, `backup_seaweedfs` — set
  `restart: on-failure` **explicitly** in the platform manifests, so the explicit value kept
  winning and their tasks were still never replaced after a clean (exit 0) termination.
- Those four platform services now declare `restart: any` (they are long-running daemons:
  the edge proxy, the two offen backup agents and the in-cluster backup store), keeping their
  `restart_delay`.
- Verified live after deploy: every platform service now renders
  `{"Condition":"any","Delay":5000000000}`.

## v0.2.168.2 (2026-10-07)

Node-loss recovery: replicated services come back on their own, and the daemon
returns with Docker (BUG-021b, BUG-023 — both found live during the four-manager
failover campaign).

- **BUG-023 — a cleanly-exited replicated task was never replaced.** A
  long-running service whose container exited with code 0 (SIGTERM while its
  node's Docker daemon stopped, e.g. a Docker upgrade) is marked `Complete` by
  the Swarm, and `restart_policy: condition: on-failure` ignores a zero exit, so
  the task was never rescheduled: after the test cluster's daemons restarted,
  `edge_pmcluster-edge` and `backup_control-plane-backup` sat at 0/1 and
  `backup_volume-backup` at 1/2 until a manual `docker service update --force`.
  The writer now renders `condition: any` for the default replicated case — the
  same policy the global services already used, which is why they recovered on
  their own. An explicit `restart:` in a manifest still wins, and run-once jobs
  are unchanged (`none`, or `on-failure` with `max_attempts: 3` when the job has
  `depends_on`).
- **BUG-021b — starting Docker did not start the daemon.** `BindsTo=docker.service`
  (v0.2.167) propagates a *stop* but not a *start*, so
  `systemctl stop docker && systemctl start docker` left `pmcluster` inactive on
  the standby managers; a node with no control loop silently stops reconciling if
  it is (or becomes) the Raft leader. The generated unit now also carries
  `WantedBy=docker.service`, so starting Docker pulls the daemon back in.
  `ensureDaemonRunning` (run by `cluster up`/`update`/`reset`/`join`) rewrites and
  re-enables the unit, so `install.sh` rolls the change out.
- Tests: `TestRenderSystemdUnit` asserts `WantedBy=docker.service`; the translator
  goldens and `TestTranslate_Golden`'s expectations assert `condition: any` for
  replicated services (the run-once fixtures keep `condition: none`).

## v0.2.168.1 (2026-10-07)

Standby managers also get the current daemon + systemd unit.

- Follow-up to v0.2.168: the standby early-return in `cluster update` / `cluster reset` skipped
  `ensureDaemonRunning`, so a manager that has never been promoted kept the old unit
  (`Requires=docker.service`) until it was promoted. It now refreshes the unit and restarts the
  daemon on the standby path too, so unit changes (BUG-021's `BindsTo=docker.service`) roll out
  to every manager during an `install.sh` upgrade.

## v0.2.168 (2026-10-07)

CLI: `cluster update` / `cluster reset` no longer mistake a standby manager for a fresh box (BUG-022).

- Found in the four-manager campaign: `install.sh VERSION=…` on **nxt-sw-3-w** — a manager that
  joined the Swarm but was never promoted — failed with
  `No cluster configuration found — starting the interactive setup wizard` →
  `error: domain is required (interactive prompt or --domain)`.
- Root cause: a manager that never won a Raft election keeps an intentionally **empty local
  store** (the control plane lives in the Raft state snapshot and is restored when the node is
  promoted — 3-w had 0 `cluster_settings` rows while the promoted 2-m had 29), and both
  commands used "no stored `domain`" as the test for "no cluster".
- `classifyLocalCluster` now distinguishes three states: **provisioned** (settings present),
  **standby** (no settings, but the node is a member of an existing Swarm) and **fresh** (no
  settings, no Swarm). A standby manager gets a one-line explanation and exits 0 — nothing to
  update there — and only a genuinely fresh box drops into the wizard. The Docker probe used
  for classification is best-effort, so a host without a daemon still reaches the wizard.
- Tests: `TestClassifyLocalCluster` (fresh / standby / probe-error / nil client /
  provisioned-wins).

## v0.2.167 (2026-10-07)

Control plane: survive a leaderless swarm, and come back after a docker restart (BUG-020, BUG-021).

Found in the four-manager failover test (TC10-A). `systemctl stop docker` on the leader
elected a new leader in ~25 s, the new leader restored the whole control plane from the Raft
snapshot (configs + settings) exactly as designed, and a second consecutive leader loss was
likewise recovered — but two recovery paths were wrong:

- **BUG-020 — a daemon that starts while the swarm has no Raft leader crash-looped.**
  Docker answers every config call with `The swarm does not have a leader ...`, and the
  startup restore treated it as fatal, so systemd restarted the daemon every ~5 s (observed:
  28 restarts in ~2 min). `IsSwarmUnavailable` (exported; was `isNotSwarmManager`) now also
  recognises the no-quorum messages — `does not have a leader`, `too few managers are
  online`; snapshot and restore become a logged no-op, the startup restore falls through to
  the tarball archive (and keeps local data when there is none), and the daemon stays up and
  stands by until quorum returns.
- **BUG-021 — a docker-only restart left `pmcluster` inactive.** The generated unit used
  `Requires=docker.service`, which propagates stop but not start: stopping dockerd stopped
  the daemon and starting dockerd did not bring it back (verified live — a manager returned
  with no control loop, so a leader that is or becomes the Raft leader stops reconciling
  silently). The unit now uses `BindsTo=docker.service`.

Upgrade note: the unit is rewritten by `cluster up` / `join` / `cluster reset` (not by
`cluster update`), so re-run one of those — or `install.sh` — to pick up the new unit.

Tests: leaderless-swarm fixtures in `internal/controlplane` + `internal/cli` (verbatim
Docker no-leader errors), a six-case `IsSwarmUnavailable` table, and the updated unit
expectation. Falsified: disabling both no-quorum patterns makes the cli test fail with the
exact crash-loop error.

## v0.2.166.1 (2026-10-07)

CI: golangci-lint is green again — explicit returns after `t.Fatal` nil-checks.

- The main gate had been red for six consecutive commits: under **Go 1.25** staticcheck
  reported 30 `SA5011 possible nil pointer dereference` findings in test files (in that
  analysis state it does not treat `t.Fatal` as terminating; the same code is clean under
  Go 1.26, which is why it never reproduced locally).
- Fixed by adding an explicit `return` to each nil-checked branch — 15 sites across 10
  test files. No assertion was changed, no linter rule disabled, no production code touched.
- Test-only change: the v0.2.166.1 binaries are identical to v0.2.166.

## v0.2.166 (2026-10-07)

Secrets: `pmcluster secret heal` — find and repair rows the current key cannot open.

- New command `pmcluster secret heal [--dry-run]`: walks every stored secret and
  1. decrypts the row with the current `~/.pmcluster/.encryption_key`,
  2. repairs a **stale hash** (translation derives the Docker swarm secret name from the
     stored hash, so a mismatch referenced a swarm object that never existed),
  3. creates the content-addressed swarm secret when it is missing.
- Rows that do **not** decrypt were sealed by an older key (rotated or restored at some
  point). AES-GCM cannot be reversed without the key that sealed them, so the pass reports
  them as `unrecoverable` and prints the recovery recipe: read the plaintext from a
  container that still mounts it (`docker exec <c> cat /run/secrets/<name>`) and re-supply
  it with `printf '%s' '<value>' | pmcluster secret edit <name>`. A pass never changes a
  secret's value; `--dry-run` reports without writing anything.
- Exits non-zero when something still needs attention (unrecoverable rows, or a swarm
  mirror that could not be created because no daemon was reachable).
- Driven by a real incident: on nextrum 8 of 9 rows had been sealed under an older key, so
  `cluster update` warned `cannot rebuild swarm object (ciphertext not recoverable)` and a
  from-scratch rebuild would have failed. Those rows were healed by hand; this command
  makes it a one-liner.
- Core logic lives in `internal/secrets` (`Local.Heal` + `HealReport`, with the swarm
  mirror injected) so it is testable without Docker; `internal/cli` wires the mirror and
  prints the table. `mirrorSwarmSecret` now shares `swarmSecretMirrored` with the heal pass.
- Managed credentials (`traefik_dashboard`, `openobserve_admin`, …) are still healed by
  `pmcluster cluster update`; this command covers the user secret table.
- Tests: `internal/secrets/heal_test.go` (healthy row ok, mirror created, stale hash
  repaired, unrecoverable reported without a mirror attempt, dry-run changes nothing,
  mirror failure degraded, empty store) and `internal/cli/secret_heal_test.go` (command
  registered, run path creates the content-addressed swarm secret).

## v0.2.165 (2026-10-07)

Reconcile: a failed app-stack deploy no longer suppresses retries (BUG-018).

- Found live during the v0.2.164 rollout: `demoenv` and `uitest` failed to deploy
  (`service web: secret not found: demoenv_db_pass_3bd8cb98`) yet a revision carrying the
  **new** rendered hash was recorded anyway, so every later `Sync` compared the stored
  hash with the fresh render, saw them equal, and logged `no drift` forever — the stack
  never retried and stayed on a five-day-old task. (The affected swarm secrets were
  rebuilt by hand and both stacks re-applied.)
- Root cause: the app lane recorded the revision (with its rendered hash) **before**
  applying to the swarm. That is the BUG-009 class, fixed for platform stacks in v0.2.148
  (`SetRendered` only after a successful deploy) but never for apps.
- Fix: `Deploy`, `Sync` and `Rollback` now record the revision with an **empty**
  `rendered_hash` and stamp it — via the new `store.SetRevisionRenderedHash` — only after
  the swarm apply succeeds, on the synchronous path, the async/background (webhook) path
  and rollback. A failed apply leaves the hash empty, so the next `Sync` sees a mismatch
  and retries. The revision number, source YAML and audit trail are unchanged.
- Safety net in `stackdrift.InSync`: when the fresh render carries a label but **no** live
  service does, that is drift (`the labelled render was not applied`), which also heals a
  state already poisoned by the old ordering. A stack where none carry the label used to
  be treated as in-sync.
- Tests: `TestDeploy_FailedApplyLeavesHashEmptyAndNextSyncRetries` (fails without the
  fix), a `store` test for the new setter, a `stackdrift` case for the new rule; platform
  test fixtures now stamp the real rendered-hash label — no production rule was weakened.

## v0.2.164 (2026-10-07)

Reconcile: every deployed service carries its content hash — app stacks too (k8s-style).

- The v0.2.163 drift detection covered **platform** stacks only. App/customer stacks
  still decided "no drift" purely on the stored rendered hash (DB vs fresh render), so a
  customer stack that lost a service — or got a half-applied deploy — was reported
  in-sync forever. The same bug class, in the other lane.
- Every service of an app stack is now stamped with `io.pmcluster.rendered_hash` (at the
  `Deploy`, `Sync`, `Rollback` and per-level `depends_on` writer sites), and `Sync`
  compares the **live swarm** against the fresh render before declaring no-drift:
  a missing service, an absent stack, a stale label or a partial label set re-applies the
  stack (recording a new revision). A transient docker error is treated as in-sync so it
  can never cause a redeploy loop; `Docker == nil` (CLI deploys) is unchanged.
- The `depends_on` per-level path computes the hash **once** from the full-stack
  label-free render and reuses a single writer for every subset, so each level carries
  the identical hash. A per-subset hash would never match the full render and would
  redeploy on every pass (infinite loop) — pinned by
  `TestDeploy_OrderedLevelsStampSameHashOnEveryLevel`.
- The detector moved to a shared `internal/stackdrift` package (`InSync` +
  `ContentHash`) used by both lanes (`cluster update` and the app `Sync`).
- **Upgrade note:** the app render changes, so the first `cluster update` after upgrading
  redeploys every app/customer stack once (rolling, `start-first`) — that is what stamps
  the labels. Platform stacks behaved the same way in v0.2.163.
- Tests: `internal/stackdrift/check_test.go` (every rule: nil client, absent stack,
  missing service, drifted label, partial labels, all-unlabeled in-sync, extra live
  service ignored) and `internal/stacks/drift_test.go` (label on every service,
  identical hash across `depends_on` levels, `Sync` redeploys on a missing service /
  stale label, transient docker error → in-sync).

## v0.2.163.2 (2026-10-07)

Reconcile: a partially applied platform deploy counts as drift (BUG-017 follow-up).

- The drift check treated a service with **no** `io.pmcluster.rendered_hash` label as
  "in sync" (so the first update after upgrading would not storm through every stack).
  That was too lenient: when only **some** services in a stack carry the label the stack
  was left partially applied — e.g. a `docker stack deploy` cut off mid-flight because
  the daemon restarted during an install. Hit live on wafaa: `backup_volume-backup` and
  `backup_control-plane-backup` were left unstamped while `backup_seaweedfs` was stamped,
  and the drift went unnoticed. A mixed label state is now reported as
  **"some services are missing the rendered-hash label (partial deploy)"** and re-applied.
  Stacks where **no** service carries the label (pre-upgrade) are still left alone.
- New regression test `TestUpdate_RedeploysWhenServiceLabelMissing` (fails on the
  previous logic, passes now).

## v0.2.163.1 (2026-10-07)

Reconcile: name the reason a platform stack is redeployed.

- `cluster update` now prints **why** each stack is redeployed —
  `backup drifted from the rendered compose → re-deploying`,
  `backup a required service is missing → re-deploying`,
  `backup absent from the swarm → re-deploying`, `backup content changed → …`,
  `backup forced → …`. Previously every redeploy printed the generic
  "content changed", which was actively misleading for the drift case BUG-017
  exists to catch. `platformStackInSync` returns the reason alongside the
  verdict. No behaviour change.

## v0.2.163 (2026-10-07)

Reconcile: `cluster update` detects Swarm drift instead of trusting the stored hash (BUG-017).

- **BUG-017 fixed — updates no longer report "nothing to redeploy" while the Swarm
  has drifted.** The redeploy decision compared the STORED rendered hash against a
  fresh render, never against the LIVE Swarm: the store only records what pmcluster
  last *intended* to deploy, so a service that missed an update stayed stale forever.
  This is how a production cluster's control-plane backup kept running with **no
  remote destination at all** (local-only, on the same node as the DB) while every
  `cluster update` reported success.
- **Fix**: every platform service is now stamped with
  `io.pmcluster.rendered_hash` (the hash of the render that produced it), and
  `cluster update` compares those live labels against the fresh render — alongside
  the existing stored-hash comparison. A stack is redeployed when the hash changed,
  when the stack is absent, when a service the render expects is **missing**, or when
  a live service carries the label of a **different render**.
- Deliberately conservative: an extra live service the render no longer produces is
  ignored (`docker stack deploy` cannot remove it, so flagging it would redeploy
  forever), and a service with no label (predating this release) is treated as in
  sync — the render change that introduced the label already forces one redeploy.
  The check detects swarm-vs-render drift, not a hand-edited env on a live service.
- **Upgrade note**: the rendered compose changes, so the first `cluster update` after
  upgrading redeploys every platform stack once (that is what stamps the labels).
- Tests: `TestUpdate_RedeploysWhenLiveServiceLabelDrifted` (fails against the old
  DB-only comparison), plus the swarm fixtures now model the full rendered service
  set (the control-plane + store services).

## v0.2.162.1 (2026-10-07)

Backup: store-only clusters bootstrap their bucket; setup asks about S3 and the store node.

- **BUG-016 fixed — the store bucket is bootstrapped for store-only clusters.** The
  in-cluster store leg now ALWAYS uses the WebDAV backend (whenever the store is
  enabled): the SeaweedFS S3 API does NOT auto-create the bucket, so a store-only
  cluster's first backup failed with `NoSuchBucket`; the WebDAV gateway creates the
  `/buckets/<bucket>` path on the first PUT. Offsite S3 (`AWS_*`) is now rendered
  only when `backup_s3_*` is configured, so a store-only cluster has WEBDAV only.
- **setup asks about offsite backups**: "Upload backups offsite (S3/R2)?" — if yes it
  collects endpoint / bucket / region / access key / secret key (secrets entered
  masked, no echo); if no the cluster stays store-only.
- **setup asks where the store runs**: "Run the in-cluster backup store on 'leader'
  or 'a worker'?" → new `backup_store_on` setting (`leader` | `worker`; default
  worker). `pickStoreNode` pins SeaweedFS to the leader (preferring the elected
  leader, else any manager) or to a non-storage worker. Also `--backup-store-on`
  for non-interactive setup.

## v0.2.162 (2026-10-07)

Backup store: offen double-writes each archive — the rclone replicator is gone.

- **offen double-write**: when the in-cluster SeaweedFS store AND the offsite
  `backup_s3_*` target are both configured, the backup agents (volume +
  control-plane) upload every archive to BOTH — offsite via S3 (`AWS_*`, scheme
  stripped into `AWS_ENDPOINT_PROTO=https`) and the in-cluster store via a new
  WebDAV backend (`WEBDAV_URL` / `WEBDAV_PATH`). offen's native retention
  pruning keeps both copies in lockstep.
- **rclone replicator removed**: the `backup-replicate` sidecar (the `rclone
  sync` loop) is deleted — offen writes the two copies directly, nothing mirrors
  the bucket anymore.
- **WebDAV gateway, in-process** (`weed server … -webdav -webdav.port=7333`): the
  gateway runs inside the SAME SeaweedFS container (no separate service), so it
  reaches the filer on loopback and shares the container's network namespace. It
  serves the SAME filer namespace as the S3 gateway, so a WebDAV write at
  `/buckets/pmcluster-backups/<file>` IS the S3 object `<file>` (verified live on
  the 2-node cluster, both directions). Cluster-internal: no published port,
  never on the ingress network, no auth.
- SeaweedFS keeps advertising loopback (`-ip=127.0.0.1`, bind `0.0.0.0`): the
  published 8333 attaches the ingress network, whose auto-detected task IP is not
  self-reachable and so must not be advertised. Because WebDAV is in-process,
  there is no cross-container loopback problem.
- The daemon read path is unchanged: failover/restore still reads the store via
  S3 at `127.0.0.1:8333` (bucket `pmcluster-backups`).
- Tests: `TestLoadComposeFile_BackupSeaweedFS` (in-process webdav + offen
  double-write + no rclone), `TestLoadComposeFile_BackupSeaweedFSDisabled`, and
  a new `TestLoadComposeFile_BackupSeaweedFSStoreOnly`.

## v0.2.161 (2026-10-05)

Cluster lifecycle, self-healing reconcile, and swarm-wipe recovery.

- **`cluster update` is now existence-aware**: on a matching rendered hash it
  still verifies each platform stack is present in the swarm (via the stack
  namespace label) and redeploys it when absent, so a wiped/partial swarm no
  longer stays empty.
- **Self-healing update**: every `cluster update` now re-ensures the external
  overlay networks (traefik-net, monitoring-net), re-materializes ALL managed
  credentials' swarm secrets (incl. the non-rotatable `edge_api_token`) and
  rebuilds user secrets/configs from the DB index BEFORE deploying stacks, and
  detects a changed swarm ID (wipe) and forces a redeploy.
- **`cluster reset [--restore <archive>]`**: rebuilds the cluster from the DB —
  optionally restoring the store from a purge backup, then a forced,
  existence-aware reconcile. Idempotent; the recovery verb for a wrecked swarm.
- **`cluster down --purge` now purges everything**: it stops the daemon, writes
  a restorable store backup (`purge-backup-<ts>.tar.gz` = data.db +
  .encryption_key + config.yaml) to the data dir, then removes the platform
  resources AND the local store (the backup archive is kept). Without `--purge`,
  behavior is unchanged (DB preserved). `cluster down` on a fresh box with no
  store is a graceful no-op.
- Tests: existence-aware redeploy, network re-ensure, managed-credential
  re-materialization (plain + htpasswd), swarm-ID change, `--force`, purge
  backup+delete, backup round-trip + traversal guard, `reset` registration.

## v0.2.160.3 (2026-10-05)

SeaweedFS store fixes found deploying to the 2-node test cluster.

- **Swarm advertise IP**: a published port attaches the ingress network, so
  SeaweedFS auto-detected a task IP it could not self-reach and object uploads
  timed out (`dial tcp …:8080: i/o timeout`). It now advertises `127.0.0.1` and
  binds all interfaces (`-ip=127.0.0.1 -ip.bind=0.0.0.0 -s3.ip.bind=0.0.0.0`).
- **offen endpoint**: `AWS_ENDPOINT` must be host:port with the protocol in
  `AWS_ENDPOINT_PROTO`; a scheme (`http://`) is rejected ("conflicts with the
  secure option"). The in-cluster endpoint is now `backup_seaweedfs:8333` with
  `AWS_ENDPOINT_PROTO=http` (rclone keeps its own `http://` prefix).

## v0.2.160.1 (2026-10-05)

In-cluster backup store moved from MinIO to SeaweedFS (supersedes v0.2.160).

- **Why**: MinIO withdrew its public Docker Hub images (`minio/minio`,
  `minio/mc` → "repository does not exist"; `quay.io/minio/minio` → 401), so the
  v0.2.160 store could not pull and backups broke wherever it auto-enabled. Only
  the TC7 test cluster had it; production was never on v0.2.160.
- **Engine: SeaweedFS** (`chrislusf/seaweedfs:4.48`) — fully declarative, no
  admin API and no bootstrap: `server -s3 -s3.port=8333` with the S3 identity
  supplied via env (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`); buckets are
  created on first upload. Headless; pinned to a NON-storage node via an explicit
  placement (bypasses the volumed-service → storage-node auto-pin; first
  non-storage worker, else first non-storage node, else platform/manager).
- **Offsite replication**: an `rclone/rclone` sidecar `rclone sync` loop mirrors
  the bucket to the `backup_s3_*` target (deletions propagate, so retention
  pruning also cleans the offsite copy).
- **offen uploads to SeaweedFS** (`AWS_ENDPOINT=http://backup_seaweedfs:8333`,
  bucket `pmcluster-backups`); the `seaweedfs` service publishes 8333 on the
  routing mesh so the leader daemon reads backups at `127.0.0.1:8333` during a
  failover restore. Failover/restore code path unchanged (the store *is* the S3
  endpoint).
- **`seaweedfs_admin` managed credential** (access key `pmclusterbackup`, secret
  `seaweedfs_credentials`); `cluster update` self-heals it on existing clusters.
- Tests: `TestLoadComposeFile_BackupSeaweedFS` (image + command + plain volume +
  explicit non-storage placement + storage-pin bypass + offen→store + rclone
  mirror) and `TestLoadComposeFile_BackupSeaweedFSDisabled`.

## v0.2.160 (2026-10-05)

**Superseded by v0.2.160.1** — MinIO images are no longer pullable; see above.

In-cluster MinIO backup store (one combined backup stack).

- **MinIO + backup agents in one stack**: the backup stack now runs a headless
  MinIO (`MINIO_BROWSER=off`, no console) alongside the offen agents, so there
  is nothing to click — configure the offsite target once and forget it.
- **Durable copy in a different failure domain**: MinIO is pinned to a
  NON-storage node via an **explicit placement**, which bypasses the
  volumed-service → storage-node auto-pin (that pin only fires when placement is
  empty). Chosen at render time as the first non-storage worker, then the first
  non-storage node, falling back to the platform node / manager only when every
  node is a storage node.
- **offen uploads to MinIO** instead of straight to the offsite endpoint
  (`AWS_ENDPOINT=http://backup_minio:9000`, bucket `pmcluster-backups`).
- **Offsite replication**: a `backup-replicate` sidecar (`minio/mc`) continuously
  mirrors the MinIO bucket to the existing `backup_s3_*` target
  (`mc mirror --overwrite --remove --watch`), so retention pruning also cleans
  the offsite copy.
- **Failover/restore unchanged**: MinIO *is* the S3 endpoint now — the leader
  daemon's `fetchNewestArchiveFromS3` and `backup restore --from-s3` read from
  MinIO (`127.0.0.1:9000` via the routing mesh); the code path is identical.
- **`minio_admin` managed credential**: root user/password auto-minted at
  `cluster up` (username `pmcluster`, secret `minio_root_password`); `cluster
  update` self-heals it on existing clusters so MinIO enables on the next update.
- Tests: `TestLoadComposeFile_BackupMinIO` (headless MinIO, explicit non-storage
  placement, storage-pin bypass asserted by constraint count, offen→MinIO,
  replicator) + `TestLoadComposeFile_BackupMinIODisabled` (legacy offsite path).

## v0.2.159 (2026-10-05)

Storage failover safety, lifecycle & visibility (Test Case 8 follow-up).

- **`storage_failover` setting (opt-in)**: automatic failover is now gated on
  `storage_failover=true`. When a storage node goes down and the setting is off,
  the control loop leaves the stack paused and emits an alert signal instead of
  moving it. New clusters are prompted at setup when offsite S3 is configured
  ("Enable automatic storage failover?"); without S3 it stays off.
- **S3-only failover**: the automatic path always restores the newest offsite
  archive (`MoveOptions{FromS3:true}`); a down node can never be a source, so
  storage HA depends on S3. The HTTP mover remains for manual moves with both
  nodes up.
- **Failover marker + lifecycle**: a successful failover persists a
  `stack_failover` row (`from`, `to`, `at`, `acked`). Until acknowledged or moved
  back, the stack shows a `failover` status.
- **Failover badge (GitHub-shared)**: `/api/public/badge/{stack}` returns an
  amber `failover` status while an unacknowledged marker exists — the shared
  signal a human must act on.
- **Console + CLI**: stack detail shows a warning banner with [Move back] and
  [Acknowledge & mark healthy]; `pmcluster stack show` surfaces the marker and
  `pmcluster stack ack <stack>` acknowledges it. A move (including move-back)
  clears the marker.
- **OTel failover metrics**: `pmcluster.storage.failover.total`,
  `pmcluster.storage.node.down`, `pmcluster.storage.failover.disabled` — the
  operator builds alerts on top in OpenObserve.
- **Hourly backups**: new `backup_cron` setting (default `0 * * * *`) renders
  into the volume-backup agent's `BACKUP_CRON_EXPRESSION`, so the offsite archive
  is never more than an hour old (the failover restore source). Set e.g.
  `0 3 * * *` for the old daily cadence. Offen already prunes remote S3 objects
  via `BACKUP_PRUNING_PREFIX`/`BACKUP_RETENTION_DAYS`.

## v0.2.158.2 (2026-10-05)

Storage move transit fixes (round two) — found live in Test Case 8 (two-node storage HA).

- **Mover candidate ordering**: the mover now tries local interface IPs (RFC1918
  private addresses first) before the swarm advertise address. The advertise is
  often a public IP that hardened nodes silently drop on ephemeral ports, and
  busybox `wget -T` cannot reliably bound the connect on a dropped SYN — so a
  public-first order hung the mover until the task deadline even though a
  working private address existed. `moverCandidateURLs` now sorts private
  (10/8, 172.16/12, 192.168/16) local addresses first and keeps the advertise
  address as the final fallback.
- **Stale target cleanup**: the mover script now runs `rm -rf /data/<stack>`
  before `mv` so a leftover subtree from a previous failed move attempt (which
  made `mv` error with "Is a directory") no longer blocks a retry.
- Tests updated/added for the new candidate ordering (advertise last, dedup,
  no loopback leak, bare-IP advertise handled) + full suite green.

## v0.2.158.1 (2026-10-05)

Storage move transit fix — found live in Test Case 8 (two-node storage HA).

- **BUG-015:** the cross-node mover built its archive URL only from the swarm
  leader advertise address (public IP). On hardened clusters the public
  interface is not routed between nodes, so the mover task hung and failed
  (`non-zero exit (1)`) while the stack stayed safely on the source node.
  Fix: the mover now tries the advertise address first, then every non-loopback
  local interface IP (private-network reachability), deduped — the ephemeral
  archive server binds 0.0.0.0 so any of them serves it. Mover script loops the
  candidate URLs until one `wget` succeeds.
- Tests: `moverCandidateURLs` unit tests (advertise-first ordering, port
  stripping via SplitHostPort, bare-address + empty-address handling, no
  loopback leakage, dedup).

## v0.2.158 (2026-10-05)

Storage-node lifecycle, move + automatic storage failover — Test Case 8 (two-node storage HA).

- **`node promote <hostname>` / `node demote <hostname>`** — make any swarm node a storage node (updates the `storage_nodes` setting and stamps/clears the `pmcluster.storage` node label). Workers qualify, not just the leader.
- **Console storage promotion** — the Overview nodes table shows a storage pill per node and a promote/demote button (shield icon), backed by new daemon endpoints `POST /api/nodes/{hostname}/storage` + `DELETE /api/nodes/{hostname}/storage` (the `/api/nodes` list now also reports each node's `storage` flag).
- **`stack move` via console** — new `POST /api/stacks/{name}/move` daemon endpoint + a Move form on the stack detail page (target node input), matching the existing `pmcluster stack move <name> --to <node>` CLI.
- **Move with S3 restore** — `stacks.MoveWithOptions` with `MoveOptions{FromS3: true}` fetches the newest succeeded backup for the stack from the offsite store (`backups.FetchS3Object` is now exported) instead of triggering a local backup — for when the source storage node is down and its local archive is unreachable.
- **Automatic storage failover** — when a storage node goes down, the reconcile loop now picks a healthy alternate storage node (from `storage_nodes` ∩ healthy nodes) and moves each pinned stateful stack there with an S3 restore, instead of pausing indefinitely. Moves run async with a 5-minute per-stack cooldown (`FailoverMove` seam is injectable for tests).
- **Tests:** CLI promote/demote (6), API storage handler (6: promote/demote/idempotent/404/500/storage-flag), reconcile failover (move-to-healthy + cooldown + no-alternate), UI controller move (happy/empty-target), UI node promote/demote.

## v0.2.157 (2026-10-05)

Traefik runs on **every** node — found during Test Case 7 (1 manager + 1 worker bring-up).

- **Root cause:** `infra-stack.yml` rendered traefik as `mode: global` but with a placement constraint `node.hostname == <platform_node>` (or `node.role == manager` fallback). On a swarm with a single manager, that pinned the ingress gateway to the leader only — workers never got a traefik replica, so the requirement "traefik on all nodes" was not met.
- **Fix:** traefik is now placement-free (global, unconstrained) — one replica on every node, managers and workers alike, so any node can serve ingress traffic. The other platform stacks (edge, openobserve, backup, sso) keep their platform-node/manager pinning.
- **Tests:** `TestTraefikRunsOnAllNodes` (asserts global + no `node.role`/`node.hostname`/`constraints` with and without PlatformNode); `TestPlatformNodePinsAllStacks` and `TestPlatformNodeEmptyKeepsManagerRole` narrowed to the non-ingress stacks.

## v0.2.156 (2026-10-05)

Fix `secret(<name>)` env refs for **user/app manifests** — found during the post-v0.2.155 test-node purge + re-test.

- **Root cause:** `secret()` worked only on the platform path (`renderRefResolver`); the app-path resolver (`StoreConfigResolver`) did not implement `manifest.SecretValueResolver`, so a manifest like `env: APP_PASS: secret(t2_pass)` failed with `secret(t2_pass) requires secret-value resolution (not available)`.
- **Fix:** `StoreConfigResolver` gained a `Cipher *credentials.Cipher` field + `ResolveSecretValue(ctx, stack, name)` — fetches the DB secret row, refuses cross-stack rows (`belongs to stack %q`), decrypts the ciphertext with the cluster encryption key (Docker secrets are write-only, so the value can only come from the DB). Nil store / nil cipher / missing secret all fail loud with operator hints.
- **Wiring:** daemon (`serve.go`) passes the already-opened cipher; CLI local deploys (`openDeploySvc`) open the cipher from `cfg.EncryptionKeyPath()`.
- **Test:** `TestStoreConfigResolver_ResolveSecretValue` (5 subtests: own-stack decrypt, cross-stack refusal, missing-secret hint, nil cipher, nil store).
*current* state of the project; this file is the only place that tracks what changed
and when.

## v0.2.155.3 (2026-10-04)

- **fix(cluster): edge-stack TLS certresolver labels are ACME-conditional (prod still down on non-ACME clusters — 404/000 persisted after v0.2.155.2).** The edge-stack DSL conversion hard-coded `traefik.http.routers.{pmcluster-web,pmcluster-api,sso-bridge}.tls.certresolver=letsencrypt` unconditionally. On ACME clusters (test node) the resolver exists so the labels are harmless; on BYO-cert clusters (prod: `tls_mode` empty, ACMEEmail unset) the letsencrypt resolver is never declared (infra-stack only declares it under `[[ if .ACMEEmail ]]`), so Traefik logged `Router uses a nonexistent certificate resolver letsencrypt [pmcluster-api@swarm, sso-bridge@swarm, pmcluster-web@swarm]` and dropped every edge router — customer apps kept returning 404 even after the v0.2.155.2 mount fix. Fix: the three certresolver labels are now wrapped in `[[- if .ACMEEmail ]]`. Regression test: `TestLoadComposeFile_EdgeBYOModeNoLetsEncrypt` (BYO render has no letsencrypt references; ACME render still pins the resolver).

## v0.2.155.2 (2026-10-04)

- **fix(cluster): infra stack mounts the site certificate at its VERSIONED path (prod outage — apps 000/404).** The L4 platform-via-DSL conversion of infra-stack.yml listed the TLS cert/key in the traefik service secrets array under their LOGICAL names (`cert`/`key`) → compose mounted them at `/run/secrets/cert` + `/run/secrets/key`. The rendered Traefik dynamic config, however, references the VERSIONED content-addressed paths (`certFile: /run/secrets/cert_ab64977f`, `keyFile: /run/secrets/key_a6411269`) — the legacy raw-compose template resolved `secrets(cert)` to the versioned name, so the two agreed before L4. Result after v0.2.155: Traefik logged `Unable to parse certificate /run/secrets/cert_ab64977f ... no PEM data`, could not build the default TLS store, dropped every router that needs the `letsencrypt` resolver, and all customer apps returned HTTP 000/404. Fix: infra-stack.yml service secrets now list `[[.CertSecretName]]`/`[[.KeySecretName]]` (the versioned names) so the mount path matches the dynamic config. Host certs already used versioned names and were unaffected. Regression test: `TestLoadComposeFile_InfraBYOMode` now asserts `- cert_v001`/`- key_v001` in the service secrets array. Found live on prod nextrum-sy-1 after the v0.2.155 rollout.

## v0.2.155.1 (2026-10-04)

- **fix(cluster): rebuild-on-missing also applies to cluster-managed secrets' reuse path (migration regression).** The v0.2.155 content-addressed naming (cert_<sha8> instead of cert_v042) made `EnsureVersionedSecret`'s reuse path return the content-addressed name whenever the stored DB hash matched the current content — without verifying that the swarm object actually exists. On clusters upgraded from the old `_v<N>` naming era the DB hash matches unchanged content while the content-addressed object was never minted, so the infra deploy failed with `secret not found: cert_ab64977f`. Reuse now requires BOTH the stored-hash match AND `SecretExists` on the content-addressed name; a missing object is minted (and old `_v<N>` objects GC'd). Regression test: `TestEnsureVersionedSecret_RebuildsWhenSwarmObjectMissing` (seeds legacy `cert_v042` + matching DB hash, asserts the content-addressed object is minted and the legacy one GC'd). Found live during the v0.2.155 prod rollout on nextrum-sy-1.

## v0.2.155 (2026-10-04)

- **feat(config): configs always live in the swarm — content-addressed names, no
  numbered increments.** Per the design directive (configs always come from the swarm;
  the DB is the index but still holds values so it can rebuild missing swarm objects;
  no numbered increments anywhere), every config and secret now uses content-addressed
  naming: `<base>_<sha256-first-8-hex>` (`store.SwarmConfigName` / `store.SwarmSecretName`).
  Identical content → identical name → reuse; a real change mints a new name. First
  creation gets the bare name when the hash is short enough — no `_v1` at creation.
- **Swarm-first translate.** `stacks.StoreConfigResolver` now reads config VALUES from
  the Docker config (`ConfigInspect` of the name derived from the DB row hash) when a
  Docker client is available; if the swarm object is missing it REBUILDS it from the DB
  value (labels `io.pmcluster.managed` / `pmcluster.base`) and falls back to the DB row
  content when Docker is nil (CLI remote/tests). `config_path()` mounts also resolve via
  the swarm.
- **Rebuild-on-missing repair.** `cluster update` gained a "Repairing swarm configs and
  secrets from the DB index" pass: every indexed config/secret row is ensured to have a
  matching swarm object, created from the DB value when absent (secrets decrypted via the
  cipher; non-recoverable ciphertexts warn and skip).
- **CLI + console materialize swarm objects.** `config create/edit/rollback` and
  `secret create/edit` mirror their objects into the swarm (`mirrorSwarmConfig` /
  `mirrorSwarmSecret`, content-addressed, skip-when-exists). The daemon wires
  `StoreConfigResolver{Docker}` and `configs.Local{Docker}`.
- **Writer wiring.** `stacks.Service.configExternalName` feeds `ComposeWriter.ConfigNames`
  at all four render sites so `config_path()` mounts auto-reference the versioned swarm
  config; `secretExternalName` is content-addressed (hash-based, not `_v<rev>`).
- Removed the numbered-increment naming everywhere: platform `EnsureConfig` /
  `EnsureVersionedSecret` mint content-addressed names with GC of stale `<base>_*`
  objects; secret/config `_v<rev>` counters are gone (migration 0023 `swarm_rev` remains
  on the table but is no longer used for naming).

## v0.2.154 (2026-10-04)

- **feat(dsl): `secret(<name>)` prints a secret VALUE as-is — replaces the platform
  placeholder sentinels.** New reference kind `secret(name)` (whole env value or
  inline) resolves to the secret's *value* (unlike `secrets(name)`, which mounts a
  Docker secret file at `/run/secrets/<name>`). Manifest translation resolves it via
  an optional `SecretValueResolver`; refs.ReplaceRefs resolves it via an optional
  `SecretValueResolver` interface; malformed-ref validation advertises `secret(name)`.
- **Platform observability now uses the shared reference language instead of bespoke
  placeholders.** `observability-stack.yml` env `ZO_ROOT_USER_PASSWORD` is now
  `secret(oo_admin_password)` (OpenObserve only accepts the root password via env —
  it cannot read it from a mounted secret file); `otel-collector-config.yml` exporter
  `Authorization` is now `secret(oo_basic_auth)`. Both resolve through
  `renderRefResolver.ResolveSecretValue` / a new refs-style `otelRefResolver` in
  `internal/cluster/templates.go`. The `__OPENOBSERVE_PASSWORD__` /
  `__BASIC_AUTH_PLACEHOLDER__` sentinels are gone; `RenderOTelCollectorConfig` now
  renders through `refs.ReplaceRefs`. `renderRefResolver.ResolveSecret` restored
  (maps cert/key via `secretNameAliases`) so it satisfies the full `refs.RefResolver`
  surface.
- **Tests:** `refs_test.go` `TestReplaceRefs_SecretKind` + `TestParseEnvRef_SecretKind`
  (secret refs resolve via `SecretValueResolver`, error + text-preservation without
  one); `translate_test.go` `TestTranslate_SecretEnvRef` (secret() value lands in env
  verbatim, resolver-missing and validate-path error messages mention
  `secret(name)`).

## v0.2.153 (2026-10-04)

- **feat(dsl): `expose.external` publishes a real swarm port; `protocol` dropped.**
  `expose` gained `external: <port>` — when set, the exposed container port is also
  published to the swarm ingress (`target=port`, `published=external`, TCP), so a
  service can be reached directly (not only through Traefik). `host` is now only
  required when `external` is unset: `expose` with `external` and no `host` publishes
  the raw port with no Traefik router or routing-network membership (used for the OTel
  collector's node-local OTLP endpoint and Traefik's own 80/443). `ports` dropped the
  `protocol` field (all publishing is TCP) and gained `mode` (`ingress`/`host`).
  Platform embeds updated: `otel-collector` now uses `expose {port: 4318, external:
  4318, mode: host}` instead of a `ports` list.
- **feat(dsl): `config_path(<name>)` helper unifies config file mounting.**
  `configs:` entries are now `config_path(<name>)` expressions (one syntax for app and
  platform DSL). The config is mounted at the resolved path (default `/etc/<name>`)
  and the rendered compose references the versioned Docker config automatically.
  Traefik dynamic config → `/etc/traefik/dynamic/conf.yml`, OTel collector config →
  `/etc/otel-collector-config.yaml`. Validation errors for malformed expressions.

## v0.2.152.2 (2026-10-04)

- **fix(cluster): storage node is defaulted before platform stacks are rendered.**
  `cluster up` used to default `storage_nodes` to the leader hostname in
  `persistInstallState` — i.e. AFTER the platform stacks were deployed. The first
  `cluster update` therefore switched `backup_volume-backup` from replicated to global,
  which Docker rejects in place (`service mode change is not allowed`). The defaulting
  now runs as its own early workflow step (`Defaulting storage node`, extracted into a
  `defaultStorageNode` helper) before the render is built, so the backup agent is
  deployed global and storage-node-constrained from the very first bring-up and the
  first update is a content-aware no-op. Regression test:
  `TestUp_BackupRendersGlobalWhenStorageNodeDefaulted`.

## v0.2.152.1 (2026-10-04)

- **fix(cluster): legacy zero-padded name migration in EnsureConfig /
  EnsureVersionedSecret.** Clusters that minted `cert_v042` / `key_v042` /
  `pmcluster_otel_config_v005` under the old `%03d` naming were broken by the
  v0.2.152 naming unification: the reuse path reconstructed the current name as
  unpadded (`cert_v42`) without verifying it exists in the swarm, so the next
  `cluster update` / `cluster up` failed with `secret not found: key_v42` /
  `config not found: pmcluster_otel_config_v4`. The fix tracks the ACTUAL
  highest-version name present in the swarm (`maxName`) and reuses it verbatim;
  only genuinely new content mints `_v<N+1>`. Regression tests:
  `TestEnsureVersionedSecret_ReusesLegacyPaddedName`,
  `TestEnsureConfig_ReusesLegacyPaddedName`.

## v0.2.152 (2026-10-04)

- **feat: ONE pipeline for everything (consolidated L4 directive).** Platform stacks —
  `infra`, `edge`, `observability`, `backup`, `sso` — are no longer rendered as raw
  Compose via a separate path. The five embedded manifests are now DSL manifests
  (`app.platform: true`) that flow through the SAME pipeline as customer app stacks:
  `LoadComposeFile` runs the Go-template pre-pass (ACME/SSO/backup-branch conditionals
  + `${DOMAIN}`-style substitution) and then hands the result to
  `manifest.Parse → Interpolate → Validate → BuildIR → ComposeWriter`. One renderer,
  one hash, one deploy/reconcile/purge path.
- **feat: DSL surface extended for platform needs.** `app.networks` + per-service
  `networks` (join shared external overlays; the per-stack private net is skipped when
  app networks are set), `app.volumes` map (platform named volumes like
  `openobserve_data`/`traefik_acme`/`pmui-data` are declared verbatim — never relocated),
  `mode: global`, `restart`/`restart_delay`, raw `constraints`, `binds` (host paths
  emitted verbatim), `ports` (target/published/protocol/mode), `configs` mounts,
  `extra_hosts`, `resources` (cpus/memory reservations+limits), `user`, raw `labels`,
  `logging` (driver+options), `healthcheck.start_period`. New `settings(name)` env-ref
  kind resolves cluster settings at render time (`settings()` env refs, `ResolveSetting`
  on the resolver). `io.pmcluster.platform=true` is stamped on every service of a
  platform stack.
- **feat: unified rendered-config primitive.** The two non-service platform artifacts —
  the Traefik dynamic config and the OTel collector config — are now handled by one
  `RenderedConfig` abstraction (`EnsureRenderedConfigs`), so they version/persist/render
  exactly like every other platform artifact.
- **feat: versioning unified (user: keep the rev counter).** `EnsureConfig` /
  `EnsureVersionedSecret` now mint unpadded `_v<N>` names (were `_v%03d`), matching the
  user-secret swarm_rev counter. `cert_v1`, `pmcluster_otel_config_v1`, etc.
- **feat: user manifests may not claim `platform: true`.** The deploy/webhook path
  refuses the flag (reserved for pmcluster's own stacks), so a customer manifest cannot
  stamp `io.pmcluster.platform=true` on swarm services.
- Migration note: platform stack rendered output changes shape (env maps, label maps,
  long-syntax ports, platform label), so the first `cluster update` after this release
  performs one forced redeploy of the platform stacks — BUG-009 (fixed in v0.2.148)
  makes a failed redeploy retry on the next update.

## v0.2.151 (2026-10-04)

- **fix(cli): `credentials rotate` now applies the rotation immediately (BUG-012).** Rotating a
  credential (e.g. `openobserve_admin`) updated the DB row + re-materialized the swarm secret,
  but dependent configs (`pmcluster_otel_config` embeds the OpenObserve basic-auth header) and
  stacks (`observability` — OO reads `ZO_ROOT_USER_PASSWORD` at start) were only refreshed by a
  manual `cluster update`, so the rotation had no runtime effect until then. `runCredsRotate` now
  runs the cluster-update pipeline after a successful rotate (via an injectable `credsUpdateFn`
  seam + `applyRotatedCredential` helper), mirroring the per-host cert refresh. Regression tests
  `TestApplyRotatedCredential_TriggersClusterUpdate` / `TestApplyRotatedCredential_ErrorSurfaces`
  in `internal/cli/credentials_test.go`. Found live during Test Case 11 (credentials rotate).

## v0.2.150 (2026-10-04)

- **fix(cluster): per-host certs are now mounted into the Traefik service (BUG-011).** The
  Traefik dynamic config correctly referenced `hostcert-<host>_vNNN` / `hostkey-<host>_vNNN`
  in `tls.certificates`, but the `infra` stack never mounted those versioned secrets into
  the `traefik` service — so Traefik logged `Unable to parse certificate ... failed to find
  any PEM data` and served the default cert for per-host SNI. The `infra-stack.yml` template
  now iterates `HostCerts` and mounts every cert/key secret (service `secrets:` list + top-level
  `external: true` declarations). Regression test asserts both secrets appear in the rendered
  infra stack.
- Found live during Test Case 7 (per-host TLS) on the single-manager test node: BUG-010
  (row persisted after refresh) is fixed in v0.2.149, and BUG-011 (secrets not mounted)
  completes the per-host TLS path.

## v0.2.149 (2026-10-04)

- **fix(cluster): `tls hosts add` now applies the per-host certificate immediately (BUG-010).** `ApplyCert` ran the refresh pipeline (which re-renders the Traefik dynamic config from the `site_certs` table) *before* persisting the new row, so the first per-host cert was stored but Traefik kept serving the default certificate until some later `cluster update`. The per-host row is now persisted before the refresh. Regression test `TestApplyHostCert_PersistsBeforeRefresh` (fails on the old ordering, passes with the fix). Found during Test Case 7 (per-host TLS).

## v0.2.148 (2026-10-04)

- **fix(cluster): a failed platform-stack deploy no longer marks the stack
  up-to-date (BUG-009).** During the v0.2.147 rollout the backup stack's switch
  to storage-node mode hit Docker's "service mode change is not allowed", and
  `cluster update` then refused to redeploy it forever: the rendered hash was
  stamped into the store *before* the deploy ran, so a failed deploy left the
  hash matching the fresh render and every subsequent update skipped the stack
  even though the swarm never received it. Now each stack's rendered snapshot is
  stamped only *after* its deploy succeeds — a failed deploy leaves the stored
  hash stale and the next `cluster update` retries. Regression test
  `TestUpdate_FailedDeployLeavesStaleHashRetriedOnNextUpdate` proves the fix
  (fails on the old code: hash changes after the failed deploy + backup never
  redeployed).

## v0.2.147 (2026-10-04)

- **feat(cluster): backups run only on storage nodes.** The volume-backup agent is
  now constrained to nodes carrying the `pmcluster.storage=true` node label
  (label const `runtime.StorageNodeLabel`). Three-way rendering in the backup
  stack: `backup_all_nodes=true` → global on every node; otherwise when the
  `storage_nodes` setting is non-empty → global constrained to
  `node.labels.pmcluster.storage == true`; otherwise → the legacy replicated /
  platform-node / manager fallback. The label is set automatically when the
  default storage node is recorded at `cluster up` (leader), when
  `pmcluster join --storage-node` registers a node, and repaired on every
  `cluster update` so `storage_nodes` and node labels can't drift.
- **test:** three-way backup template rendering, up default-storage labels the
  leader, update repairs labels for every storage node, join `--storage-node`
  also runs `docker node update --label-add pmcluster.storage=true`.
- Requires a `docker stack deploy` of the backup stack (cluster update) to take
  effect; OTel collector already runs `global` (one per node) and OpenObserve
  log/metric ingestion is unchanged.

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
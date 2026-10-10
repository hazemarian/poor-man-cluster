# 04 — Bug catalogue

Forty tracked defects were found by the TC1–TC10 campaigns (and the
pre-production/production rollouts that followed them): **BUG-001 … BUG-034**,
the TC10 short series **BUG-03 / BUG-05 / BUG-06 / BUG-09**, and **BUG-A**.
All but three were fixed and re-verified on the same hardware; **BUG-027** was
cancelled as not fixable from our side, and **BUG-033 / BUG-034** were still
open at v0.2.179.

### A note on the numbering

Two numbering series coexist, and this is a real footgun for anyone grepping
the history:

- **TC1–TC9** use a single continuous series `BUG-001 … BUG-018`.
- **TC10** continues that series from `BUG-019a` but *also* introduced its own
  short series `BUG-03 / BUG-05 / BUG-06 / BUG-09` for the pre-production
  blockers (these are **not** the same as BUG-003/005/006/009 from the earlier
  campaigns — the TC10 reports use two-digit names without the leading zero).
- **BUG-A** exists only in the session history and the TC10 narrative; it has
  no entry in `CHANGELOG.md`.
- **BUG-013 does not exist anywhere in the repository** — the number was never
  used. Treat the gap as a recording error, not a missing fix.

Where a bug was found *only* because of a live campaign, that is called out.
Roughly half of this catalogue is of that kind: unit tests and golden files
did not catch them because they are failures of *composition* (two correct
features interacting on real hardware), not of a single function.

---

## Theme A — Data integrity & persistence

### BUG-002 — `cluster update` does not create storage dirs when `volume_root` changes
- **Campaign:** TC2 (single manager, backup/restore/S3/ACME). **Severity:** Medium.
- **Symptom:** setting `volume_root` then running `cluster up` cleared it;
  re-setting it and updating did not create the new root, so every deploy
  failed on a missing bind source. Documented as a *workaround* in the TC2
  checklist before the fix landed.
- **Root cause:** no code path recreated the root on update — only on bring-up.
- **Fix:** v0.2.143 (`0acde7a`) — `cluster update` calls `ensureStorageDirs`.
- **Verification:** TC2 §5 checklist item flipped from workaround to ✅.

### BUG-003 — `backup restore --dest-root` defaults to a hardcoded `/var/stack/data`
- **Campaign:** TC2. **Severity:** Medium.
- **Symptom:** on a cluster with a custom `volume_root`, restore wrote to the
  wrong tree.
- **Root cause:** the flag default was a literal, not the `volume_root` setting.
- **Fix:** v0.2.143 (`0acde7a`) — the default resolves from `volume_root`, on
  both the CLI and the REST API.
- **Verification:** TC2 §6.

### BUG-004 — Multi-process SQLite WAL corruption (data-loss risk)
- **Campaign:** TC3. **Severity:** High.
- **Symptom:** the daemon and the CLI disagreed about which stacks existed; the
  daemon held `data.db-wal (deleted)` + `data.db-shm (deleted)` descriptors;
  `PRAGMA integrity_check` reported a torn index; CLI-created users returned
  401 everywhere.
- **Root cause:** WAL mode + a second process that opened, wrote and closed the
  database. SQLite deletes the `-wal`/`-shm` sidecars on close even though the
  long-running daemon still holds them; the daemon then keeps writing to the
  orphaned inode → split-brain database.
- **Fix:** v0.2.144 (`6a45768`) — both stores switched to
  `journal_mode=DELETE`; `WALCheckpoint` became a no-op. Regression test
  `TestMultiProcessNoWALSplitBrain` asserts no sidecars ever exist
  (`docs/test-reports/TC3-…md:50-94`, DEC-07).
- **Verification:** live on `nxt-sw-3-w`: `REINDEX` repaired the torn index; a
  `python3` write+close no longer orphaned anything.

### BUG-005 — Scoped/unscoped CLI-created tokens returned 401
- **Campaign:** TC3. **Severity:** High.
- **Root cause:** *not a separate bug* — the symptom of BUG-004. The daemon's
  connection was wedged on the orphaned WAL inode, so rows written by CLI
  processes were invisible to `UserByToken`. Recorded separately because it was
  reported as an auth failure first.
- **Verification:** immediately after the WAL fix, fresh users authenticated
  (200).

### BUG-006 — Leadership-gain restore clobbered the live store
- **Campaign:** TC3. **Severity:** High.
- **Symptom:** the leadership goroutine restored from a Raft state config
  *after* `store.Open`; `extractKit` truncated `data.db` under the daemon's
  live connection. A CLI-created user was auth-200 to the daemon (stale page
  cache) but absent from fresh reads.
- **Fix:** v0.2.145 — control-plane restore became **startup-only** (before
  `store.Open`); on promotion the daemon only snapshots (DEC-07a).
- **Verification:** live log now shows a snapshot on promotion, never a restore.

### BUG-007 — Secret rotation silently defeated for live stacks
- **Campaign:** TC4. **Severity:** Medium/High.
- **Symptom:** `pmcluster secret edit` updated the DB, but a running service
  kept the old value with only a WARN line — Docker secrets are immutable and
  in-use objects cannot be updated.
- **Fix (interim):** v0.2.146 — versioned names `name_v<rev>`; the writer emits
  the versioned external name while the container path stays
  `/run/secrets/<logical-name>`.
- **Follow-up:** this is the bug that forced the L4 unification and, ultimately,
  DEC-03 (content addressing). Re-verified live in TC5.

### BUG-031 — Storage failover restored *another node's* archive (empty database)
- **Campaign:** TC10-B. **Severity:** **Critical** — silent data loss
  immediately after an automatic failover.
- **Symptom:** after a storage node went down, postgres came up with an **empty**
  database instead of the failed node's data.
- **Root cause:** the archive search took the *globally newest*
  `backup-*.tar.gz` in the store, which on a multi-storage-node cluster can be
  a different node's whole-disk archive.
- **Fix:** v0.2.175 — select only objects whose `backup-<nodeID>-…` name
  matches the **source node**, plus a mover-side guard that verifies the tarball
  contains `backup/data/<stack>/` **before** `rm -rf` on the target.
- **Verification:** `move_store_test` guards assert the content check precedes
  the erase; live failover ends with the seeded row intact.

### BUG-032 (+follow-up) — `backup restore` wrote into the volume root of the node the CLI ran on
- **Campaign:** TC10-C. **Severity:** High.
- **Symptom:** the restore "succeeded" while the stack's own node stayed empty
  and postgres re-initialised.
- **Root cause:** the restore path never resolved the stack's owning node.
- **Fix:** v0.2.175 — resolve the owning node and route through the
  store-transit mover; fail loudly when routing is impossible
  (`this stack's volume lives on <node> — run the restore there`). Follow-up:
  the routed restore ignored `--from-s3`.
- **Verification:** `TestRestoreDestination` (decision matrix),
  `TestRestoreArchiveToNode_RoutesMoverToOwner`; live restore from the store
  **and** from `--from-s3`.

---

## Theme B — Control plane & cluster lifecycle

### BUG-001 — `WatchSwarmLeadership` re-emitted `leader=true` on every poll
- **Campaign:** TC1 (found on the first bring-up). **Severity:** High.
- **Symptom:** only one `pmcluster_state_*` snapshot existed (from startup) no
  matter how many deploys ran; the restore path logged every ~15 s.
- **Root cause:** in `internal/cli/leader.go`, the `case leader:` branch emitted
  `true` and then reset `current, first = false, false` — so the next 15 s poll
  re-emitted `leader=true` forever, and the serve goroutine treated each emit as
  "leadership gained", cancelling and restarting **both** loops. The 5-minute
  snapshot ticker was cancelled 15 s after it armed.
- **Fix:** commit `e145d6f` — the leader branch sets `current, first = true,
  false`.
- **Verification:** `TestWatchSwarmLeadership_StableLeaderEmitsOnce`,
  `TestWatchSwarmLeadership_LeaderToNotLeaderReEmits`; live: exactly one
  `became swarm leader`, a fresh snapshot at the next tick.
- **Note:** found *by a test campaign on day one*, before any release carried
  the fix — the campaign's value in one line.

### BUG-009 — A failed platform-stack deploy permanently marks the stack up-to-date
- **Campaign:** TC4 (found during the v0.2.147 rollout). **Severity:** High.
- **Symptom:** switching the backup stack to storage-node mode hit Docker's
  "service mode change is not allowed"; the deploy failed, but every later
  `cluster update` reported "nothing to redeploy" forever.
- **Root cause:** the rendered hash was written to the store **before** the
  deploy ran, so `hash == ConfigHash(fresh)` even after the failure.
- **Fix:** v0.2.148 — stamp only **after** success.
- **Verification:** `TestUpdate_FailedDeployLeavesStaleHashRetriedOnNextUpdate`
  (fails on the old code). This invariant is now load-bearing for the whole
  drift model (DEC-08a).

### BUG-020 — A daemon starting while the swarm has no Raft leader crash-loops
- **Campaign:** TC10-A. **Severity:** High.
- **Symptom:** 28 restarts in ~2 min on `nxt-sw-4-m`, each carrying Docker's
  `The swarm does not have a leader …`.
- **Root cause:** the startup restore treated the no-quorum error as fatal;
  systemd restarted forever.
- **Fix:** v0.2.167 — `controlplane.IsSwarmUnavailable` recognises
  `does not have a leader` / `too few managers are online`; snapshot and
  restore become logged no-ops and the daemon stands by.
- **Verification:** a six-case `IsSwarmUnavailable` table (deliberately
  falsified by disabling both patterns) + live: no restart storm.

### BUG-021 / BUG-021b — The generated systemd unit did not bring the daemon back with Docker
- **Campaign:** TC10-A. **Severity:** High.
- **Symptom:** `systemctl stop docker && systemctl start docker` left `pmcluster`
  inactive on the standby managers — a manager returns with no control loop;
  if it is (or becomes) the leader it stops reconciling silently.
- **Root cause:** the unit was not in Docker's `WantedBy` set. v0.2.167's
  `BindsTo=docker.service` propagates a *stop* but not a *start*.
- **Fix:** v0.2.167 `BindsTo=docker.service`; v0.2.168.2
  `WantedBy=docker.service`; v0.2.168.1 makes the standby path refresh the unit.
- **Verification:** `TestRenderSystemdUnit` asserts `WantedBy=docker.service`;
  live `systemctl is-active pmcluster` → `active` after a docker-only restart.

### BUG-022 — A standby manager fell into the interactive setup wizard on `cluster update`
- **Campaign:** TC10-A. **Severity:** Medium.
- **Root cause:** a manager that never won a Raft election keeps an
  intentionally empty local store; `cluster update` used "no stored `domain`"
  as the test for "no cluster".
- **Fix:** v0.2.168 — `classifyLocalCluster` distinguishes **provisioned** /
  **standby** / **fresh**; a standby prints one line and exits 0.
- **Verification:** `TestClassifyLocalCluster` (five cases); live re-run exits 0.

### BUG-023 — A long-running replicated service whose container exits cleanly is never rescheduled
- **Campaign:** TC10-A. **Severity:** High.
- **Symptom:** after a Docker restart, `edge_pmcluster-edge` and
  `backup_control-plane-backup` sat at 0/1 — Swarm marks a zero-exit task
  `Complete`, and `condition: on-failure` ignores exit 0.
- **Root cause:** the writer rendered `on-failure` for the default replicated
  case. Global services already used `any`, which is exactly why they recovered
  unaided.
- **Fix:** v0.2.168.2 renders `condition: any` for the default case (an explicit
  `restart:` in a manifest still wins); v0.2.168.3 stops the four platform
  services that forced `on-failure` from overriding it.
- **Verification:** translator goldens + live: every platform service renders
  `{"Condition":"any","Delay":5000000000}` and comes back unaided.

### BUG-024 — The leader is not always a storage node (policy bug)
- **Record:** `CHANGELOG.md:284` (v0.2.170), from the user directives quoted in
  DEC-08. **Severity:** High (the backup agent and edge console assume the
  leader's node holds the data).
- **Symptom:** a manager could hold leadership without being a storage node;
  failover/backup then had no local archive to work from.
- **Fix:** `cluster up` seeds the leader into `storage_nodes`; every
  `cluster update` re-adds the current leader; v0.2.171 makes storage follow
  leadership (promote the new leader, demote the former, clear the label).
- **Verification:** TC10-E CLI sweep (promote/demote) and the leader-failover
  slice (TC10-A item 2).

### BUG-033 — A worker's daemon runs a full reconcile loop it cannot perform — **OPEN at v0.2.179**
- **Found during:** the production rollout after TC10. **Severity:** Low/Medium.
- **Symptom:** a worker spams `ERR` on every ~3 s pass:
  `platform pass failed: this node is a Swarm worker, not a manager`, plus
  `service list failed` per stack.
- **Root cause (diagnosed):** the manager-only passes run on a worker where
  those APIs always fail; the errors are logged at `ERR` rather than skipped.
- **Status:** **NOT FIXED.** The v0.2.176 worker-standby work fixed the *startup*
  path (`leader.go:52` — one line, then idle); the reconcile-loop path for a
  worker that was previously a manager is still noisy.

### BUG-034 — Two orphaned stacks in the worker's local DB fail manifest parsing every pass — **OPEN at v0.2.179**
- **Found during:** the same rollout. **Severity:** Low.
- **Symptom:** `abbas-test` and `simple-erp` fail every pass with
  `json: cannot unmarshal array into Go struct field App.volumes of type
  map[string]string`.
- **Root cause:** a legacy manifest written before `App.volumes` became a map;
  the stored JSON still has an array, so the decode fails forever.
- **Status:** **NOT FIXED** — needs a migration/compat decode, or a way to prune
  orphaned local rows so one stale record cannot fail the pass indefinitely.

---

## Theme C — Backups, storage and failover

### BUG-012 — `credentials rotate` had no runtime effect
- **Campaign:** TC6. **Severity:** Medium.
- **Symptom:** the DB and the Swarm secret were updated, but OpenObserve still
  accepted the old password and the OTel config header was unchanged until a
  manual `cluster update`.
- **Fix:** v0.2.151 — rotate triggers the cluster-update pipeline automatically.

### BUG-015 (two rounds) — Mover transit unreachable across hardened nodes
- **Campaign:** TC8. **Severity:** High.
- **Symptom:** cross-node `stack move` mover tasks failed with a non-zero exit;
  the stack never lost data.
- **Root cause, round 1:** the mover built the archive URL from the leader's
  **public** advertise address; on the hardened cluster the public IP silently
  drops SYN while the private interface works. Fix (v0.2.158.1): try every
  non-loopback local interface, deduped.
- **Root cause, round 2:** busybox `wget -T 10` does not reliably bound a
  silently-dropped connect, and a stale target subtree blocked `mv`. Fix
  (v0.2.158.2, `b4ddb4e`): RFC1918 private addresses first, advertise address
  last; `rm -rf /data/<stack>` before `mv`.
- **Superseded by:** BUG-030, which removed cross-node host transit entirely.

### BUG-016 — The store bucket is not bootstrapped for store-only clusters
- **Record:** `CHANGELOG.md:632` (v0.2.162.1). **Severity:** Medium.
- **Symptom:** a cluster configured with the in-cluster store but no offsite S3
  had no bucket to write to.
- **Fix:** v0.2.162.1 — bucket bootstrap on the store leg.

### BUG-025 — `backup list` only saw the archives this node produced
- **Campaign:** TC10-C. **Severity:** High.
- **Root cause:** archives uploaded by other nodes' agents land in the store but
  never pass through the local `Trigger` path, so no DB row existed off-node —
  restore and failover could not use them.
- **Fix:** v0.2.170 — `discoverStore` indexes the store (S3 ListObjectsV2,
  prefix `backup-`, deduped, `pmcluster-ctlplane-*` excluded). It only became
  usable once BUG-029 fixed the request signature.
- **Verification:** `internal/backups/s3_list_test.go`; live `backup list`
  shows the other nodes' tarballs.

### BUG-028 — The offsite IONOS leg failed with `403 SignatureDoesNotMatch`
- **Campaign:** TC10-C. **Severity:** High — no offsite copy, i.e. no DR source.
- **Root cause:** two stacked faults — (a) the rendered env set
  `AWS_S3_FORCE_PATH_STYLE=true`, a knob offen does not implement, so it fell
  back to virtual-host style and IONOS rejected the signature; (b) the IONOS key
  had been rotated provider-side.
- **Fix:** v0.2.172 renders `AWS_S3_BUCKET_LOOKUP: "path"`; a fresh key was
  issued and applied.

### BUG-029 — Our SigV4 signer omitted `x-amz-date` from `SignedHeaders`
- **Campaign:** TC10-C. **Severity:** High — every store-backed read failed, so
  automatic failover could not restore.
- **Root cause:** `internal/backups/s3.go` signed only
  `host;x-amz-content-sha256` while still sending `x-amz-date`; strict verifiers
  recompute over exactly the headers listed in `SignedHeaders`. IONOS tolerated
  the omission — only the in-cluster store path failed, which is how it
  survived earlier testing.
- **Fix:** v0.2.173 — include `x-amz-date` in the canonical headers and in
  `SignedHeaders`, for both `fetchS3Object` and `listS3ObjectsPage`.
- **Verification:** byte-exact repro on the node (old form → 403, standard form
  → 200); `TestSignV4Headers_SignsAmzDate`.

### BUG-030 (+3 follow-ups) — Cross-node move/failover transit depended on an ephemeral host port
- **Campaign:** TC10-B. **Severity:** High.
- **Symptom:** the failover reached the transit step and the mover died with
  `wget: download timed out`.
- **Root cause:** unattended recovery depended on an **undocumented firewall
  rule** — hardened hosts only open 2377/7946/4789 per peer.
- **Fix:** v0.2.174 removes cross-node transit: the mover service on the
  **target** pulls the archive object straight from the object store and
  unpacks the `<stack>` subtree. Follow-ups: **.1** a container cannot use the
  daemon's `127.0.0.1:8333` (use `host.docker.internal`), timeout 120→240 s;
  **.2** `docker service create` rejects `--host-add` (use
  `--host host.docker.internal:host-gateway`); **.3** a hung
  `docker service create` holding the CLI's pipes stalled the move, so the
  create runs asynchronously against a temp file.
- **Verification:** `TestStorePullMoverScriptAndArgs`, `TestMoverEndpoint`,
  `TestStartMoverService_HangingCreateIsTolerated`; live moves and failovers
  complete with data intact.
- **This is DEC-10**, and it is the clearest example in the catalogue of a bug
  that is really an architecture decision discovered by hardware.

### BUG-A — OpenObserve OOM-starves a 1-core manager (found in the session history, narrated in TC10)
- **Found:** 2026-10-08, session `ses_ee234a16fffebAYvvTgksndBgE`, mid-run of
  the pre-production campaign. **Severity:** High.
- **Symptom:** `nxt-sw-4-m` (1 core / 1.8 GB) went `Down / Unreachable`, SSH
  banner timeouts, `load average: 12.69` with `kswapd0` active and OpenObserve
  at 331 MB RSS / 17.5% mem — for roughly an hour, mid-campaign.
- **Root cause:** OpenObserve had **no placement guard**. With `platform_node`
  empty, platform placement fell back to `node.role == manager`, a no-op on an
  all-manager swarm, so a JVM-class workload landed on a 1-core node. This is
  the same defect as TC10's **BUG-09**; BUG-A is the *observed outage*, BUG-09
  is the *tracked fix*.
- **Fix:** v0.2.175 — `platform_node` defaults to the leader (pinning
  OpenObserve, the edge and `control-plane-backup` to the big node), and the
  store node avoids the platform node.
- **Verification:** `TestUpdate_DefaultsPlatformNodeToLeader`,
  `TestPickStoreNode_AvoidsPlatformNode`; live: OpenObserve/edge/
  control-plane-backup on `nxt-sw-1-m`, store on `nxt-sw-3-w`; 4-m load fell
  to 0.35.
- **Status:** confirmed fixed — but only for clusters that have received the
  fix. A stale task on an un-upgraded 1-core node will starve it exactly the
  same way.

---

## Theme D — Drift detection & the reconcile contract

### BUG-017 — `cluster update` cannot see live-Swarm drift
- **Campaign:** TC9 (reproduced deliberately; first seen in production on
  `wafaa`). **Severity:** High.
- **Symptom:** on `wafaa`, `backup_control-plane-backup` ran with 6 env vars
  and no `WEBDAV_URL`/`AWS_*`, while `volume-backup` had all 16 — the
  control-plane archive was landing on the *same node* as the database it was
  supposed to protect, i.e. zero DR value. The stored render was correct; the
  Swarm was stale.
- **Root cause:** `cluster update` compared the stored rendered hash against a
  fresh render and **never inspected the live Swarm**. Once a service drifted
  out of band (orphan task, interrupted apply, manual `docker service update`),
  every later update reported "nothing to redeploy" indefinitely.
- **Fix:** v0.2.163 — the rendered hash is stamped as the
  `io.pmcluster.rendered_hash` label on every service, and `cluster update`
  compares live labels against the fresh render. v0.2.163.2 counts a partially
  applied platform deploy as drift; v0.2.164 extends the same label to
  **app/customer stacks**.
- **Verification:** `TestUpdate_RedeploysWhenLiveServiceLabelDrifted` (fails
  against the old DB-only comparison); the deliberate injection on TC9 produced
  `▶ No rendered content changed` before the fix and a redeploy after.

### BUG-018 — A failed app-stack deploy suppresses retries
- **Record:** `CHANGELOG.md:517` (v0.2.165). **Severity:** High.
- **Root cause:** the app-stack lane stamped the hash before the apply — the
  same class as BUG-009, two years of architecture later, in the other lane.
- **Fix:** v0.2.165 — stamp after the apply, everywhere.

### BUG-019a / BUG-019b — `join --storage-node` needed joiner→manager SSH; stale storage labels
- **Campaign:** TC10-E. **Severity:** Medium.
- **Symptom:** `pmcluster join --storage-node` registered the node by reaching
  the manager over SSH; with no keys the join succeeded but the node never
  became a storage node. Separately, a node dropped from `storage_nodes` could
  keep `pmcluster.storage=true`.
- **Root cause:** the leader's store setting was the only intent channel, and a
  fresh joiner has no path to it.
- **Fix:** v0.2.175 — the joiner stamps the **swarm-visible**
  `pmcluster.storage` label on itself; the leader adopts labeled-but-unlisted
  nodes into `storage_nodes` on the next update; demotion goes through
  `pmcluster node demote` (clears label + setting), superseding BUG-019b.
- **Verification:** live join prints the label line; `cluster update` prints
  `⚠ adopted <host> into storage_nodes`.

---

## Theme E — UI, console, TLS and platform placement

### BUG-008 — The webhook retry policy is dead code
- **Campaign:** TC4. **Severity:** Medium.
- **Symptom:** `MaxRetries`/`RetryDelay` were wired but never exercised;
  delivery rows stayed `retries:0`.
- **Fix:** v0.2.146 — the receiver retries under a phase budget (2 × 30 s) on a
  detached context that deliberately outlives the router's 30 s per-request
  timeout; the real retry count lands on the delivery row and in the 502 body.

### BUG-010 — `tls hosts add` applied the per-host cert only after a later update
- **Campaign:** TC6 (found live). **Severity:** High.
- **Root cause:** the `site_certs` row was persisted **after** the Traefik
  dynamic-config refresh, so the first render did not include it.
- **Fix:** v0.2.149 — persist the row before the refresh.

### BUG-011 — Host cert secrets were not mounted into the traefik service
- **Campaign:** TC6 (found live, immediately after BUG-010). **Severity:** High.
- **Root cause:** the dynamic config *referenced* the host cert secrets but
  `infra-stack.yml` never mounted them.
- **Fix:** v0.2.150 — the infra stack iterates `HostCerts`; regression test.
- **Note:** two bugs, one symptom (default cert served), found by refusing to
  accept the first fix as the whole answer.

### BUG-014 — Traefik did not run on all nodes
- **Campaign:** TC7. **Severity:** High — this is the production outage DEC-11
  describes.
- **Symptom:** `pmcluster.<domain>` 404'd on ~half of requests behind a load
  balancer; retrying "worked" because the retry landed on a manager.
- **Root cause:** Traefik's Swarm provider lists services through the local
  Docker socket — **only a manager can read it**. A placement-free global
  replica on a worker had no routers, and the ingress routing mesh
  intermittently forwarded to it.
- **Fix:** v0.2.157 (global everywhere, the *wrong* direction) → v0.2.178
  (`placement: manager`, DEC-11).

### BUG-03 — The console's `/api/*` through the edge returned 502 (pre-production blocker)
- **Campaign:** TC10-E. **Severity:** **Critical** — the product's main UI/API
  entry point was down.
- **Symptom:** `GET https://<domain>/api/…` → 502,
  `dial tcp 172.17.0.1:9090: connection refused`.
- **Root cause:** the edge proxies to *its own* node's API, and its placement
  was only `node.role == manager` — a no-op on an all-manager swarm — so the
  edge could sit on a node whose API was not the upstream it dialled.
- **Fix:** v0.2.175 — `platform_node` defaults to the leader (pinning the edge,
  OpenObserve and `control-plane-backup` to it) plus an edge `ErrorHandler`
  returning clean JSON instead of leaking a Go transport error.
- **Verification:** `TestNew_UpstreamUnavailableReturnsCleanJSON502`; live
  console `/api/*` returns 200.

### BUG-05 / BUG-06 — `credentials rotate` split-brain and lost secret mounts (pre-production)
- **Campaign:** TC10-E. **Severity:** High.
- **Symptom:** rotation deleted the Swarm secret *after* writing the store row
  (row says new, Swarm has none); separately, secret mounts that disappeared
  from a render were never re-added, so Traefik routers served 404 with no
  error.
- **Fix:** v0.2.175 — mint the new versioned secret **first**, never remove the
  old one, and add a self-heal step that rebuilds missing rendered
  secret/config mounts.
- **Verification:** live rotation leaves both secrets present and the dashboard
  serving 200/401; `pmcluster secret heal` → `1 ok, 0 degraded, 0 unrecoverable`.
- **This was the bug that produced `secret heal`.**

### BUG-09 — No placement guard for the platform stack (pre-production blocker)
- **Campaign:** TC10-D. **Severity:** **Critical** — ~30 minutes with **no
  swarm leader** and the backup store down.
- **See BUG-A** for the observed outage; the tracked fix is v0.2.175 (default
  `platform_node` to the leader; the store node avoids the platform node).

### BUG-027 — ACME certificates behind a round-robin LB — **CANCELLED**
- **Campaign:** TC10. **Severity:** High for production TLS.
- **Symptom:** each Traefik instance keeps its own ACME store; behind a
  round-robin LB only one node ends up with a valid Let's Encrypt certificate
  while the others serve the default cert, and re-issue attempts make the rate
  limits worse.
- **Root cause:** certificate state is per-node and the LB distributes
  round-robin. **Not fixable from inside the cluster** — cancelled by the user
  before any change; the mitigations are operator-supplied wildcard certs, a
  DNS-01 cert issued outside the cluster, or TLS termination at the LB
  (`docs/network-topology.md:120-133`).

### BUG-026 (+3 follow-ups) — A stateful stack pinned to a node other than the deploying host never starts
- **Campaign:** TC10-B. **Severity:** High.
- **Symptom:** `sfapp` on `nxt-sw-4-m`:
  `failed to populate volume: mount /var/stack/data/sfapp/db_data: no such file
  or directory`.
- **Root cause:** storage directories were created **where the deploy ran**;
  Swarm refuses a task whose bind source is missing, and no code path created
  it remotely (by design — no remote mkdir).
- **Fix:** v0.2.169 — the daemon `mkdir -p`s the roots it owns at startup and
  runs a **node-local volume-repair loop** (startup + every 30 s, leader or
  not) that creates missing volume dirs for services pinned to *this* hostname
  and force-updates them. Follow-ups: .1 resolve the local-driver volume
  `device` (not just raw binds); .2 treat a blank `volume_root` as unset; .3
  start the repair *before* the leadership wait so standby managers run it too.
- **Verification:** the repair line appears on `nxt-sw-4-m`; `sfapp_db` leaves
  `Rejected` with no manual intervention.

---

## Theme F — CI and the recording gaps

- **BUG-013:** never recorded. The number is absent from `CHANGELOG.md`, from
  every test report, and from the git history. Recorded here so the gap is
  visible rather than silently skipped.
- **CI gates that caught things before the campaigns did:** v0.2.166.1 fixed
  `SA5011` (explicit returns after `t.Fatal` nil-checks under Go 1.25) so the
  lint gate stayed green; `daemonExecStart` guards the systemd-unit regression
  (BUG-021); `TestMultiProcessNoWALSplitBrain` guards BUG-004 forever.

---

## Summary table

| Bug | Theme | Severity | Found by | Fix release | Status |
|---|---|---|---|---|---|
| BUG-001 | control plane | High | TC1 | `e145d6f` | fixed |
| BUG-002 | data integrity | Medium | TC2 | v0.2.143 | fixed |
| BUG-003 | data integrity | Medium | TC2 | v0.2.143 | fixed |
| BUG-004 | data integrity | High | TC3 | v0.2.144 | fixed |
| BUG-005 | data integrity | High | TC3 | (BUG-004) | fixed |
| BUG-006 | control plane | High | TC3 | v0.2.145 | fixed |
| BUG-007 | secrets | Med/High | TC4 | v0.2.146 → v0.2.155 | fixed |
| BUG-008 | webhooks | Medium | TC4 | v0.2.146 | fixed |
| BUG-009 | drift | High | TC4 (rollout) | v0.2.148 | fixed |
| BUG-010 | TLS | High | TC6 (live) | v0.2.149 | fixed |
| BUG-011 | TLS | High | TC6 (live) | v0.2.150 | fixed |
| BUG-012 | credentials | Medium | TC6 | v0.2.151 | fixed |
| BUG-013 | — | — | — | — | **never recorded** |
| BUG-014 | networking | High | TC7 (prod outage) | v0.2.157 → v0.2.178 | fixed |
| BUG-015 | storage move | High | TC8 | v0.2.158.1/.2 | fixed (superseded by BUG-030) |
| BUG-016 | backups | Medium | CHANGELOG | v0.2.162.1 | fixed |
| BUG-017 | drift | High | TC9 (prod sighting) | v0.2.163 | fixed |
| BUG-018 | drift | High | CHANGELOG | v0.2.165 | fixed |
| BUG-019a/b | storage nodes | Medium | TC10 | v0.2.175 | fixed |
| BUG-020 | control plane | High | TC10 | v0.2.167 | fixed |
| BUG-021/021b | control plane | High | TC10 | v0.2.167 → .168.2 | fixed |
| BUG-022 | control plane | Medium | TC10 | v0.2.168 | fixed |
| BUG-023 | control plane | High | TC10 | v0.2.168.2/.3 | fixed |
| BUG-024 | storage policy | High | user directive | v0.2.170 | fixed |
| BUG-025 | backups | High | TC10 | v0.2.170 | fixed |
| BUG-026 (+3) | storage | High | TC10 | v0.2.169…169.3 | fixed |
| BUG-027 | TLS/LB | High | TC10 | — | **cancelled** (unsolvable) |
| BUG-028 | backups | High | TC10 | v0.2.172 | fixed |
| BUG-029 | backups | High | TC10 | v0.2.173 | fixed |
| BUG-030 (+3) | storage move | High | TC10 | v0.2.174…174.3 | fixed |
| BUG-031 | failover | **Critical** | TC10 | v0.2.175 | fixed |
| BUG-032 | restore | High | TC10 | v0.2.175 | fixed |
| BUG-033 | control plane | Low/Med | prod rollout | — | **OPEN** |
| BUG-034 | data integrity | Low | prod rollout | — | **OPEN** |
| BUG-03 | platform placement | **Critical** | TC10 | v0.2.175 | fixed |
| BUG-05 | credentials | High | TC10 | v0.2.175 | fixed |
| BUG-06 | credentials | High | TC10 | v0.2.175 | fixed |
| BUG-09 | platform placement | **Critical** | TC10 | v0.2.175 | fixed |
| BUG-A | platform placement | High | live session | v0.2.175 | fixed |

**Three findings are Critical** (BUG-031, BUG-03, BUG-09/BUG-A). Two are open.
One was cancelled as unsolvable. The rest are closed, each with a regression
test that (where noted) *fails on the pre-fix code*.

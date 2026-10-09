# TC10 — Four-Node Storage HA, Leader Failover and the Pre-Production Campaign (Test Case 10)

**Date:** 2026-10-09
**Versions under test:** v0.2.166.1 → v0.2.175 (commit `37e38b1`) — every fix was built and deployed back to the same sandbox as it shipped
**Cluster:** 4-node Docker Swarm sandbox (see Environment)
**S3 offsite / domain:** IONOS eu-central-3 bucket `nextrum-bu`; `nextrum-sy.de` (BYO self-signed cert)

## Objective

Take the platform from the working two-node test bed (TC7–TC9) to a pre-production verdict on four nodes:

1. **TC10-A — leader failover:** stop the leader's Docker, prove Raft re-election plus L2 control-plane survival.
2. **TC10-B — storage failover:** move a stateful app between storage nodes, then drain its storage node and prove the automatic failover end-to-end (detect → pause sync → alternate → source-node archive → store-transit mover → restore → re-pin → marker → badge → ack → move back).
3. **TC10-C — backup + restore:** backup list/restore from the in-cluster store and from the offsite bucket, the control-plane archive contents, and the destructive DR drill.
4. **TC10-D — deploy lifecycle + drift:** `cluster update` idempotency, live-swarm drift detection, config/secret re-minting.
5. **TC10-E — console / CLI / webhooks / tokens:** the console through the edge, the CLI sweep, signed webhooks, badges, per-stack tokens, node promote/demote.

**Verdict:** the campaign table scored **12 PASS / 1 PARTIAL FAIL / 1 FAIL**; the two blockers it found (**BUG-09** platform placement, **BUG-03** console 502) were fixed in v0.2.175 and **every item re-verified PASS → READY FOR PROD ROLLOUT**. 16 bug write-ups below (15 fixed, 1 cancelled), plus 2 open items found during the production rollout.

## Environment

| Host | Public IP | Role (final topology) | Size |
|---|---|---|---|
| nxt-sw-1-m | 217.160.143.22 | Manager — **Raft Leader**, `platform_node` | 2 core / 3.7 GiB |
| nxt-sw-2-m | 217.160.143.24 | Manager | 1 core / 1.8 GiB |
| nxt-sw-4-m | 217.160.143.26 | Manager | 1 core / 1.8 GiB |
| nxt-sw-3-w | 217.160.128.29 | Worker — hosts the **in-cluster backup store** (SeaweedFS, pinned by hostname) | 1 core / 1.8 GiB |

- The swarm opened the campaign as **four managers** (the CHANGELOG entries for this campaign call it the "four-manager sandbox"); the **final topology is 3 managers (nxt-sw-1-m Leader) + the worker nxt-sw-3-w**. nxt-sw-3-w was a **standby manager** (joined, never promoted) during TC10-A — that is where BUG-022 was found — before it ended up as the plain worker above.
- `platform_node=nxt-sw-1-m`: OpenObserve, the edge and `control-plane-backup` pin there (v0.2.175 defaults it to the leader).
- **Storage nodes** = the managers (v0.2.170/.171 keep the *current* leader in `storage_nodes` and clear the former leader); **backup store** = SeaweedFS in-process S3+WebDAV (`-s3 -s3.port=8333 -webdav -webdav.port=7333`) pinned to **nxt-sw-3-w — the non-storage worker**, never to the platform node (BUG-09 rule).
- **Offsite:** IONOS `nextrum-bu`; both offen agents double-write (WebDAV to the store + S3 offsite).

## Procedure

Each sub-test is repeatable on its own; fixes were redeployed between runs and the affected step re-run.

### TC10-A — leader failover

```bash
ssh nxt-sw-1-m 'systemctl stop docker'                 # 1. kill Docker on the current Raft leader
watch -n1 'docker system info | grep -i -A1 raft'      # ~25 s → a new Raft leader elected
pmcluster cluster status                              # L2 survivor restored from the Raft state config
journalctl -u pmcluster -f                            # BUG-020: pre-v0.2.167 restarted every ~5 s ("The swarm does not have a leader …")
systemctl stop docker && systemctl start docker && systemctl is-active pmcluster   # BUG-021/021b
ssh nxt-sw-3-w 'pmcluster cluster update'             # BUG-022: pre-v0.2.168 → interactive wizard, exit 1
docker stack services edge backup                     # BUG-023: edge 0/1, control-plane-backup 0/1, volume-backup 1/2
# a second consecutive leader loss was run too and recovered the same way
```

### TC10-B — storage failover

```bash
pmcluster deploy /root/tests/sfapp.yaml             # postgres `sfapp`, volume db_data pinned to a storage node
pmcluster stack move sfapp --to nxt-sw-4-m          # manual move; seeded row intact after it
docker node update --availability drain <storage-node>
# leader daemon log, in order:
#   WRN reconcile — storage node down; pausing app sync (clears when the node returns or the stack is moved)  stack=sfapp
#   WRN reconcile — storage failover: moving stack to a healthy storage node (S3 restore)  from=<down> to=<target>
#   INF reconcile — storage failover move completed  stack=sfapp to=<target>
pmcluster stack show sfapp                          # failover marker present (from/to/at/acked)
curl -s https://<domain>/api/public/badge/sfapp     # amber "failover"
pmcluster stack ack sfapp                           # badge healthy
pmcluster stack move sfapp --to <original-node>     # marker cleared, seeded row intact
docker node update --availability active <storage-node>   # node back in service
```

### TC10-C — backup + restore + DR drill

```bash
pmcluster backup list                               # sees archives OTHER nodes uploaded (store index, BUG-025)
pmcluster backup restore <id> --volume sfapp/db_data --dest-root /tmp/tc10restore
pmcluster backup restore <id> --from-s3             # offsite IONOS nextrum-bu
tar -tzf /var/stack/purge-backup-<ts>.tar.gz        # data.db + .encryption_key + config.yaml
pmcluster cluster down --purge                      # writes purge-backup-<ts>.tar.gz, removes platform + store
pmcluster cluster reset --restore /var/stack/purge-backup-<ts>.tar.gz
```

### TC10-D — deploy lifecycle + drift

```bash
pmcluster cluster update                            # run 1 redeploys what changed; run 2 → "No rendered content changed — nothing to redeploy"
docker service rm <stack>_<service> && pmcluster cluster update   # recreated, reason "<stack> absent from the swarm → re-deploying"
docker service update --env-add PMCLUSTER_DRIFT_PROBE=1 backup_volume-backup
pmcluster cluster update                            # "<stack> drifted from the rendered compose → re-deploying"
pmcluster secret edit db_pass && pmcluster cluster update   # mints a new <name>_<sha8> swarm object, redeploys the stack
pmcluster config edit <name>  && pmcluster cluster update   # same for configs
```

### TC10-E — console / CLI / webhooks / tokens

```bash
curl -s -o /dev/null -w '%{http_code}\n' https://<domain>/api/cluster   # FAIL: 502 "dial tcp 172.17.0.1:9090: connection refused"
curl -s -H "X-Pmcluster-Signature: sha256=deadbeef" -H "X-Pmcluster-Timestamp: $(date +%s)" \
     https://<domain>/api/webhooks/deploy/<src>      # 401; the correctly signed request returns 202
pmcluster credentials rotate <name>                 # BUG-05/06
pmcluster secret heal                               # "1 ok, 0 degraded, 0 unrecoverable"
pmcluster stack badge <stack> ; pmcluster tls hosts list ; pmcluster registry add <bad-host>   # badge SVG; registry add fails loudly
pmcluster node promote <host> ; pmcluster node demote <host>   # demote of the leader refused
PMCLUSTER_API_URL=… PMCLUSTER_API_TOKEN=… pmcluster stack list  # remote mode
```

## Verification Matrices

### TC10-A

| # | Check | Result | Evidence |
|---|-------|--------|----------|
| A1 | Leader Docker stop → new Raft leader | PASS | new Leader in ~25 s; second consecutive loss also recovered |
| A2 | L2 control-plane survivor restored on the new leader | PASS | configs + `cluster_settings` present after the failover |
| A3 | Daemon survives a leaderless start (no crash loop) | PASS | pre-fix: 28 restarts in ~2 min; post v0.2.167 the no-quorum error is a logged no-op, daemon stands by |
| A4 | `systemctl start docker` brings the daemon back | PASS | unit carries `BindsTo=docker.service` **and** `WantedBy=docker.service` (v0.2.167/.168.2) |
| A5 | `cluster update` on a never-promoted manager | PASS | "The cluster exists; nothing to update here." exit 0, no wizard (v0.2.168/.168.1) |
| A6 | Replicated services reschedule after a clean exit | PASS | platform services render `{"Condition":"any","Delay":5000000000}` and return without a manual force-update (v0.2.168.2/.168.3) |

### TC10-B

| # | Check | Result | Evidence |
|---|-------|--------|----------|
| B1 | Stateful app pinned to a *different* node starts | PASS | pre-v0.2.169: `mount /var/stack/data/sfapp/db_data: no such file or directory` → `sfapp_db` Rejected on nxt-sw-4-m; after the repair loop it starts |
| B2 | Manual `stack move` between storage nodes | PASS | `stack_pin_sfapp` persisted, placement re-rendered, seeded row intact |
| B3 | Detect → pause sync → healthy alternate selected | PASS | the three reconcile log lines above, in order |
| B4 | Restore uses the **source node's** newest archive via a store-transit mover (no cross-node host ports) | PASS | only `backup-<sourceNodeID>-…` objects + `backup/data/<stack>/` content guard (v0.2.175); rclone pulls via `host.docker.internal:8333` (v0.2.174 → .174.3) |
| B5 | Re-pin + failover marker + amber badge; `stack ack` → healthy; move-back → marker cleared | PASS | `stack_failover` row written, badge amber → healthy after ack, marker gone after move-back, seeded row present |

### TC10-C

| # | Check | Result | Evidence |
|---|-------|--------|----------|
| C1 | `backup list` discovers other nodes' store archives; restore from the in-cluster store | PASS | store index (v0.2.170) + SigV4 fix (v0.2.173); `--volume sfapp/db_data` restore extracted the postgres tree |
| C2 | Restore `--from-s3` from the offsite bucket | PASS | after the IONOS key rotation (BUG-028) the S3 leg succeeded |
| C3 | Control-plane archive contents | PASS | `data.db` + `.encryption_key` + `config.yaml` in `purge-backup-<ts>.tar.gz` |
| C4 | DR drill: `cluster down --purge` → `reset --restore` | PASS | swarm rebuilt from the DB, app row intact, platform services healthy again |
| C5 | Restore routes to the node that owns the stack's volume | PASS | `RestoreArchiveToNode` + loud failure when it cannot route; `--from-s3` honoured (v0.2.175) |
| C6 | Stateful stack pinned off the deploying host | PASS | node-local volume-repair loop (v0.2.169 → .169.3) creates the missing dir and force-updates the service |

### TC10-D

| # | Check | Result | Evidence |
|---|-------|--------|----------|
| D1 | Second `cluster update` is a clean no-op | PASS | "No rendered content changed — nothing to redeploy" |
| D2 | A deleted platform service is recreated | PASS | reason printed: "<stack> absent from the swarm → re-deploying" |
| D3 | Live drift detected and re-deployed | PASS | `io.pmcluster.rendered_hash` label compared against the live services (v0.2.163–.165) |
| D4 | A partially-applied deploy counts as drift; a failed app deploy does not suppress retries | PASS | mixed label state → "partial deploy" re-apply (v0.2.163.2); rendered hash stamped only after a successful apply (v0.2.165) |
| D5 | Config/secret change mints a new content-addressed object | PASS | new `<name>_<sha8>` swarm object + redeploy of the dependent stack |

### TC10-E

| # | Check | Result | Evidence |
|---|-------|--------|----------|
| E1 | Console `/api/*` through the edge | **FAIL → PASS** | 502 `dial tcp 172.17.0.1:9090: connection refused` (BUG-03); after v0.2.175 the same call returns 200 |
| E2 | CLI sweep (list/show/deploy/logs/exec/rollback) | PASS | all sub-commands behaved as documented |
| E3 | Env-var auth / remote mode | PASS | `PMCLUSTER_API_URL` + `PMCLUSTER_API_TOKEN` drive the CLI against the daemon API |
| E4 | Signed webhook → 202, bad signature → 401 | PASS | `X-Pmcluster-Signature`/`X-Pmcluster-Timestamp` accepted; every HMAC failure mode collapses to 401 |
| E5 | Badge SVGs | PASS | `/api/public/badge/<stack>` and `…/<stack>/services` render |
| E6 | Per-stack tokens | PASS | 200 on their own stack, 403 elsewhere |
| E7 | `registry add` with a bad host; `tls hosts list` | PASS | registry add fails loudly with the underlying error; per-host certificate rows listed |
| E8 | `secret heal` | PASS | `1 ok, 0 degraded, 0 unrecoverable` |
| E9 | `credentials rotate` keeps the stack serving | **FAIL → PASS** | split-brain + lost mounts (BUG-05/06) → after v0.2.175 both secrets present, dashboard 200/401 |
| E10 | node promote / demote; `join --storage-node` without joiner→manager SSH | PASS | demote of the **leader** refused ("the leader must stay a storage node …"); joiner stamps `pmcluster.storage`, leader adopts it on the next update (v0.2.175) |

## Pre-Production Campaign Verdict

Verdicts as recorded at the campaign review; BUG-03 and BUG-09 were the two blockers.

| # | Check | Verdict | Note |
|---|-------|---------|------|
| 1 | TC10-A Raft leader failover + control-plane restore | PASS | v0.2.167 |
| 2 | TC10-A daemon survival across a docker-only restart | PASS | BUG-020/021/021b — v0.2.167/.168.2 |
| 3 | TC10-A standby-manager `cluster update` / `reset` | PASS | BUG-022 — v0.2.168/.168.1 |
| 4 | TC10-A node-loss service recovery | PASS | BUG-023 — v0.2.168.2/.168.3 |
| 5 | TC10-B manual `stack move` | PASS | store transit after BUG-030 |
| 6 | TC10-B automatic storage failover end-to-end | PASS | BUG-030 + 3 follow-ups, BUG-031 — v0.2.174…v0.2.175 |
| 7 | TC10-C backup list incl. other nodes' archives | PASS | BUG-025/029 — v0.2.170/.173 |
| 8 | TC10-C restore (store + `--from-s3` + DR drill) | PASS | BUG-028/029/032 — v0.2.172/.173/.175 |
| 9 | TC10-C stateful stack pinned off the deploying host | PASS | BUG-026 — v0.2.169…v0.2.169.3 |
| 10 | TC10-D `cluster update` idempotency | PASS | second run is a clean no-op |
| 11 | TC10-D drift detection + config/secret re-minting | PASS | v0.2.163–.165 |
| 12 | TC10-E CLI sweep, auth, webhooks, badges, tokens, promote/demote, `secret heal` | PASS | incl. BUG-05/06, BUG-019a — v0.2.175 |
| 13 | TC10-E console `/api/*` through the edge | **FAIL** | **BUG-03** — 502 through the edge |
| 14 | Platform placement (OpenObserve / edge / control-plane-backup) | **PARTIAL FAIL** | **BUG-09** — no placement guard; ~30 min with no swarm leader |

**After the v0.2.175 blocker batch every one of the 14 items re-verified PASS → READY FOR PROD ROLLOUT.**

## Bug Register

### BUG-020 — a daemon starting while the swarm has no Raft leader crash-loops
**Severity:** High — during a quorum outage every manager can lose its control loop.
**Symptom:** after `systemctl stop docker` on the leader (new Raft leader in ~25 s, L2 survivor restored as designed), a daemon started in the leaderless window died every ~5 s — 28 restarts in ~2 min on nxt-sw-4-m, each carrying Docker's `The swarm does not have a leader …`.
**Root cause:** the startup restore treated the no-quorum error as fatal, so systemd restarted the daemon forever.
**Fix:** v0.2.167 — `controlplane.IsSwarmUnavailable` (exported, was `isNotSwarmManager`) also recognises `does not have a leader` and `too few managers are online`; snapshot/restore become a logged no-op, the startup restore falls through to the tarball archive, and the daemon stands by until quorum returns.
**Verification:** leaderless-swarm fixtures with verbatim Docker errors + a six-case `IsSwarmUnavailable` table (falsified by disabling both patterns); live: no restart storm, daemon stays up through the outage.
### BUG-021 / BUG-021b — the generated systemd unit did not bring the daemon back with Docker
**Severity:** High — a manager returns with no control loop; if it is (or becomes) the Raft leader it stops reconciling silently.
**Symptom:** `systemctl stop docker && systemctl start docker` left `pmcluster` inactive on the standby managers (v0.2.167's `BindsTo=docker.service` propagates a *stop* but not a *start*).
**Root cause:** the unit was not in Docker's `WantedBy` set, so starting dockerd never pulled the daemon in.
**Fix:** v0.2.167 adds `BindsTo=docker.service`; v0.2.168.2 adds `WantedBy=docker.service` (`ensureDaemonRunning` rewrites and re-enables the unit on `cluster up`/`update`/`reset`/`join`, so `install.sh` rolls it out — v0.2.168.1 makes the standby path refresh it too).
**Verification:** `TestRenderSystemdUnit` asserts `WantedBy=docker.service`; live `systemctl is-active pmcluster` → `active` after a docker-only restart.
### BUG-022 — a standby manager fell into the interactive setup wizard on `cluster update`
**Severity:** Medium — the upgrade path fails on a manager that joined but was never promoted.
**Symptom:** `install.sh VERSION=…` on nxt-sw-3-w → `No cluster configuration found — starting the interactive setup wizard` → `error: domain is required (interactive prompt or --domain)`.
**Root cause:** a manager that never won a Raft election keeps an intentionally empty local store (0 `cluster_settings` rows against 29 on the promoted node), and `cluster update`/`cluster reset` used "no stored `domain`" as the test for "no cluster".
**Fix:** v0.2.168 — `classifyLocalCluster` distinguishes **provisioned** (settings present), **standby** (no settings, but a member of an existing Swarm) and **fresh**; a standby prints one line ("The cluster exists; nothing to update here.") and exits 0. v0.2.168.1 refreshes the daemon unit on that path as well.
**Verification:** `TestClassifyLocalCluster` (fresh / standby / probe-error / nil client / provisioned-wins); live re-run exits 0 with no wizard.
### BUG-023 — a long-running replicated service whose container exits cleanly is never rescheduled
**Severity:** High — services sit at 0/1 after a Docker restart until a manual `docker service update --force`.
**Symptom:** after the TC10-A daemons came back, `edge_pmcluster-edge` and `backup_control-plane-backup` were 0/1 and `backup_volume-backup` 1/2 — Swarm marks a zero-exit task `Complete`, and `condition: on-failure` ignores exit 0.
**Root cause:** the writer rendered `on-failure` for the default replicated case; global services already used `any`, which is exactly why they recovered on their own.
**Fix:** v0.2.168.2 renders `condition: any` for the default replicated case (an explicit `restart:` in a manifest still wins; run-once jobs unchanged); v0.2.168.3 stops the four platform services that forced `on-failure` (edge, both offen agents, seaweedfs) from overriding it.
**Verification:** translator goldens + `TestTranslate_Golden`; live every platform service renders `{"Condition":"any","Delay":5000000000}` and comes back unaided.
### BUG-025 — `backup list` only saw the archives this node produced
**Severity:** High — restore and failover could not use archives uploaded by another node.
**Symptom:** hourly offen archives written on the storage nodes were invisible to `pmcluster backup list` / `restore` on any other manager.
**Root cause:** those archives land in the in-cluster store but never pass through the local `Trigger` path, so no DB row existed off-node.
**Fix:** v0.2.170 — `discoverStore` indexes the store (S3 ListObjectsV2, prefix `backup-`, deduped, `pmcluster-ctlplane-*` deliberately excluded); the SeaweedFS leg only became usable once BUG-029 (v0.2.173) fixed the request signature.
**Verification:** `internal/backups/s3_list_test.go` (store discovery, pagination, store-only restore); live `backup list` shows the other nodes' `backup-<nodeID>-…` tarballs.
### BUG-026 (+3 follow-ups) — a stateful stack pinned to a node other than the deploying host never starts
**Severity:** High — the service stays `Rejected` and the app is down.
**Symptom:** `sfapp` on nxt-sw-4-m: `failed to populate volume: mount /var/stack/data/sfapp/db_data: no such file or directory` — the per-app volume directory existed only on the deploying host (reported live in TC10-B).
**Root cause:** storage directories were created where the deploy ran; the Swarm refuses a task whose bind source is missing, and no code path created it remotely (by design — no remote mkdir).
**Fix:** v0.2.169 — the daemon `mkdir -p`s the roots it owns at startup and runs a node-local volume-repair loop (startup + every 30 s, leader or not) that creates missing volume dirs for services pinned to *this* hostname and force-updates them; v0.2.169.1 resolves the local-driver volume `device` (not just raw binds), v0.2.169.2 treats a blank `volume_root` as unset (default `/var/stack/data`), v0.2.169.3 starts the repair *before* the leadership wait so standby managers run it too.
**Verification:** after v0.2.169.3 the repair line appears on nxt-sw-4-m, `sfapp_db` leaves `Rejected` and the tasks start without manual intervention.
### BUG-027 — ACME certificates behind a round-robin LB (cancelled by the user)
**Severity:** High for production TLS, but **not fixable from our side** — cancelled before any change.
**Symptom:** with Traefik global on every node each instance keeps its own ACME store, so behind the round-robin LB only one node ends up with a valid Let's Encrypt certificate while the others serve the default cert; re-issue attempts make the Let's Encrypt rate limits worse.
**Root cause:** certificate state is per-node and the load balancer distributes traffic round-robin.
**Fix:** none from pmcluster — needs TLS termination at the LB, a shared ACME store (Traefik enterprise) or certificates we issue and manage ourselves.
**Verification:** n/a — recorded as a known limitation, no code shipped.
### BUG-028 — the offsite IONOS leg failed with `403 SignatureDoesNotMatch`
**Severity:** High — no offsite copy, i.e. no DR/failover restore source.
**Symptom:** every offen agent uploaded to the in-cluster store and kept a local copy, but the offsite S3 leg failed with `The request signature we calculated does not match the signature you provided` on IONOS.
**Root cause:** two faults stacked — (a) the rendered env set `AWS_S3_FORCE_PATH_STYLE=true`, a knob the agent does not implement, so it fell back to virtual-host style (`<bucket>.<endpoint>`) and the provider rejected the signature; (b) the IONOS key had been rotated on the provider side, so every client failed with `403 SignatureDoesNotMatch`.
**Fix:** v0.2.172 renders `AWS_S3_BUCKET_LOOKUP: "path"` (and `AWS_ENDPOINT=<host>` + `AWS_ENDPOINT_PROTO=<proto>` on the legacy branch); a fresh IONOS key was issued, applied to the cluster and recorded — offsite **and** store backups resumed.
**Verification:** `TestLoadComposeFile_BackupS3Renders`; live `backup create` lands the archive in `nextrum-bu` again.
### BUG-029 — our SigV4 signer omitted `x-amz-date` from `SignedHeaders`
**Severity:** High — every store-backed read failed, so automatic failover could not restore.
**Symptom:** draining a storage node made the reconcile pick a healthy alternate and start the failover move, which then died with `403 SignatureDoesNotMatch` against the in-cluster SeaweedFS store (IONOS tolerated the omission — only the store path failed).
**Root cause:** `internal/backups/s3.go` signed only `host;x-amz-content-sha256` while still sending `x-amz-date`; strict verifiers recompute the signature over exactly the headers listed in `SignedHeaders`.
**Fix:** v0.2.173 — include `x-amz-date` in the canonical headers and in `SignedHeaders`, for both `fetchS3Object` and `listS3ObjectsPage`; this also unblocks store-backed `backup restore --from-s3` and the v0.2.170 store discovery.
**Verification:** byte-exact repro on the node (old form → 403, standard form → HTTP 200 + archive downloaded); `TestSignV4Headers_SignsAmzDate`.
### BUG-030 (+3 follow-ups) — cross-node move/failover transit depended on an ephemeral host port
**Severity:** High — moves and unattended failovers failed on hardened hosts.
**Symptom:** the failover reached the transit step and the mover task failed (`wget: download timed out`); the old mover had the leader serve the archive over a temporary HTTP server on a random ephemeral port, and hardened hosts only open 2377/7946/4789 per peer (the ephemeral range was allowed only for the original two-node pair).
**Root cause:** unattended recovery depended on an undocumented firewall rule.
**Fix:** v0.2.174 removes transit entirely — the mover service on the TARGET pulls the archive object straight from the object store with rclone and unpacks the `<stack>` subtree (no cross-node host ports, no firewall rules). Follow-ups: **v0.2.174.1** — a container cannot use the daemon's `127.0.0.1:8333` store endpoint (host 200 / container 127.0.0.1 fail / `host.docker.internal` 200), so loopback endpoints are rewritten to `host.docker.internal` and the mover timeout goes 120 s → 240 s; **v0.2.174.2** — `docker service create` rejects `--host-add` (`exit status 125: unknown flag`), so create with `--host host.docker.internal:host-gateway`; **v0.2.174.3** — a hung `docker service create` holding the CLI's pipes stalled the whole move, so the create runs asynchronously against a temp file and proceeds once `docker service inspect` sees the service.
**Verification:** `TestStorePullMoverScriptAndArgs`, `TestMoverEndpoint`, `TestStartMoverService_HangingCreateIsTolerated`, `TestStartMoverService_ReportsCreateFailure`; live moves and failovers complete with data intact.
### BUG-031 — storage failover restored another node's archive (empty database)
**Severity:** Critical — silent data loss on the app immediately after an automatic failover.
**Symptom:** the failover picked the **globally newest** `backup-*.tar.gz` in the object store, which could be a different node's whole-disk archive; postgres then came up with an EMPTY database instead of the failed node's data.
**Root cause:** the archive search was not restricted to the source node (the stack's current pin), so "newest wins" could select the wrong node's data.
**Fix:** v0.2.175 — select only objects whose `backup-<nodeID>-…` name matches the source node, plus a mover-side guard that verifies the tarball actually contains `backup/data/<stack>/` **before** `rm -rf` on the target, failing loudly otherwise.
**Verification:** `move_store_test` guards assert the content check runs *before* the target subtree is erased; live failover ends with the seeded row intact.
### BUG-032 (+follow-up) — `backup restore` wrote into the volume root of the node the CLI ran on
**Severity:** High — the restore "succeeded" while the stack's own node stayed empty and postgres re-initialised.
**Symptom:** on a multi-node cluster the CLI extracted into the local volume root instead of the node that owns the stack's volume.
**Root cause:** the restore path never resolved the stack's owning node.
**Fix:** v0.2.175 — resolve the owning node and route the restore there through the store-transit mover (`RestoreArchiveToNode`: rclone pulls from the store via the published ingress and extracts into the owning node's volume root), always name the destination node, and fail loudly when it cannot route (`this stack's volume lives on <node> — run the restore there`). Follow-up: the routed restore ignored `--from-s3` → `RestoreArchiveToNode(..., fromOffsite)` + `Service.OffsiteS3` so `--from-s3` genuinely pulls from the offsite bucket.
**Verification:** `TestRestoreDestination` (decision matrix) + `TestRestoreArchiveToNode_RoutesMoverToOwner`; live restore lands on the owning node from the store **and** from `--from-s3`.
### BUG-03 — the console's `/api/*` through the edge returned 502 (pre-production blocker)
**Severity:** Critical — the product's main UI/API entry point was down.
**Symptom:** `GET https://<domain>/api/…` → 502 with body `dial tcp 172.17.0.1:9090: connection refused`.
**Root cause:** the edge proxies to *its own* node's API and its placement was only `node.role == manager` — a no-op on an all-manager swarm — so the edge could sit on a node whose API was not the upstream it dialled.
**Fix:** v0.2.175 — `platform_node` defaults to the leader (written by `update`/`up`/`persistInstallState`), pinning the edge, OpenObserve and `control-plane-backup` to it; plus an edge `ErrorHandler` that returns clean JSON `{"error":"control plane unavailable"}` instead of leaking a Go transport error.
**Verification:** `TestNew_UpstreamUnavailableReturnsCleanJSON502`; live console `/api/*` through the edge returns 200 after the fix.
### BUG-05 / BUG-06 — `credentials rotate` split-brain and lost secret mounts (pre-production)
**Severity:** High — rotation can break the running stack, and dropped mounts are silent (Traefik routers 404).
**Symptom:** `credentials rotate` deleted the swarm secret *after* writing the store row (split-brain: row says new, swarm has none); separately, secret mounts that disappeared from a render were never re-added, so Traefik routers served 404 with no error.
**Root cause:** non-atomic ordering (row first, old secret removed) and no re-materialisation of missing rendered secret/config mounts.
**Fix:** v0.2.175 — mint the new versioned swarm secret **first**, never remove the old one, and add a self-heal step that rebuilds missing rendered secret/config mounts.
**Verification:** live `credentials rotate` leaves both secrets present and the dashboard serving 200/401; `pmcluster secret heal` → `1 ok, 0 degraded, 0 unrecoverable`.
### BUG-09 — no placement guard for the platform stack (pre-production blocker)
**Severity:** Critical — ~30 minutes with **no swarm leader** and the backup store down.
**Symptom:** OpenObserve had no placement guard and landed on a 1-core/1.8 GB manager → load 10–13 → SSH banner timeouts and Raft starvation; the hostname-pinned backup store went down with it.
**Root cause:** with `platform_node` empty, platform placement fell back to `node.role == manager` — a no-op on an all-manager swarm — and `pickStoreNode` could co-locate the store with the platform stack, so one overloaded small node took quorum *and* the durable store down together.
**Fix:** v0.2.175 — default `platform_node` to the leader in `cluster update`/`up`/`persistInstallState` (pinning OpenObserve, the edge and `control-plane-backup` to the big node) and make the store node avoid the platform node (last resort only).
**Verification:** `TestUpdate_DefaultsPlatformNodeToLeader`, `TestPickStoreNode_AvoidsPlatformNode`; live: OpenObserve/edge/control-plane-backup on nxt-sw-1-m, store on nxt-sw-3-w.
### BUG-019a / BUG-019b — `join --storage-node` needed joiner→manager SSH, stale storage labels
**Severity:** Medium — storage registration silently failed without SSH keys; a demoted node could keep a stale label.
**Symptom:** `pmcluster join --storage-node` registered the node by reaching the manager over SSH (`root@<sshHost>`); with no keys the join succeeded but the node never became a storage node. A node dropped from `storage_nodes` could keep `pmcluster.storage=true`.
**Root cause:** the leader's store setting was the only intent channel, and a fresh joiner has no path to it.
**Fix:** v0.2.175 — the joiner stamps the **swarm-visible** `pmcluster.storage` label on itself and the leader adopts labeled-but-unlisted nodes into `storage_nodes` on the next update; demotion goes through `pmcluster node demote` (clears label + setting), which supersedes BUG-019b's stale-label clearing.
**Verification:** live join prints `✓ labeled <host> with pmcluster.storage (the leader adopts it on its next update)`; `cluster update` prints `⚠ adopted <host> into storage_nodes (it carries the pmcluster.storage label)`.

## Final Clean-Cluster Rebuild and Production Rollout

After the campaign the four sandbox nodes were purged to empty — stacks, services, secrets, configs and volumes removed, the Swarm left in place, `/var/stack` + `~/.pmcluster` and the binary deleted — and the IONOS bucket was emptied (**93 → 0 objects**).

A fresh cluster was then built from the released **v0.2.175**: 3 managers with the bigger **nxt-sw-1-m as Leader** (hosting OpenObserve, the edge and `control-plane-backup`) + the worker **nxt-sw-3-w** hosting the in-cluster backup store.

- Production rollout, all via `install.sh VERSION=v0.2.175`: **nextrum-sy-1** (manager), **nextrum-sy-2** (worker), **wafaa** (single node) — all on **v0.2.175 (commit `37e38b1`)**.
- `platform_node` pinned to each leader; every platform service at its target replicas; all **15 customer stacks 1/1** on nextrum plus wafaa's app stacks healthy; a second `cluster update` is a clean no-op.

## Open / not yet fixed (found during the production rollout)

### BUG-033 — a worker's daemon runs a full reconcile loop it cannot perform
**Severity:** Low/Medium — log noise and wasted work every pass; no data risk.
**Symptom:** a worker node's daemon spams `ERR` lines on every ~3 s pass instead of standing by silently: `platform pass failed: this node is a Swarm worker, not a manager`, plus `service list failed: This node is not a swarm manager` for each stack.
**Root cause (diagnosis):** the reconcile loop runs the manager-only platform and service passes on a worker, where those APIs always fail; the errors are logged at `ERR` rather than being skipped (the volume-repair loop already recognises this case and stays quiet).
**Status:** **NOT FIXED** — a worker should stand by (or log at debug) instead of failing a full pass every cycle.
### BUG-034 — two orphaned stacks in the worker's local DB fail manifest parsing every pass
**Severity:** Low — the two rows are dead; every other stack still reconciles.
**Symptom:** `abbas-test` and `simple-erp` in the worker's local DB fail every pass with `json: cannot unmarshal array into Go struct field App.volumes of type map[string]string`.
**Root cause:** a legacy manifest written before `App.volumes` became a map (top-level named volumes are `map[string]string` today) — the stored JSON still has an array where the field now expects an object, so the decode fails forever.
**Status:** **NOT FIXED** — needs a migration/compat decode (or a way to prune orphaned local rows) so one stale record cannot fail the pass indefinitely.

## Operational Notes (not bugs)

1. **The reconcile loop logs `storage node down; pausing app sync` on every ~3 s pass** while a storage node is down. Noisy but correct — the message clears when the node returns or the stack is moved.
2. **A platform stack update/restart briefly makes the in-cluster store unavailable** — a Docker service with no running container has no DNS record — so a backup started in that window fails its **WebDAV leg only**; the offsite S3 leg is independent and still lands the archive.
3. **1-core/1.8 GiB nodes thrash under deploy load** (SSH banner timeouts). This is exactly why placement guards matter (BUG-09) — put heavy platform work on the big node.
4. **A worker's `systemctl start` after `install.sh` is a no-op** when the daemon never stopped — use `systemctl restart pmcluster`.
5. **The daemon logs zerolog console format** — grep `ERR`, not `level=error`.
6. **Expanding `storage_nodes` re-pins round-robin stateful stacks without moving their data** (still true after TC8) — run `pmcluster stack move <stack> --to <node>` afterwards.

## Outcome

**PASS.** The four-node pre-production campaign did its job: leader failover, storage failover, backup/restore/DR, deploy drift and the console/CLI surface were all exercised on real hardware, and the run surfaced **16 bug write-ups** — every one of the 15 actionable ones fixed and re-verified on the same sandbox (BUG-019a/b, BUG-020 … BUG-032, BUG-03, BUG-05/06, BUG-09 — v0.2.167 → v0.2.175), BUG-027 cancelled as not fixable from our side, and 2 left open (BUG-033, BUG-034) above.

The two pre-production blockers — **BUG-09** (no platform placement guard: ~30 min with no swarm leader and the store down) and **BUG-03** (console `/api/*` 502 through the edge) — were closed in **v0.2.175**, after which all 14 campaign items re-verified PASS: **READY FOR PROD ROLLOUT**. The rollout itself is clean: sandbox purged, bucket emptied, a fresh cluster from v0.2.175 (3 managers + 1 worker) and all three production hosts on **v0.2.175 (37e38b1)** — platform services at target replicas, 15/15 customer stacks 1/1 on nextrum, wafaa healthy, second `cluster update` a no-op. **Next:** fix BUG-033/BUG-034, then re-run the TC10-D/TC10-E slices against production.

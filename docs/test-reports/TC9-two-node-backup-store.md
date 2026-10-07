# TC9 — Backup Store on a Two-Node Swarm (Test Case 9)

**Date:** 2026-10-07
**Version under test:** v0.2.162.1 (commit 92dd93b)
**Cluster:** TC7 two-node swarm — 1 leader + 1 worker
- **Leader / Manager:** nxt-sw-1-m (217.160.143.22, private 10.7.224.12, 2-core/3.7Gi) — the **storage node** (`storage_nodes=nxt-sw-1-m`)
- **Worker:** nxt-sw-2-m (217.160.143.24, private 10.7.224.13, 1-core/1.8Gi) — plain worker (not a storage node)
- S3 offsite: IONOS eu-central-3, bucket `nextrum-bu`
- Domain: `nextrum-sy.de` (BYO self-signed cert)
- Both nodes upgraded to v0.2.162.1 via `install.sh VERSION=v0.2.162.1`

## Objective

Verify the in-cluster backup-store changes (v0.2.162 → v0.2.162.1) on a real 1-leader/1-worker pair:

1. `backup_store_on` selects which node runs the SeaweedFS store (leader vs worker).
2. Both offen agents (`volume-backup`, `control-plane-backup`) reach the remote destinations — the in-cluster store via WebDAV **and** the offsite bucket (double-write).
3. The control-plane agent specifically writes off-node (the case that was silently local-only on wafaa).
4. Restore from the store works; pruning runs automatically on every backend.
5. Characterize `cluster update` behaviour when a platform service drifts.

## Procedure

1. **Upgrade both nodes**
   ```
   # leader
   curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.162.1 bash
   # worker
   ssh worker '… install.sh VERSION=v0.2.162.1 …'   # auto update hits the interactive wizard (benign)
   ```
   Leader: all 7 platform templates synced into the store, `backup content` reconciled, `Stacks re-deployed: [edge]`.

2. **Store placement — the new `backup_store_on` setting**
   ```
   pmcluster cluster settings set backup_store_on=leader   # + cluster update
   pmcluster cluster settings set backup_store_on=worker   # + cluster update
   ```
   Each flip reported `backup content changed → re-deploying` / `Stacks re-deployed : [backup]`.

3. **Store service shape (verified live)**
   ```
   placement: ["node.hostname == nxt-sw-2-m"]
   args: ["server","-s3","-s3.port=8333","-s3.port.iceberg=0","-s3.port.lance=0",
          "-webdav","-webdav.port=7333","-ip=127.0.0.1","-ip.bind=0.0.0.0","-s3.ip.bind=0.0.0.0"]
   curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8333/healthz   # 200
   ```

4. **Data backup (double-write)**
   ```
   pmcluster backup create       # ✅ Backup id=82 succeeded
   # archive: backup-yeiit2ni1ita9soioqz7ija93-2026-10-07T08-26-35.tar.gz (6,635,826 bytes)
   ```
   Checked with rclone: present in the in-cluster store (`127.0.0.1:8333`, bucket `pmcluster-backups`) **and** in IONOS `nextrum-bu`.

5. **Control-plane backup (the wafaa regression)**
   ```
   docker exec <backup_control-plane-backup> backup
   ```
   Log: `Uploaded a copy … to bucket 'nextrum-bu'. storage=S3`, plus pruning lines for `storage=Local`, `storage=WebDAV`, `storage=S3`. The archive (`pmcluster-ctlplane-…-2026-10-07T08-26-47.tar.gz`, 3,502,140 bytes) is present in **both** the store and the offsite bucket.

6. **Restore from the in-cluster store**
   ```
   pmcluster backup restore 82 --volume tc7db/db_data --dest-root /tmp/tc9restore --from-s3
   # ✅ Restored 1251 file(s) — PG_VERSION 14, full postgres tree
   ```

7. **Drift probe** (see BUG-017)
   ```
   docker service update --env-add PMCLUSTER_DRIFT_PROBE=1 backup_volume-backup
   pmcluster cluster update      # → "No rendered content changed — nothing to redeploy"
   # cleanup: re-apply the stored render via `docker stack deploy -c <render> backup`
   ```

## Verification Matrix

| # | Check | Result |
|---|-------|--------|
| 1 | `backup_store_on=leader` places the store on the manager | ✅ constraint `node.hostname == nxt-sw-1-m` |
| 2 | `backup_store_on=worker` places the store on the worker | ✅ constraint `node.hostname == nxt-sw-2-m` |
| 3 | Default (setting empty) prefers a non-storage worker | ✅ `nxt-sw-2-m` chosen while `storage_nodes=nxt-sw-1-m` |
| 4 | Store runs S3 **and** WebDAV in one process | ✅ args contain `-s3` + `-webdav`; `/healthz` 200 on `127.0.0.1:8333` |
| 5 | `volume-backup` runs on storage nodes | ✅ global 1/1, constraint `node.labels.pmcluster.storage == true` |
| 6 | `control-plane-backup` runs on the manager | ✅ 1/1, constraint `node.role == manager` |
| 7 | Both agents carry **WebDAV + offsite S3** env | ✅ WEBDAV_URL + AWS_ENDPOINT/AWS_ENDPOINT_PROTO on both |
| 8 | Data backup reaches **both** destinations | ✅ 6,635,826 B in store **and** `nextrum-bu` |
| 9 | Control-plane backup reaches **both** destinations | ✅ 3,502,140 B in store **and** `nextrum-bu` |
| 10 | Restore from the store | ✅ `--from-s3` restored 1251 files |
| 11 | Automatic pruning runs per backend | ✅ log lines for Local + WebDAV + S3 (15d data / 30d ctlplane) |
| 12 | Platform placements otherwise correct | ✅ traefik + otel-collector global 2/2; edge + OO 1/1 |
| 13 | Daemon health after the whole run | ✅ 0 errors in the last 5 min |

## Bugs Found

### BUG-017 — `cluster update` cannot see live-swarm drift (rendered hash compared against the DB, not the swarm)
**Severity:** High — a platform service can silently stay in the wrong state forever.

**Reproduced on TC9 (deliberate):** injected an env var into the live `backup_volume-backup` service, then ran `pmcluster cluster update`:
```
▶ No rendered content changed — nothing to redeploy.
  Stacks re-deployed     : []
```
The drift was invisible. Cleaned up by re-applying the stored render (`docker stack deploy -c <render> backup`).

**Original sighting (wafaa, prod):** `backup_control-plane-backup` was running **local-only** — 6 env vars, no `WEBDAV_URL`, no `AWS_*` — while `volume-backup` had all 16. The control-plane archive was therefore landing on the *same single node* as the database it protects, i.e. no disaster-recovery value at all. The stored render *did* contain the correct env for both services (verified by dumping `rendered_content`), so the swarm was stale, not the template.

**Root cause:** `cluster update` decides whether to redeploy by comparing the **stored rendered hash** with a **fresh render**. It never inspects the live swarm. Once a service drifts out of band (orphan/partial redeploy, an interrupted apply, a manual `docker service update`), the hashes still agree and every later update reports "nothing to redeploy" indefinitely.

**Workaround applied (wafaa):** dump `rendered_content` for `backup-stack` from `data.db` and re-apply it with `docker stack deploy -c <file> backup`. Verified: the control-plane agent then wrote to WebDAV + R2, and the identical archive appeared in both.

**Fix (shipped in v0.2.163):** the rendered hash is now stamped as a label
(`io.pmcluster.rendered_hash`) on every service of a rendered platform stack, and
`cluster update` compares those labels on the **live** services against the fresh
render, in addition to the stored-hash comparison. A stack is redeployed when the
hash changed, the stack is absent, a service the render expects is missing, or a
live service carries the label of a **different** render. Extra live services are
ignored (`docker stack deploy` cannot remove them → flagging them would redeploy
forever) and an unlabeled (pre-v0.2.163) service is treated as in sync, since the
render change that introduced the label already forces one redeploy. Regression test:
`TestUpdate_RedeploysWhenLiveServiceLabelDrifted` (fails against the old DB-only
comparison).

**Extended to app stacks (shipped in v0.2.164):** the same label is now stamped on every
service of a **customer/app** stack, and `Sync` applies the same live-swarm check before
declaring "no drift". Before this, an app stack that lost a service or got a half-applied
deploy was reported in-sync forever — the identical bug class, in the app lane. The
detector lives in the shared `internal/stackdrift` package (`InSync` + `ContentHash`),
used by both `cluster update` and the app `Sync`. The `depends_on` per-level path stamps
the **full-stack** hash on every subset (a per-subset hash would never match and would
redeploy every pass). Upgrade cost: the first update after upgrading redeploys every app
stack once, which is what stamps the labels.

## Operational Notes (not bugs)

1. **The store's data volume is node-local.** Flipping `backup_store_on` moves the SeaweedFS task, and the named volume `backup_seaweeddata` is per-node — so the store starts **empty** on the new node. The offsite copy is the safety net; don't flip the setting casually on a cluster that relies on the store.
2. **Worker `install.sh` auto `cluster update`** falls into the interactive setup wizard and fails (`error: domain is required …`) — benign worker quirk; the binary installs and `systemctl start pmcluster` brings the daemon up.
3. On this cluster `platform_node` is empty, so `control-plane-backup` pins `node.role == manager`; on a multi-manager cluster it follows the platform node instead.
4. Retention is age-based: hourly data + 15 days ≈ 360 archives/host once the policy is in steady state; the control-plane keeps 30 daily copies.

## Outcome

**PASS.** The v0.2.162.1 backup changes behave correctly on a 1-leader/1-worker swarm: the store is placed by `backup_store_on`, both offen agents write to the in-cluster store **and** the offsite bucket, the control-plane archive is no longer local-only, restore from the store works, and pruning runs automatically on every backend.

**One real product bug recorded (BUG-017, update drift blindness)** — reproduced deliberately on TC9 and previously observed in production on wafaa. **Fixed in v0.2.163** (rendered-hash label + live-swarm comparison) and **extended to app/customer stacks in v0.2.164**.

**Limitation of this run:** the v0.2.162.1 setup-wizard changes (offsite SSO/S3 prompts, masked key input, `--backup-store-on`) were exercised via the resulting `backup_store_on` setting, not by re-running `pmcluster setup`.

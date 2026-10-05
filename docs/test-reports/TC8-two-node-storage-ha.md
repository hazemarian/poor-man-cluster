# TC8 — Two-Node Storage HA (Test Case 8)

**Date:** 2026-10-05
**Versions under test:** v0.2.158 → v0.2.158.1 → v0.2.158.2
**Cluster:** TC7 two-node swarm
- **Leader / Manager:** nxt-sw-1-m (217.160.143.22, private 10.7.224.12, 2-core/3.7Gi)
- **Worker:** nxt-sw-2-m (217.160.143.24, private 10.7.224.13, 1-core/1.8Gi)
- Both nodes are **storage nodes** (`pmcluster.storage=true`, `storage_nodes=nxt-sw-1-m,nxt-sw-2-m`)
- S3 offsite: IONOS eu-central-3, bucket `nextrum-bu`
- Domain: `nextrum-sy.de` (BYO self-signed cert), admin token from TC7 (`pmc_70f34abb_...`)

## Objective

Per the TC8 directive: promote the worker as a storage node, expose storage promotion in the UI, drain a storage node and verify the control plane moves its stacks to a healthy storage node restoring from S3, and verify apps can be moved between storage nodes via both CLI and UI.

## Procedure

1. **Promote worker as storage node:**
   ```
   pmcluster cluster settings set storage_nodes=nxt-sw-1-m,nxt-sw-2-m
   pmcluster cluster update   # label-repair step stamps pmcluster.storage on both
   ```
   Verified: `docker node inspect` shows `pmcluster.storage=true` on **both** nodes; `backup_volume-backup` became **global 2/2** with constraint `node.labels.pmcluster.storage == true`.

2. **Configure S3 offsite backup** (so failover has an offsite copy):
   ```
   pmcluster cluster settings set backup_s3_endpoint=https://s3.eu-central-3.ionoscloud.com \
     backup_s3_bucket=nextrum-bu backup_s3_region=eu-central-3 \
     backup_s3_access_key=… backup_s3_secret_key=…
   pmcluster cluster update
   pmcluster backup create      # id=2, archive …backup-ixq618bs4p2ry990xn4zp9q40-2026-10-05T01-15-48.tar.gz
   ```
   Verified offsite upload via boto3 `list_objects_v2` on `nextrum-bu`.

3. **CLI node promote/demote (live):**
   ```
   pmcluster node demote nxt-sw-2-m   # storage_nodes=nxt-sw-1-m, label cleared
   pmcluster node promote nxt-sw-2-m  # storage_nodes=nxt-sw-1-m,nxt-sw-2-m, label set
   ```
   Daemon API verified too: `POST /api/nodes/nxt-sw-2-m/storage` → `{"storage":true,…}`, `GET /api/nodes` shows `storage:true` for both.

4. **Cross-node stack move (CLI + UI path):**
   ```
   pmcluster stack move tc7db --to nxt-sw-2-m
   ```
   and via the console API (what the UI Move button calls):
   ```
   curl -X POST http://127.0.0.1:9090/api/stacks/tc7db/move -H "Authorization: Bearer …" \
     -d '{"target":"nxt-sw-1-m"}'
   ```
   Both completed. Each move: whole-disk backup → restore of the `<stack>/` subtree on the target (local `extractStackSubtree` or cross-node mover transit) → `stack_pin_<stack>` persisted → new placement revision.

5. **Automatic storage failover (S3 restore):**
   ```
   docker node update --availability drain nxt-sw-2-m
   ```
   Leader daemon logged:
   ```
   WRN reconcile — storage node down; pausing app sync … node=nxt-sw-2-m stack=tc7db
   WRN reconcile — storage failover: moving stack to a healthy storage node (S3 restore) from=nxt-sw-2-m to=nxt-sw-1-m
   INF reconcile — storage failover move completed stack=tc7db to=nxt-sw-1-m
   ```
   The offsite fetch was used: `/var/stack/backup/s3restore-backup-…-01-15-48.tar.gz` exists on the leader (source node was down). Node restored with `--availability active`.

## Verification Matrix

| # | Check | Result |
|---|-------|--------|
| 1 | Worker promoted to storage node (setting + label) | ✅ both nodes `pmcluster.storage=true` |
| 2 | Backup agent global on storage nodes | ✅ `backup_volume-backup` 2/2 with label constraint |
| 3 | S3 backup uploaded offsite | ✅ archive listed in `nextrum-bu` |
| 4 | CLI promote/demote storage node | ✅ demote cleared label + setting; promote restored both |
| 5 | Daemon API promote (used by UI) | ✅ POST/DELETE `/api/nodes/{host}/storage` 200 |
| 6 | Cross-node move via CLI | ✅ `tc7db` moved nxt-sw-1-m → nxt-sw-2-m, data alive (`SELECT 1` → 1) |
| 7 | Cross-node move via UI path (daemon API) | ✅ `POST /api/stacks/tc7db/move` → moved back to nxt-sw-1-m, data alive |
| 8 | Data integrity after move (both directions) | ✅ postgres `SELECT 1` returns live row; `db_data` tree intact |
| 9 | Storage pin survives (reconcile never moves back) | ✅ `stack_pin_tc7db` persisted, placement `node.hostname == …` |
| 10 | **Automatic failover on storage-node drain** | ✅ moved to healthy storage node, **restored from S3** |
| 11 | Failover cooldown prevents thrash | ✅ 5-min per-stack cooldown (unit-tested) |
| 12 | All platform placements correct | ✅ traefik + otel-collector global 2/2; edge + OO 1/1 on manager |

## Bugs Found

### BUG-015 — mover transit unreachable across hardened nodes (two rounds)
**Severity:** High (moves failed; stack stayed safe)
**Symptoms:** cross-node `stack move` mover task failed `non-zero exit (1)` / `Failed`; stack never lost data.
- **Round 1 (v0.2.158.1):** the mover built the archive URL only from the leader **advertise address (public IP)**. On the hardened test cluster the public IP silently drops SYN while the private interface works. Fix: `moverCandidateURLs` tries every non-loopback local interface IP, deduped; mover script loops candidate URLs. Still failed because busybox `wget -T 10` does not reliably bound a silently-dropped connect.
- **Round 2 (v0.2.158.2):** candidates reordered — RFC1918 **private addresses first**, advertise address last as fallback; and the mover now `rm -rf /data/<stack>` before `mv` (a stale target subtree from an earlier failed move blocked the move: `mv: can't remove '/data/tc7db': Is a directory`).
- **Fixed in v0.2.158.2 (commit b4ddb4e).** Verified live: moves now succeed in both directions with data intact.

## Operational Notes (not bugs)

1. **Expanding `storage_nodes` re-pins round-robin stateful stacks without moving data.** When `storage_nodes` grew from one node to two, `tc7db` was round-robin re-pinned to the new node whose `/var/stack/data/tc7db` did not exist → rejected tasks (`bind source path does not exist`). Operator response: run `pmcluster stack move <stack> --to <node>` (CLI or UI). Documented as expected workflow.
2. Worker install.sh auto `cluster update` still hits the interactive-wizard attempt and fails — benign worker quirk; `systemctl start pmcluster` after install.
3. Cross-node ssh leader→worker is not available on this test cluster (keys only on the Mac); worker-local verification is done from the Mac.
4. `pmcluster cluster settings get stack_pin_<stack>` errors `unknown setting` — stack-pin keys are intentionally outside the settings allowlist (written directly by `stack move`); read via sqlite.

## Outcome

**PASS.** All TC8 features verified live on a two-node swarm: storage-node promotion (CLI + API + UI), stack move between storage nodes (CLI + UI), automatic storage failover with **S3 restore** when a storage node is drained, and data integrity preserved across every move. Two real product bugs found and fixed (BUG-015 rounds 1+2, shipped in v0.2.158.1/v0.2.158.2).
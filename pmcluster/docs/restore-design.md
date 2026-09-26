# Restore

> **Status:** implemented. `pmcluster backup restore <id>` shipped in
> The whole-disk runs count as
> verified coverage). Archive discovery (migration 0018) and retention
> pruning (`backup_retention_days`, default 15) removes rows and archive files.
> The control-plane restore used by the leader-aware daemon
> (`RestoreControlPlane`) shipped in v0.2.82. Last updated: v0.2.84.

The earlier "Phase 5 — design only" framing is obsolete: `pmcluster backup
create`, the `backup_before_deploy: true` DSL hook, and the nightly offen
cron all write archives that pmcluster now discovers, lists, browses, and
**restores**. This file records what shipped and the gaps still open.

---

## What shipped

### Restore a stack's data

```
pmcluster backup restore <id> [--dest-root /var/stack/data]
```

- Works on **SUCCEEDED, stack-scoped** runs (on-demand or
  `backup_before_deploy`). Whole-disk (scheduled) runs restore into the
  **volume root** directly.
- Extraction lands under `<dest_root>/<stack>` for stack-scoped runs, or
  `<dest_root>/<app>/<vol>/...` for whole-disk runs — the offen archives
  bake in a `/backup/data/` prefix, which the restore strips
  (`internal/backups/browse.go` `archiveRelPath`). A restore writes
  **under the volume root**, never outside it (path-escape guard).
- **Control-plane archives are refused** into the volume root:
  `control-plane archive: cannot restore <p> into the volume root`
  (`browse.go` `refuseControlPlaneArchive`) — they contain
  `~/.pmcluster` state, not app data.
- Dest root defaults to `volume_root` (the `volume_root` cluster setting,
  default `/var/stack/data`); pass `--dest-root` to override.
- REST: `POST /api/backups/{id}/restore` `{dest_root}`; console
  `/web/backups/<id>` Browse → Restore card.
- Archive dir is **`/var/stack/backup`** (`backups.DefaultArchiveDir`) —
  not the old `/var/backups/docker-volumes`.

### Restore the control plane (leader-aware daemon)

- `RestoreControlPlane(archive, dataDir)` + `NewestControlPlaneArchive(dir)`
  (`internal/backups/browse.go`) are used by `pmcluster serve` on startup:
  the newest `pmcluster-ctlplane-*.tar.gz` (30-day retention) is restored
  into `~/.pmcluster` **only when the local control-plane DB is missing or
  older than the archive** (`internal/cli/leader.go` `ensureControlPlaneFresh`).
- This is the failover path for a standby manager promoted to leader —
  and it is **LINSTOR/shared-storage-safe**: a current DB (fresh local
  replica) is never clobbered.

### Discovery & retention

- Migration `0018_backup_filename.sql` adds `filename` to `backups` with a
  unique partial index — scheduled offen archives on disk are discovered
  once into `pmcluster backup list` / the console (no double-recording).
- `backup_retention_days` (default **15**) prunes old rows **and** their
  archive files on `List`/`ListForStack`; control-plane archives keep a
  30-day window.

---

## Constraints (unchanged from the original design)

- **A volume cannot be restored while it's mounted.** Restore extracts
  under the volume root to a temp path and renames; services still
  mounting the old volume see the new files on next task start. For
  zero-downtime data refreshes, prefer a re-deploy + restore onto a fresh
  task (the stack detail page's Restore button follows the same path).
- **Multi-node restore needs to happen on the right node.** Volumes are
  per-host; pmcluster restores the local node's archives. Worker restores
  remain a gap (see below).

## Open questions

- **Per-volume restore.** Currently restore is per-archive (whole run).
  A `pmcluster backup restore <id> <stack> <volume>` form selecting one
  volume out of a multi-volume archive is still open.
- **Worker-node restore.** Punted in the shipped design: pmcluster only
  restores manager-local archives. A future path could add an SSH-out /
  `docker service` based mechanism to restore a worker's volume in place.
- **Cross-host archive transport.** Archives from another node must be
  copied over first (documented, not automated).
- **`--from-s3`.** The bundled offen stack has S3 envs commented out; if
  enabled, archives may not exist locally and restore would need to pull
  from object storage first.

---

## What pmcluster gives you today

- `pmcluster backup list` / `browse` / `restore` — audit-logged, verified,
  path-guarded restore of app data (and of the control plane on leader
  promotion).
- `pmcluster backup create` — on-demand snapshot; `backup_before_deploy:
  true` in the DSL — automatic pre-deploy snapshot.
- Scheduled nightly whole-disk backups discovered automatically, with
  retention pruning and whole-disk runs counting as verified coverage for
  every stack.
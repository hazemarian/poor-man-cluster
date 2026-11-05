# Restore

`pmcluster backup restore <id>` extracts a successful run under the volume
root (`--dest-root` overrides the default), with `--volume <name>` narrowing
to one volume and `--from-s3` forcing a fetch from the offsite `backup_s3_*`
destination. Archive discovery (migration 0018) and retention pruning
(`backup_retention_days`, default 15) remove old rows and archive files. The
control-plane restore used by the leader-aware daemon (`RestoreControlPlane`)
restores the newest `pmcluster-ctlplane-*.tar.gz` into `~/.pmcluster` at
daemon startup.

`pmcluster backup create`, the `backup_before_deploy: true` DSL hook, and the
hourly offen cron all write archives that pmcluster discovers, lists, browses,
and restores. This file records what shipped.

---

## What shipped

### Restore a stack's data

```
pmcluster backup restore <id> [--dest-root /var/stack/data] [--volume <name>] [--from-s3]
```

- Works on **SUCCEEDED, stack-scoped** runs (on-demand or
  `backup_before_deploy`). Whole-disk (scheduled) runs restore into the
  **volume root** directly.
- `--volume <name>` narrows the restore to a single volume (e.g. `db_data`).
  On a stack-scoped run the volume is anchored to that stack, so it can never
  pull another app's same-named volume; on a whole-disk run a bare volume name
  matches every app's volume, so scope it as `<app>/<volume>`.
- `--from-s3` forces the fetch from the offsite `backup_s3_*` bucket instead
  of the in-cluster store / local disk.
- Extraction lands under `<dest_root>/<stack>` for stack-scoped runs, or
  `<dest_root>/<app>/<vol>/...` for whole-disk runs — the offen archives
  bake in a `/backup/data/` prefix, which the restore strips
  (`internal/backups/browse.go` `archiveRelPath`). A restore writes
  **under the volume root**, never outside it (path-escape guard).
- **Control-plane archives are refused** into the volume root:
  `control-plane archive: cannot restore <p> into the volume root`
  (`browse.go` `refuseControlPlaneArchive`) — they contain
  `~/.pmcluster` state, not app data.
- **Routed to the owning node.** A stack's volume lives on its pinned storage
  node; when the CLI runs elsewhere the restore is routed there through the
  store-transit mover, and fails loudly when routing is impossible
  (`this stack's volume lives on <node>`). Single-node clusters restore
  locally.
- Dest root defaults to `volume_root` (the `volume_root` cluster setting,
  default `/var/stack/data`); pass `--dest-root` to override.
- REST: `POST /api/backups/{id}/restore` `{dest_root}`; console
  `/web/backups/<id>` Browse → Restore card.
- A restore **replaces**: entries under the restored scope that the snapshot did
  not hold are removed after extraction (`pruneToSnapshot`), so a volume comes
  back as the snapshot captured it and not as a merge of two points in time.
  The scope is derived from the snapshot (`<app>` for a whole-run restore,
  `<app>/<volume>` under `--volume`), so an app deployed after the backup is
  never touched. Replacing is today's behaviour; merge-vs-replace is still open.
- Archive dir is **`/var/stack/backup`** by default, resolved through
  `cluster.BackupRootDir()` so the CLI reads the same directory the agent writes
  to (both honour `PMCLUSTER_BACKUP_DIR`; a relocated archive dir is why the two
  must not be resolved independently) — not the old
  `/var/backups/docker-volumes`. A row records the agent's CONTAINER path
  (`/archive/<name>`), so restore and browse map it onto the archive dir before
  declaring an archive missing.

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
- `backup_retention_days` (default **3**: every hour for 48 h, then one a day
  to day 3) prunes old rows **and** their archive files on
  `List`/`ListForStack`; the store and the bucket are pruned on the same
  policy through `planPrune`, and control-plane archives keep a 30-day window.

### When a restore extracts nothing

Three different conditions produced the same sentence, and only one of them is
a bug in the restore. They are now distinct, and `Restore` refuses all three
with the cause named:

| Message | Meaning | What to do |
| --- | --- | --- |
| `… contains only directories, so the snapshot captured an EMPTY volume` | the artifact is intact; the volume was empty when it was taken | the stack's data lives on another node — restore that node's artifact |
| `… holds N file(s), none of which matched the restore scope (volume "x")` | the artifact is intact and full; the filter excluded every entry | fix `--volume` (stack-scoped runs anchor it to the app) |
| `offsite artifact <key> …: not an archive` | the object exists but is not a tar.gz — an HTML error page, a JSON sidecar, an empty body | the key or the endpoint is wrong; nothing was written to disk |

Guards behind those messages: `fetchS3Object` rejects a 200 with an empty body
and a body shorter than the announced `Content-Length` (nothing is kept);
`restoreSource` classifies the fetched bytes (gzip magic, `ustar` at offset
257, zero length) before the extractor ever sees them; `listArchive` errors on
a non-archive and on a zero-entry archive instead of returning an empty list.
A fetched archive is staged in `<archive dir>/.restore-tmp` — `/tmp` is a
tmpfs on the reference hosts, so staging there would cap a restore at the
node's free RAM.

### An empty volume is not an artifact

The backup agent counts regular files before it commits to an artifact. A stack
whose directory exists on a node but holds no files (the leftover of a stack
whose volumes moved) produces **no artifact, no upload and no row**; the run
stays green and names the stack: `○ <stack> nothing to archive — no files under
/var/stack/data/<stack> — the volume is empty on this node`, plus a run-level
warning. Without this, the node kept writing a "succeeded" archive of directory
entries that restores zero files — seven of the twenty most recent artifacts in
the reference cluster were exactly that. Artifacts recorded before this rule
carry the note `empty: 0 files — the volume was empty on this node when the
snapshot was taken`, read from their sidecar manifest.

---

## Constraints

- Restore extracts files **in place** under the volume root (no temp staging
  + rename); a service still running against those files sees the new
  contents on next read/task start. For a zero-downtime data refresh,
  re-deploy and restore onto a fresh task (the stack detail page's Restore
  button follows the same path).

---

## What pmcluster gives you today

- `pmcluster backup list` / `browse` / `restore` — audit-logged, verified,
  path-guarded restore of app data (and of the control plane on leader
  promotion).
- `pmcluster backup create` — on-demand snapshot; `backup_before_deploy:
  true` in the DSL — automatic pre-deploy snapshot.
- Scheduled hourly whole-disk backups discovered automatically, with
  retention pruning and whole-disk runs counting as verified coverage for
  every stack.

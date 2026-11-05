# One backup run, end to end

Three pictures, one per phase, rather than one wall-sized diagram: the whole
pipeline drawn at once is four and a half thousand pixels wide, which GitHub
shrinks until the labels cannot be read. Colour key: **green** = in code,
tested, running in production · **amber** = host or ops requirement with no code
· **purple** = data · **red** = not built.

Packages a node needs: `fsfreeze` and `flock` (util-linux), `lvcreate` / `lvs` /
`lvremove` (lvm2), `mount`, `umount`, `tar`, `gzip`, `losetup`, `pvcreate`,
`blkid`. `sqlite3`, `psql` and `pg_controldata` are **not** installed and not
needed: SQLite verification links into the binary, and Postgres and MySQL are
recognised from their on-disk files rather than interrogated.

## 1. Once per node: making the volume snapshot-capable

```mermaid
sequenceDiagram
 autonumber
 participant OP as operator
 participant ST as storage
 participant K as kernel/LVM
 participant V as volume (thin LV)

 Note over OP,ST: once per node
 OP->>ST: storage migrate
 ST->>K: file → PV → VG → pool → thin LV
 ST->>V: mount, copy, verify, switch
 ST->>ST: install units (pool, mount, timer)
 Note right of ST: refuses over live data unless drained,<br/>repairs a half-built volume
```

## 2. Every hour: a heartbeat asks, the control plane answers

```mermaid
sequenceDiagram
 autonumber
 participant T as timer
 participant A as agent
 participant M as manager
 participant K as kernel/LVM
 participant D as destinations

 T->>A: hourly fire
 A->>M: may I run?
 M-->>A: enabled, due, targets
 Note right of M: settings decide
 A->>K: fsfreeze -f
 Note over K: writes block only<br/>0.1-0.6 s warm
 A->>K: lvcreate -s, thaw
 A->>K: mount -o ro
 loop per stack
   A->>A: tar, verify
   A->>D: store and/or bucket
 end
 A->>K: umount, lvremove
```

## 3. Getting it back

```mermaid
sequenceDiagram
 autonumber
 participant OP as operator
 participant A as agent
 participant D as daemon
 participant V as volume (thin LV)

 OP->>A: backup-agent drill
 A->>A: extract, integrity_check
 Note right of A: this is what proves restorability
 OP->>D: backup restore ID
 D->>V: extract one artifact, prune the rest
 Note over OP,V: the snapshot is long gone,<br/>the artifact is the restore object
```

## What the pictures imply

- **The freeze is the only write stall, and it is node-wide.** Everything after
  `fsfreeze -u` reads a snapshot, so a slow tar cannot slow your database.
- **Shipping is not on the critical path of consistency.** Both upload legs run
  after the thaw; a store or bucket outage fails that artifact's shipping
  verdict, never its snapshot.
- **Nothing in the hourly path needs the leader to survive.** The decision comes
  from a manager, but the run, the artifact and the record are node-local: a
  leader that dies mid-run costs the cluster its console view, not the backup.
- **Restore reads one artifact, not the snapshot.** The snapshot is deleted at
  the end of every run.
- **A stack with no files is not an artifact.** The agent counts regular files
  before it writes anything: an empty stack directory (what a moved volume
  leaves behind) produces no artifact, no upload and no row, and the run says
  so per stack. An archive of directory entries restores zero files, which is
  indistinguishable from a broken backup — the honest record is that this node
  had nothing to back up.

## Not built, and known

| Item | State |
|---|---|
| Postgres PITR (WAL stream) | designed, deliberately not written — see [`storage-and-databases.md`](storage-and-databases.md#future-plan-postgres-point-in-time-recovery) |
| Plan publisher / run-report endpoint | the agent works without both |
| Restore stopping the stack first | restore writes into a live volume root today |

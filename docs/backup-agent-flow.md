# One backup run, end to end

The whole pipeline in one picture: what is in code and running, what is a host
requirement, and what is deliberately not built. Read it top to bottom — the
first block happens once per node, the middle every hour, the last only when you
need the data back.

Colour key: **green** = in code, tested, running in production · **amber** = host
or ops requirement with no code · **purple** = data · **red** = not built.

Packages a node needs: `fsfreeze` and `flock` (util-linux), `lvcreate` / `lvs` /
`lvremove` (lvm2), `mount`, `umount`, `tar`, `gzip`, `losetup`, `pvcreate`,
`blkid`. `sqlite3`, `psql` and `pg_controldata` are **not** installed and not
needed: SQLite verification links into the binary, and Postgres and MySQL are
recognised from their on-disk files rather than interrogated.

```mermaid
sequenceDiagram
 autonumber

 box rgb(6,78,59) IN CODE — shipped, tested, running in production
 participant OP as operator / setup wizard
 participant ST as pmcluster storage
 participant T as systemd timer
 participant A as backup-agent (root oneshot)
 participant D as pmcluster daemon (this node)
 participant S as SeaweedFS store (WebDAV 7333)
 participant UI as console + CLI
 end

 box rgb(120,53,15) HOST / OPS — no code
 participant K as kernel + LVM (fsfreeze, lvcreate)
 participant S3 as offsite S3 (the store's own mirror)
 end

 box rgb(76,29,149) DATA
 participant V as volume root (thin LV)
 participant X as archive dir
 participant E as database engines
 end

 box rgb(136,19,55) NOT IN CODE YET
 participant PITR as Postgres WAL stream
 participant API as plan / run-report endpoint
 end

 Note over OP,ST: ONE TIME, per node
 OP->>ST: sudo pmcluster storage migrate (provision on an empty node)
 ST->>K: loop file → PV → VG → thin pool → thin LV → ext4
 ST->>V: mount at /var/stack/data — copy, verify, switch, keep the old tree
 ST->>T: install units — pool, mount, and RequiresMountsFor on docker/daemon/agent
 Note right of ST: refuses over live data unless drained,<br/>and repairs a half-built volume instead of rebuilding

 Note over T,A: EVERY HOUR
 T->>A: OnCalendar hourly, Persistent=true
 A->>A: flock -n /run/pmcluster-backup.lock
 Note right of A: one run per node, a second trigger exits 0
 A->>X: read cached plan.json when present
 A->>V: otherwise derive the plan from the directory layout
 Note right of A: a backup must not depend on the leader being reachable

 A->>K: findmnt --mountpoint — is this really our LV?
 Note right of K: not mounted ⇒ REFUSE, never tar a live tree
 A->>K: fsfreeze -f /var/stack/data
 Note over K,V: writes block, measured 0.1–0.6 s warm
 A->>K: lvcreate -s vg-pmcluster/data
 A->>K: fsfreeze -u
 A->>K: mount -o ro the snapshot

 loop one artifact per stack, serial
   A->>V: walk stack/ inside the frozen image
   A->>X: tar → backup-node-ts-stack-stack.tar.gz
   A->>X: manifest.json with sha256 and snapshot facts
   A->>E: verify — sqlite integrity_check
   Note right of E: postgres / mysql / mariadb are recognised from<br/>their files, not validated from the host
   A->>S: PUT artifact over WebDAV (127.0.0.1:7333, basic auth)
 end

 A->>K: umount + lvremove
 Note right of K: pool ≥80% aborts loudly rather than shipping<br/>a snapshot the kernel invalidated
 S->>S3: the store mirrors its bucket — SeaweedFS, not offen

 A->>X: retention — 48 h dense, one per day to 15 d, per stack
 Note right of A: never deletes a stack's last copies
 A->>X: write run-ts.json

 D->>X: discover artifacts → backups rows
 UI->>D: backup list / browse / restore
 UI->>A: backup create (root) — run now instead of waiting for the hour

 OP->>A: pmcluster backup-agent drill archive
 A->>A: extract + integrity_check, which is what proves restorability
 Note over OP,V: restore opens ONE artifact, extracts it into the volume root<br/>and prunes what it did not contain. The snapshot is long gone.

 alt leader is down during the run
   Note over A,D: a systemd job, not a leader client — it finishes, and the<br/>archive on disk is the record
   D->>X: rows discovered on the next pass
 end

 rect rgb(136,19,55)
   Note over A,PITR: NOT BUILT — RPO is one hour. The WAL stream that would<br/>make it continuous (pg_receivewal over the container's trust socket) is designed, not written.
   Note over A,API: NOT BUILT — plan publisher and run-report endpoint.<br/>The agent works without both.
   Note over D,V: NOT BUILT — restore writes into a live volume root,<br/>so stop the stack around it.
 end
```

## What the picture implies

- **The freeze is the only write stall, and it is node-wide.** Everything after
  `fsfreeze -u` reads a snapshot, so a slow tar cannot slow your database.
- **Shipping is not on the critical path of consistency.** The store leg runs
  after the thaw; a store outage fails the run's shipping verdict, not its
  snapshot.
- **Nothing in the hourly path needs the leader.** Plan, run, artifact and record
  are all node-local, which is why a leader outage costs you nothing but the
  console view until it returns.
- **The red block is a plan, not an oversight.** See
  [`storage-and-databases.md`](storage-and-databases.md#future-plan-postgres-point-in-time-recovery).

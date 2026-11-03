# Storage for stack data

Every stack's data lives under `/var/stack/data/<stack>/<volume>/…` as a bind
mount. That single decision — one directory tree, one filesystem — is why the
platform can take an atomic backup of a node and why it needs a volume it can
snapshot.

This document describes that volume: how a node gets one, what it guarantees,
what it does not, and what to do when it breaks.

## Why a directory is not enough

`/var/stack/data` on a fresh install used to be a directory on the root
filesystem. A backup of a directory is a file-by-file copy of a tree the
applications are still writing to. On 2026-10-27 that produced exactly the
failure it should: one growing SQLite file tripped `tar.ErrWriteTooLong` at
16:00, the run aborted, and the other 18 stacks on that node lost their hourly
backup with it.

A snapshot removes the class of failure, not the instance: the filesystem is
frozen (ms), duplicated (thin snapshot), and unfrozen before anything is copied.
The copy then reads an image nothing can write to.

None of that is possible on a plain directory, so the backup agent refuses to
run against one:

```
$ pmcluster backup-agent run --snapshot=lvm --vg vg0 --lv data
error: backup-agent: verify: /var/stack/data is mounted from /dev/vda1, not from
       /dev/vg0/data — refusing to snapshot a volume this node does not own
```

## Provisioning

```
sudo pmcluster storage status       # what this node can do, and why not
sudo pmcluster storage provision    # create it (idempotent)
sudo pmcluster storage grow --size 64G
```

`setup` asks once and provisions before the cluster deploys; `join
--storage-node` provisions on the node being added; `cluster up` refuses to
deploy when a backing file exists but the volume is not mounted.

The default mode is a **loop device over a sparse file**
(`/var/lib/pmcluster/app-data.img`), not a second disk or a cloud volume. The
reason is that the platform can create, size, verify and remove a file on its
own; a second disk is an action it can only assume somebody performed, and a
half-provisioned node is worse than one that was never provisioned. `--mode lvm
--device /dev/vdb` exists for hosts that already have a device.

### Layout and sizing

```
app-data.img                  sparse file: 50% of free space, clamped 8 GiB..128 GiB
 └─ /dev/loopN                loop device (found by file, not by number)
    └─ vg-pmcluster           # volume group (LVM)
       ├─ pool                 thin pool: file size minus 1 GiB
       └─ data                 thin volume: 80% of the pool
          └─ /var/stack/data   ext4, noatime
```

The data volume is deliberately smaller than its pool. Every run allocates pool
blocks for the snapshot's divergent writes; a volume that already fills its pool
has nowhere to put them and the snapshot invalidates mid-copy. The missing fifth
is the snapshot's room, and the agent's guard aborts a run at 80% pool usage
rather than let it silently degrade.

Sizes are rounded down to a 64 KiB chunk — LVM rejects anything that is not a
multiple of 512 bytes, and `--size 4G` minus 1 GiB of overhead, times 80%, is
2,576,980,377 bytes.

### What gets installed

| unit | job |
|---|---|
| `pmcluster-data-loop.service` | attach the loop device, `vgchange -ay`, and undo both on shutdown |
| `var-stack-data.mount` | `/var/stack/data` = `/dev/vg-pmcluster/data` |
| `docker.service.d/pmcluster-data.conf` | `RequiresMountsFor=/var/stack/data` |
| `pmcluster.service.d/pmcluster-data.conf` | same, for the daemon |
| `pmcluster-backup.service.d/pmcluster-data.conf` | same, for the agent |

Not `/etc/fstab`. `nofail` would let the node boot with the volume missing, and
every bind mount would then point at an empty directory on the root filesystem;
without `nofail` a missing loop device drops the machine into emergency mode for
a data volume. A native mount unit says what we actually mean.

**Ordering is the fragile part**, and it has been wrong twice:

- Pool service `After=local-fs.target`, mount unit `Before=local-fs.target` →
  ordering cycle → systemd resolved it by *silently deleting the mount job*. No
  failed unit, no journal error, just an unmounted volume at boot.
- Ordering the pool service before `local-fs.target` assumed `/` is the
  filesystem holding `/var`. On Fedora CoreOS (and any host with `/var` on its
  own filesystem) the backing file does not exist yet at that point and the unit
  fails.

The current, tested arrangement: pool service after `local-fs.target`; the mount
unit after the pool service and *not* ordered against `local-fs.target`; every
consumer ordered behind the mount with `RequiresMountsFor`. `internal/storage`
tests assert those directives, so an "improvement" to the ordering has to argue
with a test that names the two failures above.

A boot where the volume does not come up leaves docker down and the node out of
service. That is the intended trade: a node that refuses to run is recoverable,
a node that serves empty databases is not.

## What a backup run does

```
fsfreeze -f /var/stack/data       ← writes block; measured 0.3–0.6 s warm,
lvcreate -s -n snap-<ts> …          13.5 s once, on a busy node with a cold cache
fsfreeze -u
mount -o ro the snapshot
  per stack: tar → verify → manifest
umount · lvremove                 ← the snapshot is never the restore object
```

The freeze is the only write stall in the design and it is node-wide. Its
duration is dominated by how much dirty data the filesystem has to flush, so it
is a property of the node's write rate and page cache, not a constant. Plan for
seconds on a busy database node.

## Restore

One artifact holds one stack: `backup/data/<stack>/<volume>/…`. Restore extracts
that tree into the volume root and removes anything under the stack's scope that
the artifact did not contain — the volume ends as the snapshot saw it, not a
merge. The snapshot itself is deleted within seconds of being taken; there is no
node image to go back to, which is why a partial restore is not a special case.

Restore does **not** stop the stack. That is a known gap: the correct procedure
today is stop-the-stack, restore, start. See "Not done yet".

## Runbook

**Is this node backed up?**

```bash
pmcluster storage status                     # "snapshots possible" or why not
systemctl list-timers pmcluster-backup.timer # is the schedule alive
journalctl -u pmcluster-backup.service -n 50 # what the last run did
ls -l /var/stack/backup/*.tar.gz             # artifacts, one per stack
```

**The volume is not mounted after a boot.**

```bash
sudo pmcluster storage provision   # activates the VG and starts the mount
findmnt /var/stack/data
```

It will not rebuild over existing storage — if the volume group is present it
activates it, and refuses rather than recreate when it cannot.

**The pool is filling up.** `lvs -o lv_name,data_percent vg-pmcluster`. Growth is
`pmcluster storage grow --size 64G` (bigger file → `pvresize` → extend pool).
Above 80% the agent aborts runs rather than take a snapshot that will invalidate.

**A run failed.** The failure is per stack: one unreadable file costs that
stack's artifact, not the node's. `manifest.json` beside each artifact carries
the verdict, and `pmcluster backup-agent drill <archive>` re-checks an archive on
any machine.

## Not done yet

| gap | consequence |
|---|---|
| Host → store shipping | the agent writes locally; artifacts reach the in-cluster store only through offen today. WebDAV :7333 is not published for a host process, and publishing it exposes the store — the S3 leg on the published :8333 is the better route |
| Postgres PITR | a snapshot restores to the snapshot instant only; no `pg_receivewal` stream yet, so RPO = one hour |
| Retention | per-stack artifacts accumulate; offen's `BACKUP_RETENTION_DAYS=15` has no equivalent on the new path |
| Plan endpoint | `GET /internal/backup/plan` does not exist; the agent derives the plan from the directory layout, which works but carries no attribution |
| Restore stop/start | restore writes into a live volume root |
| Nodes with data | provisioning needs an empty volume root, so a node with stacks must be drained first (`stack move`) |

## Verified

- Two Fedora CoreOS machines (one with `/var` on its own filesystem): provision,
  idempotent re-run, repair after a failed boot, restart with data intact.
- CI: provisioning on three independent machines plus a backup-while-writing
  test that restores postgres, mariadb and sqlite onto new containers and
  asserts every pre-snapshot row survived.
- Production worker `nxt-sw-3-w`: provisioned, two engines deployed through the
  swarm, snapshot taken mid-write (368 ms), both artifacts verified, restored
  into fresh containers (postgres replayed its WAL, mariadb passed CHECK TABLE),
  hourly systemd unit installed and firing, machine restarted with the volume,
  the timer, docker and the data intact.

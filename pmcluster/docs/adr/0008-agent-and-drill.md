# ADR 0008 — the backup agent is part of being a storage node, and the export is drilled

**Status:** accepted (2026-11-16) · amends ADR 0007 (the export gets a drill) and the opt-in rule in `docs/backup.md`

## Context

A backup audit of production on 2026-11-16 found one of three storage nodes (`nxt-sw-4-m`) with a
live stack, five sqlite databases on its volume, and **no backup agent at all** — no service, no
timer, no policy, no `backup.env`, only a memory drop-in written by the storage provisioning.
Every check said the cluster was fine, because `backup coverage` only asks whether a stack's volume
*sits on* a storage node.

Two independent causes:

1. **The agent is opt-in per node** (`pmcluster backup-agent install`), and nothing tracked which
   storage nodes had it. Designating a node as storage, via the reconcile's adoption or
   `pmcluster node promote`, never installed anything.
2. **Nothing could see the gap.** The run history is node-local and not mirrored, so no follower
   could answer "is every storage node reporting", and the leader's `doctor` had no check for it.

Separately, ADR 0007's platform export was verified only by `S3ObjectExists`: an object was written.
Nothing had ever read one back, so "the control plane has a restore point" rested on the bucket
accepting a PUT.

## Decision

**1. A storage node's daemon installs its own agent, at startup.** `convergeStorage` already runs on
every node once at start and every two minutes, before the leadership wait. The daemon now calls
`nodeunits.EnsureBackupAgent` there, so the restart that a binary upgrade performs is the moment the
agent converges — "install or update" is literally when it happens. It is re-checked only when the
node's storage membership CHANGES, so the toggle an operator flips takes effect within a tick
instead of every tick writing units.

Why not the leader's reconcile, which was the first attempt: **`cluster update` is refused on a
follower manager** ("this node holds no authoritative copy …"), so the reconcile only ever runs on
the leader, and a leader cannot install a systemd unit on another host. Measured: a reconcile-side
install on `nxt-sw-4-m` never ran, and the unit never came back until the daemon did it.

`EnsureBackupAgent` is self-policing — on a node that is not a storage node it removes the units —
so calling it unconditionally is a converge, not a guess. The leader also still ensures its own
agent during `cluster update` (injected as `UpdateDeps.EnsureBackupAgent`, since nodeunits imports
cluster); a nil installer is reported, never silently skipped.

**2. The agent's absence is a finding, on the leader.** `doctor`'s `backup agents` check fails when
a designated storage node has not reported a run in seven days, reading the node id out of the
artifact names in the run records. It skips on a follower by design: `backups` is node-local and not
mirrored, so a follower would call a healthy cluster silent — measured, on the first build that ran
it there. The operator's answer is one command on the leader.

**3. The export is drilled, not just stored.** `pmcluster backup platform-verify [archive]` unpacks
the newest export into a scratch dir and checks, in the order a restore would: every member against
its manifest sha256, `PRAGMA integrity_check` on `data.db`, and **the archived key decrypting a
sampled secret** — the only step that proves the key belongs to the ciphertext it shipped with,
which is why the artifact carries both.

## Consequences

- A storage node can no longer be silently agentless. The two paths that make it a storage node both
  end with an installed agent, and the leader fails a check if one goes quiet.
- `cluster update` on a storage node now writes systemd units. It is idempotent and fast, and it
  needs root exactly as it already did for the daemon and watchdog units; a non-root run reports the
  failure instead of failing the update.
- The drill runs wherever the archive is available — the leader holds the local archive dir, so the
  weekly timer can run it there without pulling from S3. Drilling the *offsite* copy is still a
  separate act (download, then drill), which is honest about what has been proven.
- The console still does not show any of this: `internal/api/doctor.go` claims to be "reachable from
  the console" and nothing calls it. That comment is now false and marked as such; wiring a Health
  page remains open work.

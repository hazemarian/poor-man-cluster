# 0005 — The config channel is gone: the mirror is the whole document, and adoption copies nothing

Status: accepted · Date: 2026-11-15

## Context

ADR 0003 replaced the Docker-config channel with a pull, in slices. Slices 1–3 shipped the
route, the follower's mirror and the readers. Slice 4 — "retire the config path" — was left
open because it was written as an irreversible step with a precondition (observe slice 3 for
a day), and because it was scoped to removing the settings blob while ADOPTION still copied a
control-plane file.

That scoping is the thing the operator reversed, and his reasoning is one sentence: **"adopt
will just work because we already copy the data."** Every manager already mirrors the state
into its own store, so a promoted manager needs no file copy at all — and once no file is
copied, the Docker-config path has no remaining job:

- The **survivor kit** (`pmcluster_state_<ts>`, `internal/controlplane`) was an export of the
  authoritative tables, packed into a Raft-replicated config under Docker's 500 KiB ceiling.
  It grew over that ceiling once (5.2 MB of `stack_revisions`, 538 KB gzipped, every publish
  refused for days), and it carried a WINDOWED copy of two history tables because the whole
  database would not fit. A mirror has neither limit, so a promoted leader can hold the whole
  thing instead of a subset chosen to fit.
- The **key config** (`pmcluster_state_key_<ts>`) exists because ADR 0003's security split
  says the cipher key never shares a blob with the ciphertext it unlocks. It survives THIS
  decision: the key is still read from a config, because moving it to the join route is a
  separate change with its own provisioning and rotation story.
- The **daily control-plane archive** (`pmcluster-ctlplane-*.tar.gz`, written by an
  `offen/docker-volume-backup` service in the backup stack) archived `~/.pmcluster` off the
  leader once a day. It duplicated both of the above with none of the guarantees: a tar of a
  live SQLite file, pinned to the leader, restored only by hand.
- The **drill** (`pmcluster cluster kit verify`, the `doctor` line beside it, and a weekly
  systemd step) restored the newest kit into a temp dir and proved it worked. It existed
  because the kit was only ever exercised when a leader actually died.

## Decision

1. **Settings, secrets and every other table travel only by the mTLS pull.** Nothing is
   written to a Docker config to carry cluster state. `GET /internal/api/cluster/snapshot`
   remains the one channel, on the `swarmtls` listener, managers only, certificate identity.

2. **The mirror is the whole document.** Every table is mirrored, minus an explicit exclusion
   list whose members are all node-local (`backups`, `node_stats`, `github_runners`,
   `sync_state`, and the two structural tables), with a reason per table in
   `internal/store/mirror_tables.go`. `stack_revisions` is in it — a promoted leader has to
   read the manifest behind its live stack pointer — and so is `cluster_version`, so the
   promoted leader's version describes the data it actually holds instead of restarting at 1.
   The size is accepted: a few megabytes of text per poll when something changed, and a 304
   (no body) in the steady state, which is what keeps a 60-second poll cheap.

3. **Adoption copies nothing.** On a leadership gain the node stops and JOINS the mirror
   loop, reads `sync_state`, and keeps serving the store it already has as the writer. No
   `data.db` swap, no `.encryption_key` move, no re-extraction. `store.Store.Swap` is deleted
   with the path that used it.

4. **A stale mirror is served, and the cost is logged.** A manager promoted on a
   few-minutes-old snapshot serves it; refusing would turn a failover into an outage over an
   age the operator decided is acceptable. The adoption logs the version and the age it is
   adopting, so a leader that took over on old data says so once, in the journal.

5. **One hard refusal: nothing to serve.** A node that has never applied a snapshot AND whose
   store holds no cluster state (no setting, user, stack, secret or config) is an empty
   control plane. It refuses leadership loudly and starts no control loop. This is a
   different fact from staleness — an absence, not an age — and it does not share a code path
   with it. A node that has never mirrored but DOES hold state is the cluster's source (the
   first leader after `cluster up`): it adopts itself.

6. ~~**The key keeps its config for now.**~~ **Superseded by ADR 0006**: the key config is
   deleted; the key is a file delivered by the join route (or copied by hand) and proven by
   decrypting stored secrets. What follows is the original reasoning, kept for the history.
   The key was still read from `pmcluster_state_key_*`
   and compared with the local key file; a mismatch is reported (adoption WARN, `cluster
   mirror verify` problem), never a second way to end up with no leader. The join-time key
   route replaces this in a separate step.

## What is retired with it

- **`internal/controlplane` keeps only the key.** `kitdb.go` (the export), the staging and
  promote path, `Restore`, `Verify`, the mtime walk and the tarball packer are deleted. What
  remains is `PublishKey` / `KeyLoop` (the key config, still published because the join-time
  route does not exist yet) and the adoption check that reads it.
- **`pmcluster_state_<ts>` is never written again.** Nothing reads it: a promoted manager
  serves the store it mirrored, so there is no file to restore. `serve` publishes the key
  config on promotion and on its 5-minute loop, and nothing else.
- **The control-plane backup path is deleted.** The `control-plane-backup` service
  (`offen/docker-volume-backup`, pinned to the leader, archiving `~/.pmcluster` daily) is
  removed from the backup stack, together with `backups.NewestControlPlaneArchive`,
  `backups.RestoreControlPlane`, `backups.PruneControlPlaneArchives` and their callers in the
  backup agent and the store prune. **Backups are service volumes only.** The refusal that
  keeps a stray `backup/pmcluster` archive out of a volume restore stays — it guards against
  leftovers on disk, and it costs one tar header read.
- **The drill moves, it does not disappear.** `pmcluster cluster kit verify` becomes
  `pmcluster cluster mirror verify`, the `doctor` line `control-plane kit` becomes
  `cluster mirror verify`, and the weekly systemd drill step runs the new command. It reports
  the version applied here, the age of the last successful answer, the rows every table
  carries, and whether a key is present — the same "would a promotion work" question, asked
  of the thing that now decides it. A stale mirror is a WARNING (serving it is accepted); no
  snapshot and no key are the failures.
- **`store.Export` / `CopyPlan` stay in the store package.** They are not the kit; the tarball
  and offsite backup path uses the same export helper, and deleting it would take that path
  with it.

## What deliberately did not change

- **The DSL secret/config mirror** (`internal/cluster/secrets.go`) and the content-addressed
  swarm objects: a different mechanism, load-bearing, untouched.
- **Service volume backups**: the hourly node agent, the artifacts, retention, the drill and
  the offsite re-read.
- **The encryption key's handling.** It is still a file provisioned by `init`/`join`, still
  compared against its config on a leadership gain, and still never shared with the
  ciphertext it unlocks. Moving it is the next decision.

## Consequences

- **A promotion is instant and cannot fail on I/O.** There is no rename, no extraction, no
  half-applied kit. The failure modes that remain are about TRUST (is this state mine?) and
  those are stated, not hidden.
- **A follower stores the whole control plane**, including the history that the kit
  deliberately windowed. On a cluster with 18 stacks that is a few megabytes of SQLite
  alongside a few megabytes of JSON per changed poll; the operator accepted that number
  explicitly, and it is the price of adoption needing no copy.
- **`cluster_version` now comes across the mirror**, so a promoted leader continues the
  counter instead of restarting it — which also means a follower's ETag pair keeps working
  across a leadership change when the content is genuinely the same.
- **A demoted leader stops mirroring.** It keeps the store it had; if it later regains
  leadership, its mirror age is measured from when it last polled, which may be old. That is
  served (rule 4) and logged. Restarting the daemon makes it a fresh follower again.
- **The drill moves, it does not disappear.** `cluster kit verify` becomes
  `cluster mirror verify`, and the `doctor` line beside it becomes a verification of the
  mirror: version applied, age, table counts, and whether a key is present. Losing the only
  proof that a promotion would work is not an acceptable cost of retiring the kit.
- **`internal/controlplane` keeps only the key config and the adoption check.** The export
  (`kitdb.go`), the staging/promote path, `Restore` and `Verify` are deleted; the
  classification test that guarded the kit's table list moves to the mirror's own list.

## Handoff

- [x] The mirror carries the whole document (`internal/store/mirror_tables.go`, ADR 0003's
      amendment).
- [x] Adoption copies nothing; a stale mirror is served and logged; an empty store refuses.
- [ ] **The join-time key route** (`POST /internal/api/join/key`, `200` once, `409` after,
      `0600` outside the store), then rotate everything that ever lived in a config: the S3
      access key and secret, the SSO client secret, the SeaweedFS store secret. This is what
      removes the last Docker config and the `.encryption_key` provisioning path.

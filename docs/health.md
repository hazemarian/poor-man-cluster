# Health checks

Everything here is a *measurement*. Nothing in this chapter reports what the
platform believes it did; each check either opens something and looks, or says
plainly that it could not.

```sh
pmcluster doctor              # one table, non-zero exit if anything failed
pmcluster doctor --json       # the same report, machine-readable
```

Same measurements over HTTP, for the console and anything that gates on them:
`GET /api/doctor`.

## What it checks, and what each check can and cannot see

| Check | Measured how | Fails when |
|---|---|---|
| `swarm` | `docker node ls`, then the quorum arithmetic (`internal/cluster/quorum.go`) | quorum is already lost, or the manager count cannot survive one more loss |
| `node labels` | every node's `pmcluster.*` node labels | a label carries an empty value: the key is present, so anything matching on presence (`node.labels.pmcluster.storage`) reads the node as labelled (warn) |
| `version skew` | each manager's published `pmcluster_nodever_<host>` config vs this binary | a manager reports a different version |
| `control-plane kit` | **restores** the newest Raft snapshot into a temp dir, opens the DB, decrypts its secrets | the kit does not extract, does not open, misses its key config, or its key does not decrypt its ciphertext |
| `secrets` | AES-GCM decrypt of every stored row with the current key | a row cannot be decrypted (sealed by an older key) |
| `last apply` | the recorded `ApplyRecord` | the last apply degraded something (warnings) |
| `restore drill` | `/var/lib/pmcluster-backup/drill/status.json` | the last drill failed, or none has ever run (warn) |
| `offsite` | `/var/lib/pmcluster-backup/offsite/status.json` | an artifact this node shipped is not in the bucket |
| `disk` | `statfs` on the volume root | under 5% free (warn under 15%) |

**Skipped is not passed.** A check that cannot run — a worker cannot read the
manager set, a host has no backup state dir — reports `skip` with the reason.
`doctor`'s exit status is non-zero only for `fail`, so `warn` and `skip` are
visible without breaking a pipeline.

## The control-plane kit drill

`pmcluster cluster kit verify` (and the `control-plane kit` check above).

The leader publishes the control plane every five minutes as two Raft-replicated
Docker configs: `pmcluster_state_<ts>` (a tar.gz of a slim `data.db`, `config.yaml`,
the rendered `config/` tree) and `pmcluster_state_key_<ts>` (the
`.encryption_key` bytes alone — key and ciphertext never share a blob).

The `data.db` there is an EXPORT of the authoritative tables (users and token
hashes, managed credentials, the stack list, webhook sources, registries,
settings, configs, secrets, site-cert metadata, failover markers), plus a short
rollback window on the two tables that grow without bound: each stack's newest 5
revisions **and** the one its `current_revision` points at (not always the
newest — a rollback leaves the pointer on an older row), and the newest 10 config
versions per config name. The rest of that history, plus backup audit rows,
webhook delivery history, stack status and node stats, stays behind: the whole
file had grown past Docker's 500 KiB config ceiling in revision payloads, so no
snapshot was publishable and anything created since the last accepted one lived
on one disk. A promoted leader therefore serves the same auth, settings, secrets
and stack list, reconciles each stack against its current rendered manifest
without waiting for a re-deploy, and can roll a deploy or a config edit back
within the window; a revision older than the window still reads as "no stored
revision" until the stack is re-deployed, and a config rollback further back than
ten versions is a leader-side operation. The full `data.db` still rides the
tarball/offsite backup path.

Until this drill existed, that pair was only ever exercised when a leader
actually died. The drill restores it into a temporary directory using the same
extractor as the real restore, opens the restored `data.db` through the daemon's
own store (so migrations and pragmas run exactly as they would on promotion),
and then decrypts every stored secret with the key that came with it. It never
touches the live data dir, so it is safe on a serving leader — and cheap: a few
hundred kilobytes of tar into `/tmp`.

```
snapshot      2026-11-06T11:17:25Z (4m45s old)
state config  pmcluster_state_1793963845957904713
key config    pmcluster_state_key_1793963845957904713 (key e46caf46…)
extracted     6 file(s)
database      16 table(s), schema 0025_users_role (25 migration(s))
content       50 setting(s), 20 config(s), 4 secret(s)
decryption    4/4 secret(s) decrypt with the snapshot's own key
```

A state config whose key config is missing is a hard failure, not a warning: a
ciphertext without its key is not a backup. The weekly restore drill timer runs
it after the artifact drill, so a kit that has silently rotted is found on a
Sunday instead of during an incident.

## The offsite check

The hourly run records `where:` — the destinations an artifact reached at ship
time. That is intent, and it ages badly: S3 lifecycle rules expire objects,
quotas fill, credentials rotate, buckets get deleted.

`pmcluster backup-agent verify-offsite --days 2` re-reads the bucket: it lists
this node's recent artifacts (control-plane tarballs excluded — they are not
stack artifacts) and checks each **archive and its `.json` manifest sidecar**
against every configured target.

- a missing archive **fails** the run — the node believes it shipped something
  that is not there;
- a missing sidecar is **reported but does not fail** — the archive is
  restorable, just not verifiable (rows written before sidecars shipped land
  here);
- the verdict is written to `/var/lib/pmcluster-backup/offsite/status.json`
  (last result plus 14 checks of history), which is what `doctor` and
  `backup-agent offsite-status` read.

A daily timer runs it at 04:20, after the day's first backups, so it inspects a
settled set. When any target fails, that target is recorded last — the summary
line shows the failure, not whichever bucket happened to answer first.

## Version skew

Every manager publishes its binary version into the swarm on start and every ten
minutes, as `pmcluster_nodever_<hostname>` (data = version; labels carry the
hostname, the binary version and the `pmcluster.version` key the config-drift
check already uses). It rewrites only when the version changed, so Raft sees no
churn.

```sh
pmcluster node versions       # the fleet table, stragglers named
```

**Workers do not appear.** A worker cannot create a swarm config or set a node
label, so its binary version is unknowable from the swarm. The command says so
rather than letting a list of five managers read as a list of every node.

## Services with no stack

`pmcluster cluster orphans` lists swarm services that carry no stack namespace.
They come from a half-finished `docker stack deploy`, a hand-run
`docker service create`, or a stack removed with the docker CLI instead of the
API — and they are invisible everywhere else: `docker stack ls` does not show
them, `pmcluster stack list` does not know them, and the reconcile never touches
them, so nothing scales, updates or cleans them up while their tasks keep using
the node.

```
pmcluster cluster orphans            # report only
pmcluster cluster orphans --remove   # delete them
```

A service that *does* carry a stack label is never a candidate, even when its
stack is gone: that service belongs to a deploy that is still converging, and
removing it would race the stack's own re-deploy. The command exits non-zero if
any removal fails, so a scripted cleanup cannot pass on a partial result.


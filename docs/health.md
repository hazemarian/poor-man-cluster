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
| `github app` | mints a real installation token straight from the daemon's stored key (no credential helper, no extra port), reads the configured probe repo and HEADs the probe package | key/id rejected, installation removed, or `contents:read`/`packages:read` missing. No app configured is a **skip**; a token with no probe target is a **warn** |
| `registry credential` | the `created_at` of the stored `ghcr.io` row — the age of the pull credential, reset when `registry add` rotates it | **never fails.** Warns at 60 days: read-only PATs expire at 90, and nothing renews the credential on a timer, so this is the early warning. A missing row is a **skip** naming `registry add ghcr.io` |
| `last apply` | the recorded `ApplyRecord` | the last apply degraded something (warnings) |
| `restore drill` | `/var/lib/pmcluster-backup/drill/status.json` | the last drill failed, or none has ever run (warn) |
| `offsite` | `/var/lib/pmcluster-backup/offsite/status.json` | an artifact this node shipped is not in the bucket |
| `disk` | `statfs` on the volume root | under 5% free (warn under 15%) |
| `runner memory` (node-local) | the cap in this node's applied runner assignment vs its own `/proc/meminfo` | the cap exceeds 70 % of this node's RAM: the runner would OOM the node instead of being bounded by it ([`runners.md`](runners.md#a-runner-that-cannot-fit-the-node)) |
| `cluster mirror` (non-leader) | `sync_state` plus the row counts of the mirrored `settings`/`secrets`/`users` tables · **age = since the last successful poll** (a 304 counts), on this node's clock; the rows' own age is reported beside it | nothing has been applied yet (warn: the mirror fills within one 60 s poll), or the leader has not ANSWERED for three polls (warn, `STALE`, naming the leader): the values are real but old. A quiet cluster whose polls all answer 304 is **ok**, however old the rows are |

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

### Watching docker: the one thing the daemon cannot do for itself

`pmcluster` can restart itself. It cannot restart `dockerd` — docker is outside its
control loop, and on a node whose docker has died the daemon is either dead too or
serving requests it cannot fulfil. On 2026-11-12 `nxt-sw-3-w` went Down in exactly
that state: ports 2377/7946 closed, sshd accepting TCP but never completing a
handshake, the kernel still answering ping. Nothing on the node was watching dockerd,
and the swarm reported a node that stayed Down until a human power-cycled it.

The existing `pmcluster-watchdog.timer` is the right owner: it is root, it runs every
minute, and it does not need the daemon to be healthy to do its work. It now runs a
**second check** when `node_docker_restart` is enabled:

```
every 60s   GET /_ping over the docker unix socket, 5s timeout
3 failures  → restart docker.service
            → if systemd says the unit is `failed` (StartLimitBurst reached),
              `systemctl reset-failed` FIRST — otherwise `restart` is refused and
              the node looks recoverable while nothing is retried
after       → probe again. If it still does not answer, say so: "this node needs an
              operator". Do not report a recovery that did not happen.
            → then check THIS node's daemon: the docker restart propagated to it
              through `PartOf`, and the outage may already have spent its start
              budget. `reset-failed` + start it, because nothing else starts a
              `failed` unit when docker comes back
30 min      floor between restarts — three times the daemon-restart floor, because
            the blast radius is three times larger
```

**Why it ships off.** The remedy stops every container on that node: a running CI job,
the platform services scheduled there, its storage. That is the correct trade on a node
whose docker is already dead, and a terrible surprise on one whose docker is merely
busy. So the default is `false` and the remedy only ever runs because an operator asked
for it — an upgrade will not turn it on.

**Why the probe is a socket call, not `docker info`.** The timer runs as root with no
operator environment: no `DOCKER_HOST`, no guarantee that a `docker` binary is on
`PATH` for the workload. `GET /_ping` over the socket path it is given has no such
dependencies.

**The restart only helps if the control plane survives it.** A unit rendered before
v0.2.290 carries `BindsTo=docker.service`, and systemd turns a dockerd failure into a
*stop* of `pmcluster` — `Restart=always` is not honoured for a stop a dependency
caused. Measured end-to-end on `nxt-sw-6-w` (2026-11-13): the watchdog did its whole
job — three failed probes, `reset-failed`, `restart docker`, docker answering again —
and the node came back with dockerd running and **no control plane at all**, because
`pmcluster` had been stopped at the first kill and nothing was left to start it. The
fix is in the unit, not in the watchdog: the daemon's startup convergence now removes
the inherited `BindsTo=docker.service` line from the unit file and `daemon-reload`s,
and the drop-in adds `PartOf=docker.service` so an explicit docker restart still
restarts the agent. A drop-in cannot do the removal — an empty `BindsTo=` does **not**
reset a dependency inherited from the main unit (verified on systemd 259 with a
scratch unit, not just assumed), which is why the earlier "BindsTo cancelled" drop-in
was a no-op.

**Two more links in that chain, measured the same day.** With the unit repaired, the
same test found the daemon restart-looping to `failed` — `pmcluster serve` treats an
unreachable dockerd on the post-leadership control-plane restore as a *control-plane
failure* and returns it, so systemd started it five times, hit `StartLimitBurst`, and
left it failed (nothing starts a failed unit when docker comes back). An unreachable or
wedged daemon is now classified exactly like a leaderless swarm — defer the restore,
keep serving — because it is the same fact: this node cannot read the replicated kit.

And on the way down it disarmed the watchdog: `pmcluster` rewrites
`<data_dir>/node-policy.json` at startup from `node_docker_restart`, which is a
leader-owned setting a worker's store does not hold — so it wrote its own default,
`false`, over the answer the leader had already pushed to that file. The watchdog then
probed three times, saw nothing to do, and logged nothing. The policy is now written
only by a store that actually holds the decision; on a worker the runner assignment
carries it, and the daemon leaves the file alone.

**What it still cannot do.** It cannot recover a node that is out of memory: if the
kernel is killing processes, the timer is not running either. It cannot reboot a
machine, and it will not fight systemd beyond one `reset-failed` per 30 minutes. Those
cases still need a provider-level action, which is stated rather than papered over.

## What `doctor` can and cannot see

`doctor` inspects a runner container only for the node the daemon **runs on**. From
the leader, every other node's container state is *unknown*, not *absent* — and the
two were once the same value.

`NodeReport.ContainerKnown` records whether the inspect actually happened, and the
container checks require it:

| ContainerKnown | ContainerRunning | Registered | `doctor` says |
|---|---|---|---|
| true | false | true | "registered but no container is running" — a real fault |
| true | true | false | "container is running but GitHub does not list it" — a ghost |
| **false** | — | — | **nothing**: the worker's own `doctor` is the one that can see it |

Before that field existed, the leader printed "registered but no container is
running" for every healthy worker, because the zero value of `ContainerRunning` was
read as "there is no container". A warning that fires on every node is a warning
nobody reads, which is worse than silence.

### A node with no row in Node resources

The console's node panel is fed by each node's **own** daemon over OTLP, through the
the leader over swarm mTLS, into the `node_stats` table. A node missing from
the table is therefore a telemetry question, not a swarm one — and it is the
question this section exists to answer, because the panel looked broken while four
daemons had simply given up.

**The endpoint is supervised, not decided at boot.** `pmcluster serve` resolves
self-telemetry (`otlp_endpoint: auto` → loopback on the leader; on any other node the
manager address that answers on the collector's port, i.e. the leader) every **minute**
and reconciles the exporter with the answer:

| The probe says | What the daemon does |
|---|---|
| an endpoint answers, none was running | starts the exporter, logs `OTel telemetry enabled` with the URL |
| the same endpoint still answers | nothing (no restart, no reconnect) |
| a *different* endpoint answers | shuts the old exporter down first, then starts the new one, so one node never exports under two addresses |
| nothing answers any more | stops the exporter, logs `self-telemetry disabled` **once** with the reason |
| still nothing, still disabled | one DEBUG line per probe (`self-telemetry still unavailable`), never a WARN per minute |

The node-stat sampler starts **once, on the first successful probe**, and never stops
afterwards: a sampler with no exporter would do work nobody reads. Its own sampling
is a sink, not a loop.

Before this, the decision was taken once at startup. A node that booted while the
collector was being rescheduled disabled its exporter permanently and stayed dark
until someone restarted the daemon — measured on a live worker whose journal still
said `self-telemetry disabled: nothing answers at http://10.7.224.10:4318
(connection refused)` six hours after that port had started answering `200`. The
five-node cluster showed one row.

```sh
journalctl -u pmcluster | grep -i "self-telemetry\|OTel telemetry"
**How a node's numbers reach the leader.** The console is served by the leader; the
numbers belong to the node that runs the workload. A host process cannot reach an
overlay-only service, so the leader can measure exactly one machine — itself. Each node
therefore POSTs its own sample to the leader on the swarm-mTLS listener:

```
POST /internal/api/cluster/report      (node certificate, never a token)
     {"hostname":…,"cores":…,"load1":…,"mem_total_mb":…,"docker_up":…,"top_rss":[…],"ts":…}
```

`/internal/api` and not `/api`: these routes are for nodes, and the public API's token auth
must never be the thing standing between a node and its own measurement. The hostname is
taken from the **certificate**, not the body — a node that claims another's name is
corrected and logged, because otherwise one machine overwrites another's row and every
panel built on it lies.

The leader upserts one row per node (never a history: the control-plane SQLite file is what
every request reads). So the overview and the runner panel keep working when the collector
is down or Parseable is being upgraded — the store holds the current values, the collector
holds the history.

**How the collector is exposed, and the incident behind it.** Its OTLP port is published
`mode: host`, so 4318 exists on the node running the collector — the leader — and nowhere
else. It was `mode: ingress` before, which binds the port on **every** node and forwards to
the single task, and it was chosen for a reason worth remembering: with host mode and no
daemon-side guard, every non-leader daemon dialled a port nothing bound locally, the OTel
SDK's default error handler logged each failed export, and on 2026-11-08 one node wrote
261,617 journal lines from a single boot until journald starved docker's heartbeat and the
swarm marked it Down.

That class is closed at the source now — `telemetry.SetErrorHandler` is bounded (one line
per distinct failure, then a rate-limited summary) and the supervisor disables an exporter
whose endpoint stops answering — so the narrow publish is safe again. `doctor` asserts the
shape:

```
collector exposure   observability_otel-collector OTLP 4318 is published with mode: host
                     (the node running the task only)
```

An `ingress` mode here is a WARN, not a fault: the platform works, but 4318 answers on
machines that have no collector, which is exactly the state that hid the flood.

**Where the collector runs, and why the check names two nodes.** The collector follows the
**leader by name** — the same mechanism the edge, the SSO stack and the backup stack use —
not by a label constraint. A constraint says "wherever `pmcluster.leader=true` happens to
be", which floats: when leadership moves and nothing re-renders, the task stays on the old
node, the new leader's daemon finds nothing on its own socket, and the only symptom is
quiet panels. A rendered hostname cannot float, and `checkPlacementHosts` refuses one the
swarm does not have.

`doctor` therefore compares the running service's constraint with the current leader and
names both when they disagree:

```
collector placement  observability_otel-collector is pinned to nxt-sw-1-m, but the leader
                     is nxt-sw-5-m — the stack was not re-rendered after leadership moved;
                     run `pmcluster cluster update`
```

docker service ps observability_otel-collector     # is the collector itself running
ss -ltnp | grep 4318                               # does this node answer the OTLP port
```

A node whose probe never succeeds is not a `doctor` failure, and `doctor` does not
check it: `doctor` answers "is this node's daemon, socket, assignment, runner and
disk healthy", and a missing exporter is a *panel* symptom with a *telemetry* cause.
Those three commands are the check.

### The node-stats file is bounded on purpose

The collector writes that JSONL file through a `file` exporter with
`rotation: {max_megabytes: 2, max_backups: 1}`. Both numbers are load-bearing:

- `max_backups` **must be ≥ 1**. At `0` the exporter renames the rolled file to
  `nodestat-<timestamp>-size.jsonl` and never deletes it, while the reader keeps
  tailing only the newest file. A live leader grew that directory to **722 MiB
  across 372 files** before anyone looked, and nothing was broken enough to notice:
  the panel worked the whole time.
- A sample is a few hundred bytes, so 2 MiB is thousands of them: a rotation is
  infrequent, and when it happens the reader detects the new inode (or a shrink)
  and rebuilds from the **last 256 KiB** — enough for every node to contribute at
  least one sample — rather than insisting on an offset that no longer means
  anything. It never truncates the file: the exporter holds it open, and truncating
  produces a sparse NUL-filled middle that neither side can parse.

```sh
du -sh /var/stack/nodestat && ls /var/stack/nodestat | wc -l
# expected: a few MiB, one or two files
```

A directory that keeps growing points at an exporter config older than this one still
being deployed: `pmcluster cluster update` re-renders the collector config, and the
platform prints a warning naming the path when it cannot create it.

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


## A wedged daemon restarts itself

`systemd`'s `Restart=always` covers a process that **exits**. It does nothing for
one that is still `active (running)` and no longer answering — a control loop
stuck on a lock, or a daemon that survived an OOM event by swapping instead of
dying. That state is the one that left two nodes unreachable on 2026-11-10: both
were OOM-killed by a CI build, `sshd` died with them, and with no login there was
no way to restart anything. A restart needing a human is not a restart.

So every node runs one more timer, installed next to the daemon unit by
`pmcluster cluster up` / `cluster update`:

```sh
systemctl list-timers pmcluster-watchdog.timer
journalctl -u pmcluster-watchdog.service --since -6h   # one line per restart
cat ~/.pmcluster/watchdog.json                          # the counter
```

`pmcluster-watchdog.timer` fires every 60 s (first run 90 s after boot, so a slow
daemon can bind). It runs `pmcluster health-watch`, a hidden subcommand that
`GET`s `http://127.0.0.1:9090/health` with a 5 s timeout. It is hidden because it
is not an operator command: there is nothing to configure and nothing to decide.

**The restart happens only when all four bounds hold.** Each one exists because
the alternative is a machine that restarts its own control plane in a loop:

| Bound | Value | Why it is not optional |
|---|---|---|
| unit is `active` | — | an `inactive` or `failed` unit is systemd's state machine or an operator's `systemctl stop`; restarting it would make a deliberate stop a fight, and after `StartLimitBurst` the unit is `failed` on purpose |
| consecutive failed probes | **3** (~3 min) | one miss is a GC pause or a busy CPU during a build; three is a minute of silence a healthy daemon cannot produce |
| unit age | **> 5 min** since `ActiveEnterTimestamp` | a daemon that entered `active` a minute ago is an upgrade, a manual restart, or this watchdog's own last action, not a wedge |
| time since last restart | **> 10 min** | a daemon that cannot answer ten minutes after a restart is not fixed by another restart, and one restart per minute makes the journal unusable exactly when it is being read |

A restart writes **one** line naming the count:

```
2026-11-11T03:41:07Z pmcluster-watchdog: restarted the unit after 3 consecutive failed /health probes (last restart never)
```

**The counter is a file, not memory.** `~/.pmcluster/watchdog.json`
(`{"failures":3,"last_restart":…}`) belongs to the data dir precisely so the
restart it triggers does not reset it to zero — a counter in `/run` or in the
process would give you one restart per minute forever. It is written 0600 via a
rename, and a corrupt or missing file reads as `failures: 0`: the worst case is
one extra minute of silence, never a crash loop.

What the watchdog will **not** do, on purpose:

- **Restart a unit that is not active.** See the table; `systemctl stop pmcluster`
  stays stopped, and `failed` after a crash loop stays `failed` for a human.
- **Restart on one or two failures.** Including the loopback path being briefly
  unavailable during an upgrade: the probe answers again, the counter resets, and
  the recovery line says how many it had.
- **Restart a unit younger than five minutes.** This is the upgrade guard.
- **Restart more than once per ten minutes.**
- **Restart anything but the unit.** It is not a container supervisor and does not
  touch runners, stacks or Docker.
- **Probe anything but loopback.** `serve` binds `0.0.0.0`; the probe is always
  `127.0.0.1:<port>` (a configured `[::]` becomes `::1`), so a firewall change on
  the node's public address can never make the daemon look wedged.

A non-2xx answer counts as a failure. `/health` is unauthenticated and exempt from
the adoption gate, so anything but 2xx means something *other than this daemon*
holds the port — a proxy, a stale listener — and that is worth the same restart.

The units are `contrib/systemd/pmcluster-watchdog.{service,timer}` (reference) and
the CLI writes them on every `cluster update`; an install failure is a warning, not
a failed update, because the daemon has already been started by that point.

## Memory boundaries: a limit larger than the host is not a limit

Two cgroup limits are **computed from this host's `MemTotal`**, and they live in
exactly one file — the drop-in
`/etc/systemd/system/pmcluster.service.d/memory.conf`. The daemon rewrites it on
every start (`install`/`join`/`cluster up`/`cluster update` write it too, but the
start-up convergence is what makes an upgrade self-healing):

```
/proc/meminfo → 60 % / 70 % → memory.conf → systemctl daemon-reload → applies on the next restart
```

The main unit carries **no** memory directives. It used to, and that is how a node
ends up with two answers to "what is the limit?" — the unit's and the drop-in's,
with the drop-in winning only because systemd loads it later. One owner, and it is
the one recomputed on every start, so a resized host is picked up without anyone
remembering to re-render the unit.

| Setting | Value | What it does |
|---|---|---|
| `MemoryHigh` | **60 %** of `MemTotal` | throttle: the cgroup is reclaimed and slowed, the process is **not** killed |
| `MemoryMax` | **70 %** of `MemTotal` | hard ceiling: the cgroup cannot exceed it |
| `OOMScoreAdjust` | **-500** | when the kernel must pick a victim, it prefers the workload over the control plane |

The arithmetic, on the node that died:

```
MemTotal        1839 MiB   (nxt-sw-3-w)
MemoryHigh      60 %  → 1103 MiB
MemoryMax       70 %  → 1287 MiB
```

The 10-point gap is deliberate: it is the room the daemon needs to notice
`MemoryHigh` and act (finish a write, answer 503) before it reaches the wall.

**Why the percentages, not constants.** A fixed `MemoryMax=3G` is a limit on a
4 GB box and permission to OOM on a 2 GB one. That is not a hypothetical: the
same node was allowed a runner with `github_runner_memory=2560m` — **139 % of its
RAM** — and the first CI build that used it took the node, and its `sshd`, `Down`.
A limit that exceeds the host does not limit anything; it just tells the kernel
which accounting to do afterwards. `docs/runners.md` now refuses that assignment
outright; these bounds are the other half, for the process that must survive.

**What happens when `MemTotal` cannot be read:** no limit is written at all and
`cluster update` says so —
`⚠ could not read this host's MemTotal: no MemoryHigh/MemoryMax written` (the
drop-in is still written, with `OOMScoreAdjust` only — that part does not depend on
the host's size). A
default would be a constant that could exceed a small host, which is the failure
this section exists to prevent. `OOMScoreAdjust=-500` is still written: it does
not depend on knowing the size.

Check the live values:

```sh
systemctl show pmcluster -p MemoryHigh -p MemoryMax -p OOMScoreAdjust
systemd-cgtop -1 /system.slice/pmcluster.service   # current resident vs the ceiling
```

The collector has the same shape of bound, one level down: its stack carries a
hard **320 MiB** cap (`observability-stack.yml`) with the collector's own
`memory_limiter` at 240 MiB — strictly below the cap, so the limiter sheds load
while the process still has room instead of the cgroup killing it. Swarm services
have no `oom_score_adj` field (`docker stack config` rejects it), so the ordering
between the daemon and its tasks is bought from the daemon's side: the control
plane drops to -500, the tasks stay at 0, and the kernel picks the task.

## What `doctor` reports about runner caches

`runner caches` is a per-node check on the volumes a job keeps between runs — the Go
module cache, the npm store and the inner daemon's `/var/lib/docker`. It reads sizes from
`docker system df -v`, because that is the only place they exist.

It is a WARN when a cache passes its ceiling and OK otherwise, and it is skipped when the
node runs no runner: the volumes are created with the first job, so their absence on a node
that runs nothing is not a finding. If docker cannot measure them, the check says that
rather than reporting zero — a number nobody measured is worse than an admission.

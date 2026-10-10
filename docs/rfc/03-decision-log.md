# 03 — Decision log

Each entry follows: **Context → Options → Decision → Rationale (with the user
directive where one exists) → Consequences → Implementation**. Directives are
quoted verbatim, including their original spelling.

---

## DEC-01 — Docker Swarm + the ingress routing mesh, not Kubernetes

**Context (2026-04 → 2026-09).** The project started on Swarm
(`2982549`, "Initial commit — poor man's Docker Swarm cluster"). By September
2026 the question was whether to keep that choice or port to Kubernetes.

**Options.**
1. Stay on Swarm, accept its rough edges (no dependency graph, no CSI, a
   scheduler that will happily put a JVM on a 1-core node).
2. Port to Kubernetes (k3s) — better scheduling primitives, but a different
   operational contract for the target hardware.
3. Isolate the orchestrator behind an interface and stay on Swarm for now.

**Decision.** Option 3, executed as option 1 today.

**Rationale.** The isolation is the point, not the port:

> isolate the docker client and leave that question to the future this was my
> point of that refactoring and isolation even the output of dsl translation
> should be isolated by interface and there multiple implimentation swarm yaml
> writer terraform writer (what if someone uses something different that docker
> swarm) or helm yaml writer for k8s
> — user directive, 2026-09-17

The seam that implements it: `Parse → Interpolate → Validate → BuildIR →
Writer.Write`, where the IR is neutral and the writer is the only
Swarm-specific component (`docs/dsl.md:5-9`, `docs/improvements.md:72-88`).
`internal/runtime` is a minimal orchestrator contract so a second backend is
feasible without touching the domains (code-review finding, 2026-10-08:
"genuinely clean ports/adapters … a k3s/k8s backend is actually feasible").

**Consequences.** The platform inherits Swarm's behaviours as design inputs:
`depends_on` must be enforced in the control plane (DEC-06a), Traefik's
provider needs a manager socket (DEC-11), the scheduler needs explicit
placement guards (DEC-13), and there is no CSI — hence node-local volume
roots.

**Implementation.** `pmcluster/internal/runtime/`, `internal/manifest/translate.go:129`,
`docs/improvements.md:72-88`.

---

## DEC-02 — One render pipeline: platform stacks are DSL manifests

**Context (2026-09-17).** The platform's own stacks (`infra`, `edge`,
`observability`, `backup`, `sso`) were hand-written docker-compose files with
their own config/secret injection, while user apps went through the DSL
translator. Two code paths, two hash functions, two deploy paths.

**Options.**
1. Keep two paths and keep them in sync by hand.
2. Convert the platform stacks to DSL manifests (`platform: true`) rendered
   through the same pipeline.

**Decision.** Option 2.

**Rationale.**

> now why we are still using the docker-compose file for embedded resources?
> why not use DSL and then we have a single translation layer it will make it
> way simpler can we do it
>
> my point is i only want to use the same mechanism for replacing config and
> secrets config() , secrets()
> — user directives, 2026-09-17

**Consequences.** One renderer, one hash, one deploy path, one reconcile path,
one purge path. The DSL had to grow the primitives platform stacks need:
`mode: global`, node-label constraints, `settings(name)` refs, conditional
rendering, raw `constraints`/`binds`/`ports`/`configs`/`extra_hosts`/
`resources`/`user`/`labels`/`logging`, app-level `networks`, and a top-level
`volumes:` map that passes through **verbatim** (platform volumes like
`seaweeddata` and `traefik_acme` must never be relocated under the volume
root) — `docs/improvements.md:28-31`, `docs/dsl.md:47`.

The `platform: true` flag is reserved: it stamps
`io.pmcluster.platform=true`, excludes the stack from the app drift loop,
makes it non-deletable via the API, and is **refused** in user manifests
(`docs/dsl.md:45`).

**Release/implementation.** v0.2.152 (`docs/improvements.md:25-32`);
`pmcluster/internal/cluster/embeds/*.yml` (each begins with a comment saying
exactly this), `cluster.LoadComposeFile` (`internal/cluster/templates.go:504`).

---

## DEC-03 — Content-addressed configs and secrets, not version counters

**Context (2026-09-17).** Configs and secrets were named `_v1`, `_v2`, `_v3`…
and rotated by creating the next numbered object. That produced churn ("why
every restart there is a new ssl config"), collisions when two writers raced a
counter, and a mismatch between "the DB says v7" and "the Swarm holds v6".

**Options.**
1. Keep `_v%d` counters, compare by content hash but name by counter.
2. Name by content hash (`<base>_<sha256-first-8>`), reuse identical content,
   treat objects as immutable.

**Decision.** Option 2, with the revision counter kept as a *label*, not a
name.

**Rationale.**

> i would suggest all config and secrets should be compared by hash
>
> is it ok to use numbered version in the name or use a hash and add lable of
> the version as a date
> — user directives, 2026-09-17

and later, on the numbered form:

> i do not want to have numbered config and secrets why?
> … 8 hex is good … so config always comes from swarm / secrets come form db
> but stored in the swarm / no numbered increaments / the db will be index …

**Consequences.** Rotation is safe by construction: the in-use Swarm secret is
immutable and stays mounted until the next deploy; identical content reuses
one object (no churn); the DB becomes an *index* that also holds the value, so
a deleted Swarm object can be rebuilt (`docs/improvements.md:60`). This is
what made BUG-007 (secret rotation silently defeated) and BUG-018 (failed
deploy suppresses retries) fixable in the same architecture.

**Implementation.** `internal/cluster/secrets.go:21` (`dataHash`),
`:105` (`EnsureVersionedSecret`), `:216` (`EnsureConfig`), `:167`/`:188` (GC),
plus `pmcluster secret heal` (v0.2.166). Note the migration nuance: for a
config the DB hash may match unchanged content while the content-addressed
object has never been created — hence the rebuild-on-missing repair pass
(`secrets.go:121-124`).

---

## DEC-04 — Local SQLite + a Raft-replicated survivor kit, not a shared database

**Context (2026-09-17).** The control plane needs *some* store, and it must
survive the loss of the leader's disk. A shared Postgres was considered and
rejected.

**Options.**
1. PostgreSQL/MySQL on a shared volume — rejected: "the problem with pg it
   should have volume defined in one of the nodes if that is dead we lose
   data".
2. No SQL at all — use Docker's own config/secret store as the datastore:
   "a shared cache would work or the docker config it self could be used so no
   sql at all docker config will be our storeage we store config as base64 and
   we keep reading it".
3. Local SQLite + periodic snapshots into Swarm's Raft-replicated Docker
   configs.

**Decision.** Option 3 (option 2's insight is preserved: the *kit* lives in
Docker configs; the working set stays in SQLite).

**Rationale.**

> what data we need to make sure daemond can be a manager we store that in the
> swarm config no need to store everything
> — user directive, 2026-09-17

> the sorce of true is the db cluster up and update should update the file and
> and db
> — user directive, 2026-09-17

The kit is deliberately minimal: `data.db`, `config.yaml`, the `config/` tree,
and the encryption key **in a separate config family** so key and ciphertext
never share one blob (`docs/improvements.md:20`).

**Consequences.** `cluster reset` can rebuild the entire Swarm side from the
DB; a lost manager disk is a restore, not a re-bootstrap; a leaderless swarm
degrades to no-op snapshots rather than crash-looping (BUG-020). The cost is
that the DB is single-writer, which is acceptable because exactly one daemon
is ever active (leader-only serve).

**Implementation.** `internal/controlplane/state.go:15-16`, `:48`;
`internal/cli/serve.go:254`, `:286-299`; `docs/control-loop-design.md:70-92`.

---

## DEC-05 — The in-cluster object store is SeaweedFS (after MinIO, Garage and kopia)

**Context (2026-10-02).** Backups were local tarballs plus an offsite S3 push.
The cluster needed a *second* copy inside the swarm so that a node loss did
not depend on the external S3 endpoint — and so that `stack move`/failover had
something to pull from.

**Options considered, in the order they were tried.**

| Option | Outcome |
|---|---|
| **MinIO** (v0.2.160) | Rejected on two grounds: images became unpullable (production never ran it), and the user did not want a UI: "if there other option than minio without UI it is not needed we need to configuer and forget". |
| **Garage** | Tried seriously — a live session verified the S3 API on :3900, path-style addressing, the overlay DNS name and the RPC bootstrap container (`ses_ef3ed3e94ffe…`). Abandoned; the RPC/key bootstrap was judged too much ceremony for a store that must be "configure and forget". The directive at the time was "go with garage" and, in the same breath, "no api call that is no no it should be configurable by env or files". |
| **kopia** | Considered because "the good idea about kopia is that it would backup to multiple s3 and we can get rid of webdv"; rejected when the credential path got awkward: "if it is creds problem do not use kopia leave us with what we have". Also: "they are really stupid to not fix this we need to find another container that do backups". |
| **SeaweedFS** (v0.2.160.1) | **Adopted.** One process, S3 :8333 + WebDAV :7333, no config file, no bootstrap, `seaweedfs_admin` managed credential via env. |

**Decision.** SeaweedFS, single container, both protocols, WebDAV as the write
leg.

**Rationale.** The user's own framing:

> consider minio with the backup container as one stack make sure there is a
> translation that force volumed app to be on storage node we need to bypass
> that
> if there other option than minio without UI it is not needed we need to
> configuer and forget

The WebDAV leg exists for a mechanical reason: offen's WebDAV backend creates
the bucket path on first PUT, while the S3 API does not auto-create buckets
(`docs/storage-and-databases.md:257-259`).

**Consequences.** The store is pinned to a non-storage, non-platform node (a
different failure domain from both the data and the platform stack); port 8333
is published on the routing mesh so every node reaches it at loopback with
zero cross-node host ports; `harden-host.sh` must add 8333 to
`RESTRICTED_DOCKER_PORTS` to keep it cluster-internal
(`docs/network-topology.md:262-265`).

**Implementation.** `internal/cluster/embeds/backup-stack.yml:86-97`
(`chrislusf/seaweedfs:4.48`, `-s3 -s3.port=8333 -webdav -webdav.port=7333`);
CHANGELOG v0.2.160 / v0.2.160.1.

---

## DEC-06 — The backup agent double-writes natively; no replicator service

**Context (2026-10-08).** v0.2.160.1 shipped an `rclone/rclone` sidecar that
synced the store's bucket to the offsite S3 on a loop.

**Options.**
1. Keep the rclone replicator (an extra service, an extra failure mode,
   deletion propagation to worry about).
2. Let offen write both destinations natively (WebDAV → store, `AWS_*` →
   offsite) and let offen's own pruning keep them in lockstep.

**Decision.** Option 2 (v0.2.172).

**Rationale.**

> why we have rclone/rclone ?
> if seaweed do not do it by default the backup container can already ship the
> backup twice once to s3 and once to internal
>
> but what is the webdav for if seaweed automaticly sync with s3
> — user directives, 2026-10-02

**Consequences.** One fewer service, one fewer failure mode, and pruning that
cannot desynchronise the two copies. The trade-off is that offen's S3 backend
needed `AWS_S3_BUCKET_LOOKUP: path` (BUG-028 — offen does not support
`AWS_S3_FORCE_PATH_STYLE`) and that the in-cluster store now owns bucket
creation entirely.

**Implementation.** `docs/storage-and-databases.md:274-284`,
`backup-stack.yml:44`, `:133`; CHANGELOG v0.2.172.

---

## DEC-07 — SQLite runs with `journal_mode(DELETE)`, not WAL

**Context (2026-09-26, BUG-004).** The store used WAL mode. A second process
(the CLI, or `python3`) opening and closing the same database deleted the
`-wal`/`-shm` sidecars while the long-running daemon still held them open. The
daemon kept writing to the orphaned inode → split-brain database, 401s for
previously-valid tokens (BUG-005 was this same defect's symptom), and data
invisible to fresh readers.

**Options.**
1. Keep WAL and forbid multi-process access (unenforceable — the CLI *is* a
   second process by design).
2. Switch to the rollback journal (`journal_mode=DELETE`), which commits
   directly to the main file and has no long-lived sidecars.
3. Move the store off SQLite entirely (see DEC-04).

**Decision.** Option 2.

**Rationale (from the code comment, which is the design record):**

> WAL mode corrupts the control-plane DB under multi-process access … The
> rollback journal commits directly to the main file and has no long-lived
> sidecars, so concurrent daemon+CLI access stays consistent.
> — `pmcluster/internal/store/store.go:40-48`

**Consequences.** `WALCheckpoint` becomes a no-op that keeps the
`BackupTrigger` contract (`store.go:78-87`) — the older
`docs/security-and-improvements.md` §6 still describes the WAL-checkpoint fix
and is **stale on this point** (see Chapter 07). Throughput is lower, which is
fine for a control plane with `SetMaxOpenConns(1)`.

**Implementation.** `internal/store/store.go:40-50`; BUG-004 write-up in
`docs/test-reports/TC3-webhook-cli-features.md:50-94`.

---

## DEC-08 — The storage-node model: the leader is always a storage node

**Context (2026-10-02 → 2026-10-08).** Stateful stacks need a home. Early
versions pinned them to "the platform node", which on a multi-node cluster was
an arbitrary choice that did not follow leadership — so a leader change could
leave the backup agent and edge console on a node that was no longer the
leader, and a leader could be a node with no storage role at all.

**Options.**
1. Keep `platform_node` as the single anchor.
2. Introduce a `storage_nodes` set (round-robin for stateful stacks), with the
   invariant that the leader is always a member.

**Decision.** Option 2, plus three supporting rules.

**Rationale — three user directives, in order:**

> make sure when joining a node to cluster we need to add a flag in cli to
> select it as storage node
> by default main node is storage node
> …
> make sure worker node could be storage node not only leader are storage nodes
> — 2026-10-02

> edge should be only with leader so api cli webhook goes through it and leader
> is aways storage node so we have the container maybe the bug that leader was
> not storage and that should not be allowed
> cli should fail this is not a storage node
>
> what i meant make sure you prevent leader from not being a storage
>
> and if a manager was not a storage node and got the leader ship it should be
> promoted to storage and the fail leader to worker
> — 2026-10-02 (BUG-024)

**Rules as implemented.**
1. `cluster up` seeds the leader's hostname into `storage_nodes`; every
   `cluster update` re-adds the current leader (`CHANGELOG.md:284`).
2. Storage follows leadership: a manager that *loses* leadership is removed
   from `storage_nodes` and its `pmcluster.storage` label is cleared
   (v0.2.171).
3. `join --storage-node` stamps the swarm-visible `pmcluster.storage` label on
   the joiner itself (no joiner→manager SSH required — BUG-019a), and the
   leader's next `cluster update` adopts any labeled-but-unlisted node
   (v0.2.175). Demotion goes through `pmcluster node demote`, which refuses to
   demote the current leader.

**Consequences.** Workers can be storage nodes (a real requirement — the
sandbox's store lives on the 1-core worker `nxt-sw-3-w`). The backup agent's
`mode: global` + label constraint gives exactly one agent per storage node.
Expanding `storage_nodes` re-pins round-robin stacks *without* moving their
data — an operational footgun documented in Chapter 07 (run `stack move`).

**Implementation.** `internal/stacks/placement.go:75-183`,
`internal/cluster/cluster_state.go`, CHANGELOG v0.2.170/v0.2.171/v0.2.175.

---

## DEC-09 — Storage failover is opt-in, S3-only, and never moves back by itself

**Context (2026-10-02).** With pinning in place, a dead storage node means a
dead stack. The question was whether the platform should move it automatically.

**Options.**
1. Alert only (the operator moves the stack).
2. Opt-in automatic move to a healthy alternate, restoring from the newest
   archive of the *failed* node.

**Decision.** Option 2, gated on `storage_failover=true` **and** offsite
backups configured.

**Rationale (the design spec, from the session):** the move must leave a
visible marker ("flag badge"), require an explicit `ack`, and **not** move the
stack back automatically when the old node returns. The reasoning: an automatic
move-back would race a live workload against a restore, and the operator must
always know which node's disk is authoritative.

**Consequences.**
- The failover path must restore the **source node's** archive. It initially
  restored *any* newest archive in the store — including another node's, i.e.
  an empty database (BUG-031, fixed v0.2.175).
- The badge reads `failover` (amber) until acked; an unacknowledged marker
  dominates the badge (`docs/control-loop-design.md:66-68`).
- A 5-minute per-stack cooldown prevents a persistently-down node from
  re-triggering a move every pass (`reconcile.go:170-172`).
- Without backups the loop only *alerts*
  (`pmcluster.storage.failover.disabled`).

**Implementation.** `internal/reconcile/reconcile.go:423`
(`tryStorageFailover`), `docs/storage-and-databases.md:92-99`.

---

## DEC-10 — Routed restore + the store-transit mover (no cross-node host ports)

**Context (2026-10-08, BUG-015 then BUG-030).** `stack move` between two nodes
had to ship a tarball across machines. The first design opened an ephemeral
HTTP listener on the source node and gave the mover its address.

**Options.**
1. Ephemeral per-move host port (rejected twice: it required firewall openings
   between node IPs, which hardened hosts do not have, and it failed whenever
   the port was blocked — BUG-015 "two rounds").
2. Transit through the in-cluster object store: the mover pulls the archive
   *object* from the store's published routing-mesh ingress at
   `host.docker.internal:8333`.

**Decision.** Option 2, with option 1 as a fallback only when no store is
configured.

**Rationale.** The firewall discovery: hardened hosts open only 22/80/443 to
the WAN and the cluster ports between specific peers — there is no general
host-to-host HTTP path, and creating one per move is exactly the "per-node
ephemeral firewall ports" antipattern. The routing mesh gives every node a
loopback endpoint for free.

The user's constraint on the mover design, during the same stretch:

> no api call that is no no it should be configurable by env or files

**Consequences.** The mover is a one-shot Swarm service pinned to the target;
it verifies the tarball actually contains the stack's data before unpacking
(it must never empty a node by unpacking the wrong archive); three follow-up
releases (v0.2.174.1/2/3) were needed because `docker service create` rejects
`--host-add`, containers cannot use the loopback endpoint, and a hung
`docker service create` stalled the move when the CLI waited on its pipes.

**Implementation.** `internal/stacks/move_store.go:90` (`storePullMoverScript`),
`:178` (`RestoreArchiveToNode`), `:221` (`moveViaStorePull`);
`docs/storage-and-databases.md:328-343`; BUG-030.

---

## DEC-11 — Traefik is pinned to managers; workers stay in the LB pool via the routing mesh

**Context (2026-09-26 → 2026-10-09).** v0.2.157 shipped Traefik as a
placement-free `mode: global` service so that "all of them should have traefik
… assigning it only to manager will not work". That produced a real production
outage: Traefik's Swarm provider lists services through the local Docker
socket, which only a manager can read; a worker replica had no routers, and
the ingress routing mesh intermittently forwarded to it — `pmcluster.<domain>`
404'd on ~half of requests (TC7 BUG-014; session evidence: "returns 404 but if
you go to the link directly you can login").

**Options.**
1. Global Traefik everywhere (the v0.2.157 state) — broken on workers.
2. Manager-only Traefik with host-mode ports on each node — rejected: it
   bypasses the routing mesh and re-creates the firewall choreography the
   project refuses.
3. Manager-only Traefik, `ingress`-published 80/443, so the routing mesh
   fronts it from every node.

**Decision.** Option 3 (v0.2.178).

**Rationale.** The user reasoned it out on the record:

> so if a service deployed in a worker it could be reached by manager
> taefik will route it correcly if it is on a manager
> if we make traefik works only on managers <--- the correct thing
> no worker traefik then worker nodes no need to be in the loadbalancing loop
> correct?
> — user directive, 2026-10-09

which is exactly the routing-mesh property: any node receiving a packet for a
published ingress port forwards it to a task on the node that has one.

**Consequences.** Workers can remain in an external LB's backend pool (they
will forward, not serve). The LB/TLS limitation is unchanged and unsolvable
from inside the cluster (Chapter 07).

**Implementation.** `internal/cluster/embeds/infra-stack.yml:23-32` (with the
outage narrative in the comment), `docs/network-topology.md:106-133`,
CHANGELOG v0.2.178.

---

## DEC-12 — The edge console auth model: the Traefik gate owns auth; the console is synthetic-admin behind it

**Context (2026-09 → 2026-10).** The console needs an identity story. Two
candidate models: the console does its own login (users, sessions, RBAC), or
the Traefik gate (SSO / basicAuth) does it and the console trusts the gate.

**Options.**
1. Console-native login only.
2. Gate-owned auth with `edge_login_disabled=true` (default for swarm
   deployments): every `/web/*` request is treated as a synthetic admin, the
   console runs stateless (in-memory store, no `pmui-data` volume, no user
   rows, `/web/setup` and Users CRUD hidden).
3. Both, selected by the setting.

**Decision.** Option 3, with the semantics of option 2 as the cluster default.

**Rationale.** The Traefik gate is already in the request path and already
enforces org/repo-restricted OAuth or htpasswd; duplicating it in the console
would mean two user databases. The cost — "there is no per-user console RBAC
in that mode" — is documented rather than hidden
(`docs/security-and-improvements.md:174-177`). A later addition closed the
loophole that mattered: the `/sso-bridge` page (which writes the OpenObserve
localStorage envelope) is now gated against anonymous callers when SSO is
enabled, and the console signs out of the SSO session too (v0.2.178).

**Implementation.** `internal/ui/` (CSRF guard at `ui.go:132-136`),
`internal/cluster/embeds/sso-stack.yml`, `docs/security-and-improvements.md:174-183`.

---

## DEC-13 — Path 1: pin + backup + restore is the HA story; block replication is off the roadmap

**Context (2026-10-02).** A directive had pointed at LINSTOR as the HA
prerequisite. The implementation review produced an honest cost table and a
reversal.

**Options.**
1. **Path 1** — pin stateful services to a node; hourly backups; restore or
   `stack move` for recovery.
2. **Path 2** — LINSTOR/DRBD9 block replication: a JVM controller with its own
   embedded-etcd HA, per-node satellites, DRBD9 kernel modules (DKMS rebuilds
   on kernel updates), LVM thin-pool provisioning.

**Decision.** Path 1 (documented as explicitly superseding the earlier
directive: "The decision (supersedes the earlier 'LINSTOR is the HA
prerequisite' directive)", `docs/storage-and-databases.md:53-59`).

**Rationale.** The cost table (`docs/storage-and-databases.md:104-119`):
~1 GB RAM before any app data; kernel-module complexity with zero benefit on
a single node; and a replicated device is still a second copy to keep
consistent *plus* it still needs the backup pipeline. The user's framing of
the crossroads, on the record:

> 1- we follow road 1 the idea the core objective and soul of pmcluster is i am
> poor i do not want to handle this …
> 2- path 2 build this complicated things but it is not what pm cluster for
> this not the soul and the goal

The stated user constraint that killed LINSTOR outright:

> the problem with LINSTOR the user need a network before the VPS not firewall
> and not all users are ok to use tailscale

**Consequences.** Multi-manager HA protects the *control plane* (Raft-replicated
state configs); *data* HA is backup + restore + `stack move`. A node loss means
a minutes-scale restore window, documented as the accepted trade-off
(`docs/network-topology.md:78-90`). Revisit only if a workload cannot
tolerate that window.

---

## DEC-14 — Brand: six-palette token system with a pre-first-paint picker

**Context (2026-10-09, v0.2.179).** The console had a fixed dark indigo theme
(`pmcluster/DESIGN.md`, "Console v2"). The project needed an identity that
multiple deployments could own without forking the CSS.

**Options.**
1. Keep one hard-coded palette.
2. A `--brand-*` token system with six shipped palettes and a picker that
   persists the choice *before* first paint (no flash of the wrong theme).

**Decision.** Option 2, plus an inlined combination-mark logo (no external
asset request) and a README lockup.

**Rationale.** The design record is explicit that the previous direction was
rejected on taste grounds ("v1 (rejected): … read as a generic AI-SaaS
dashboard — the exact look both reference products reject",
`pmcluster/DESIGN.md:210-212`). The brand system extends that discipline to
*colour* without re-opening the token architecture.

**Consequences.** The palette is hand-derived; the light theme's semantic
variants are measured, not eyeballed, and the document records that axe
contrast went 221 → 0 in dark (`DESIGN.md:265-268`). Re-running both after
any palette change is a stated requirement.

**Implementation.** CHANGELOG v0.2.179, commit `ad6e67f`;
`pmcluster/DESIGN.md` (the design record), README lockup.

---

## DEC-15 — The testing doctrine: every manual intervention is a bug

**Context (2026-10-02).** Setting up the four-node test cluster.

**Decision.** The rule, verbatim:

> you should not do any manual work around if there is something that needs
> manual intervension that means it is a bug or an issue
> — user directive, 2026-10-02 (issued four times in the same session,
> `ses_f015e7886ffebPQLfTXFCjrRJx`)

**Rationale.** A platform that needs a human to `docker service update` a
secret back into place is not a platform; it is a script with a UI. The rule
converts every campaign intervention into a tracked defect. TC10's bug list is
literally the interventions that had to be logged (BUG-05/06 — `credentials
rotate` split-brain requiring manual `docker service update --secret-rm`;
BUG-022 — `cluster update` on a standby manager falling into the interactive
wizard; BUG-033 — a worker running a reconcile loop it cannot perform).

**Consequences.** The campaign reports carry a mandatory
"Manual interventions (logged)" section, and an intervention that cannot be
fixed becomes an explicitly open bug (BUG-033, BUG-034) rather than an
untracked workaround.

**Implementation.** Every `docs/test-reports/TC*.md` has the section; see
Chapter 06.

---

## DEC-16 — Release workflow: verify on the sandbox, then cut ONE tag, then roll out

**Context (2026-10).** Releases were being tagged repeatedly during a test run,
producing a stream of near-identical versions and a confused prod rollout.

**Decision.**

> to make things faster do not cut tags wait for ci then deploy when you are
> sure all tests are done and we are fine we cut tag and merge to prod it will
> really speed up the process
> — user directive, 2026-10-02

**Rationale.** Tagging is a promise. A tag cut before the sandbox campaign
finishes forces either a second tag or a prod deployment of a known-bad build.

**Consequences.** The operational shape that TC10 followed: deploy a **dev
binary** to the sandbox, run the campaign, fix, redeploy the dev binary,
re-verify, and only then cut a single tag and roll to production. A related
hard rule from the rollout sessions: `systemctl start` is a no-op when the
daemon never stopped (the old binary keeps running under a deleted inode) —
use `systemctl restart`, and log it as an intervention when you forget
(`ses_ee1fc8f83ffepiLlpPfZQnnYII`).

**Implementation.** CHANGELOG v0.2.175 → v0.2.179 sequence; TC10 §"verdict";
the rollout sessions' "Manual interventions (logged)" sections.

---

## DEC-06a — `depends_on` is enforced by the control plane, not by a wrapper

**Context (2026-09-26).** `docker stack deploy` parses `depends_on` and
ignores it; Swarm has no dependency graph.

**Options.**
1. Inject a shell wait-wrapper into the dependent service's command/entrypoint
   (v0.2.110–112) — breaks baked-entrypoint images (Postgres, MySQL),
   distroless images without `sh`, and images with no `command:`.
2. Topologically sort the stack into `depends_on` levels and deploy level by
   level from the control plane, waiting for health between levels.

**Decision.** Option 2 (v0.2.118), with the wrapper removed.

**Rationale.** Ordering must work for *any* image. One drift-prune pass runs
at the end with the full stack compose — a partial deploy must never prune
sibling services it has not deployed yet. A `run_once` job with `depends_on`
gets `restart_policy: on-failure, max_attempts: 3` so a transient failure
retries (`docs/dsl.md:273-280`).

**Consequences.** `rendered_yaml` (stored whole-stack, hash-compared) is an
audit artifact only and is never executed directly.

---

## DEC-07a — Control-plane restore is startup-only

**Context (BUG-006, 2026-09-26).** On leadership gain the daemon restored the
Raft-replicated kit over its own live SQLite connection — `Restore` truncates
`data.db`, so the daemon served stale page-cache rows while fresh readers saw
the clobbered file.

**Decision.** Restore only at startup, before the store is opened, and only
when the local DB is missing or older than the snapshot. On promotion, only
publish.

**Implementation.** `internal/cli/serve.go:286-292` (the comment is the
design record), `docs/control-loop-design.md:83-92`, CHANGELOG v0.2.17x
restore semantics.

---

## DEC-08a — The rendered hash is stamped only after a successful apply

**Context (BUG-009, BUG-017, BUG-018).** Three iterations of the same insight:
(a) a *failed* platform deploy used to mark the stack up-to-date, so it was
never retried; (b) the drift check compared the fresh render against the DB,
not against the live Swarm, so a manual `docker service update` was invisible;
(c) app stacks had no live label at all.

**Decision.** Every service carries `io.pmcluster.rendered_hash=<sha256>` of
the whole-stack label-free render, stamped **after** a successful apply;
`stackdrift.InSync` compares the fresh render against the live labels as well
as the stored hash.

**Consequences.** A failed deploy always looks drifted (and is retried); a
manual service update is re-applied; a half-applied platform deploy counts as
drift (BUG-017 follow-up v0.2.163.2).

**Implementation.** `internal/runtime/…:330-337`,
`internal/stackdrift/stackdrift.go:24`, `:72`;
CHANGELOG v0.2.163 / v0.2.164 / v0.2.165.

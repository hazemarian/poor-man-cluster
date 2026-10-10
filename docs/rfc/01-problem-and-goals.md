# 01 — Problem statement, design goals, non-goals

## 1. The problem

Running application stacks on rented VPS hardware is cheap and terrible. A
single €5/month box can host a dozen small apps, but it has no redundancy; a
handful of such boxes give you redundancy only if you accept an operational
burden that most small operators cannot carry:

- **Orchestration** — scheduling containers across machines, restarting them,
  rolling out new images.
- **Ingress** — terminating TLS for a dozen hostnames and routing to the right
  container on the right machine.
- **State** — deciding where a database's files live, and what happens when
  that machine dies.
- **Secrets** — keeping passwords out of git and out of `docker inspect`
  output while still getting them into containers.
- **Recovery** — backups that actually restore, on a *different* machine, in a
  reasonable time.

The commercial answers (managed Kubernetes, managed databases) and the
self-hosted answers (Longhorn, Rook/Ceph, LINSTOR/DRBD9) all fail the same
test for this hardware envelope: they cost more in RAM, disk and complexity
than the apps do. A LINSTOR deployment alone is a JVM controller (~0.5–1 GB
RAM, with its own embedded-etcd HA) plus per-node satellites plus DRBD9 kernel
modules — ~1 GB consumed *before any application data is stored*
(`docs/storage-and-databases.md:104-119`).

## 2. What pmcluster is

A single static Go binary (`pmcluster`) that, on the Swarm leader, owns:

1. a **declarative manifest DSL** (a small, strict YAML — see
   `docs/dsl.md`);
2. a **desired-state store** (a local SQLite database at
   `~/.pmcluster/data.db`);
3. a **control loop** that renders the desired state into Docker Swarm
   services and keeps the two converged;
4. a **backup pipeline** (hourly whole-disk archives, an in-cluster object
   store, and an optional offsite S3/R2 double-write);
5. an **operator console + REST API + CLI**, behind a Traefik auth gate.

On every other node the same binary runs in *standby*: it watches Swarm
leadership, keeps node-local storage directories repaired, and stays idle
until promoted (`pmcluster/internal/cli/leader.go:52`, `:74`).

The same binary also acts as the **cluster lifecycle engine**: `cluster up`,
`cluster update`, `cluster down --purge`, `cluster reset`, `join`, `node
promote|demote`, `stack move`, `backup restore`. These are deliberately
*local-only* operations — they run the engine directly, never through the REST
API (`pmcluster/internal/ARCHITECTURE.md:63-80`).

## 3. The hardware envelope (a design input, not an afterthought)

Every design decision in this RFC is conditioned by the machines it actually
runs on. From the live campaigns:

| Host | Role | Hardware |
|---|---|---|
| `nxt-sw-1-m` | sandbox leader | the "big" node |
| `nxt-sw-2-m`, `nxt-sw-4-m` | sandbox managers | 1 core / 1.8 GB |
| `nxt-sw-3-w` | sandbox worker + backup store | 1 core / 1.8 GB |
| `nextrum-sy-1`, `nextrum-sy-2`, `wafaa` | production | small VPS |

Two consequences follow directly, and both show up as invariants in the code:

- **Heavy platform services must be pinned.** On an all-manager swarm a
  role-based constraint (`node.role == manager`) constrains *nothing*, so
  OpenObserve (a JVM-class workload, ~331 MB RSS observed) could be scheduled
  onto a 1-core node, drive it into swap, starve Raft and cost the swarm its
  leader for ~30 minutes (BUG-A / BUG-09, `docs/test-reports/TC10-…md:271`).
  The fix: `platform_node` defaults to the **leader**, and the backup store is
  pinned to a *non*-storage, *non*-platform node
  (`docs/storage-and-databases.md:260-268`).
- **There is no spare capacity for a storage layer.** This is why Path 1 (pin
  + backup + restore) rather than replication is the HA story.

## 4. Design goals

**G1 — The database is the source of truth; the Swarm is derived.**
> "the sorce of true is the db cluster up and update should update the file and
> and db"
> — user directive, session `ses_f4ebe8026ffeeXnVhrX15OFJeD`, 2026-09-17

Everything Swarm-visible (networks, secrets, configs, services, labels) is
rebuildable from the store. `pmcluster cluster reset` is a non-destructive
rebuild of the whole Swarm side from SQLite
(`docs/storage-and-databases.md:322-324`).

**G2 — One render pipeline for everything.** Platform stacks (`infra`,
`edge`, `observability`, `backup`, `sso`) are DSL manifests with
`platform: true` that flow through exactly the same
`Parse → Interpolate → Validate → BuildIR → ComposeWriter` stages as user
apps (`docs/dsl.md:5-9`, `pmcluster/internal/cluster/embeds/infra-stack.yml:3-8`).
There is one place where a config becomes a Swarm object, one hash function,
one deploy path, one purge path.

**G3 — Content addressing over version counters.** Every Swarm config and
secret is named `<base>_<sha256-first-8-hex>`; identical content reuses the
object, a change mints a new one, and the in-use object is immutable
(`pmcluster/internal/cluster/secrets.go:105-109`, `:216-209`). Rotation is
therefore safe by construction.

**G4 — Every manual intervention is a bug.** The testing doctrine quoted in
the README. If an operator has to `docker service update` something by hand,
either the platform has a bug or it is missing a convergence behaviour. This
goal is what produced the reconcile loop, the drift label, `secret heal`, the
storage-node adoption pass and the worker-standby fix.

**G5 — Fail loudly, never silently.** A failed deploy must look drifted to the
next pass (the rendered hash is stamped **only after a successful apply** —
BUG-018); a restore that cannot reach the owning node fails with "this stack's
volume lives on `<node>` — run the restore there" rather than writing into the
wrong disk; a webhook deploy without `repo_url`+`file` provenance is a 400.

**G6 — Cheap by default, HA by option.** The default topology is one VPS.
Backups are hourly and local. Offsite, multi-manager HA, storage failover and
the load-balancer layout are opt-in layers, each with a documented cost
(`docs/network-topology.md:37-144`).

**G7 — The operator must be able to reason about state from the console.**
Status is *never* colour alone, and never a state the platform did not
actually observe: "unknown ≠ zero" is a first-class UI state
(`pmcluster/DESIGN.md:230-243`). Badges read the `stack_status` DB snapshot,
never live Docker, so they cannot flicker or lie
(`docs/control-loop-design.md:66-68`).

## 5. Explicit non-goals

These are not omissions; they are rejections, recorded here so they are not
re-proposed (see also `docs/improvements.md` and Chapter 03):

- **No block-level replication.** LINSTOR/DRBD9 is off the roadmap
  (`docs/storage-and-databases.md:46-59`, `:178-187`).
- **No Kubernetes.** The IR is backend-neutral (`pmcluster/internal/ARCHITECTURE.md:8-17`
  and `docs/improvements.md:72-88` describe the writer seam), but no second
  writer exists and none is planned.
- **No shared data plane.** No NFS/GlusterFS/JuiceFS deployment by the
  platform; shared files are a bring-your-own pattern
  (`docs/storage-and-databases.md:160-187`).
- **No git access from the edge.** `repo_url` is provenance metadata only —
  pmcluster never clones or reads a repository
  (`docs/dsl.md:43`, `docs/webhook.md:95-100`).
- **No raw-compose deploys.** `deploy --compose` was evaluated and discarded
  (`docs/improvements.md:42`).
- **No auto-restart on unhealthy.** The control loop reports health; it never
  force-restarts a service. Crash-loop retry is Swarm's restart-policy job
  (`docs/control-loop-design.md:58-60`). The one deliberate exception is the
  opt-in `storage_failover` move.
- **No automatic move-back after failover.** A stack that failed over stays
  where it landed until an operator acks or moves it
  (`docs/storage-and-databases.md:92-99`).
- **No per-node ephemeral firewall ports for data transit.** See DEC-11.
- **No UI in the backup store.** "if there other option than minio without UI
  it is not needed we need to configuer and forget"
  — user directive, 2026-10-02 (this is why MinIO was replaced by SeaweedFS).

## 6. Who this RFC is for

Someone reading the code for the first time (a future maintainer, or an
operator debugging a 3 a.m. failover) who needs to know not just *what* the
platform does but *why it does it that way* — including which alternatives
were tried and abandoned. Chapters 02 and 04 answer "what and how"; Chapter 03
answers "why"; Chapters 05–06 answer "how it got here"; Chapter 07 answers
"what still bites you".

# Poor Man's Cluster: Storage & Database Architecture Guide

`Poor Man's Cluster` (pmcluster) is designed to be lightweight, unopinionated, and highly cost-effective. Instead of locking you into a complex, resource-heavy distributed filesystem (like Kubernetes CSIs or Longhorn), the stack standardizes on a **single host root mount**:

```
/var/stack/data
```

How you configure, format, or replicate the underlying host storage at `/var/stack/data` is entirely up to you. Whether you choose a single $5/month VPS or a globally distributed multi-node cluster, this guide covers the supported storage options, database architectures, and backup strategies.

---

## The zero-effort rule: pin stateful services to a specific node

If you have a **stateful service** (anything that writes to a volume — a SQLite app, a database, a file store) and you **do not want to deal with storage replication, mirroring, or any of the machinery above**, the supported answer is deliberately simple:

> **Pin the service to one specific node with `placement: <hostname>`.**

That is the whole fix. The service runs on that one node forever, its volume-backed data lives under `/var/stack/data/<app>/<name>` on that node and never has to migrate, and nothing else needs to be configured — no NFS, no DRBD, no LINSTOR, no mirroring cron, no distributed database. The nightly backup agents still cover it.

```yaml
services:
  db:                       # stateful: SQLite / Postgres / anything with a volume
    image: ghcr.io/you/db
    placement: node-01 # ← a SPECIFIC node hostname, not "manager"/"worker"
    volumes:
      - db_data:/var/lib/db
```

Why a specific node and not `manager` / `worker`?

* `placement: manager` means *any* manager — if you later add a second manager (or promote the worker), Swarm may reschedule the task onto the other node, where its `/var/stack/data` is empty → the app starts with fresh data or fails on a missing bind source.
* `placement: <hostname>` pins to exactly one machine. Swarm captures node hostnames at join time (`pmcluster join --hostname`), so the pin always refers to a real, stable name. `pmcluster cluster up` prints the available hostnames after deployment.

This is the recommended default for every stateful workload on the platform. Only reach for the replication strategies in this document when a service genuinely must be able to run on *more than one* node.

---

## Path 1 is the HA answer: pin + backup/restore (design decision)

**Path 1 — pinned placement + backup/restore — is the HA story for the platform.**
Block replication (LINSTOR/DRBD9) is **explicitly not** on the roadmap; see the
honest cost table below for why it was rejected as the 2026-10-02 directive's
default and replaced by Path 1.

**The decision (supersedes the earlier "LINSTOR is the HA prerequisite"
directive).** Stateful services pin to a node (explicit `placement: <hostname>`,
the `storage_nodes` round-robin, or the `platform_node` fallback), their data
lives node-locally under `/var/stack/data`, and nightly offen backups plus
on-demand pre-deploy backups cover it. Multi-manager HA protects the Swarm
control plane (via the Raft-replicated state configs); data HA comes from
**backup + restore + `stack move`**, not from block replication.

**How the platform operationalizes Path 1 (v0.2.139, v0.2.140):**

- **`storage_nodes` setting** — comma-separated hostnames; stateful stacks
  (services with volumes and no explicit placement) round-robin deterministically
  across them (FNV-1a of the stack name). Explicit `placement:` always wins.
  The `platform_node` setting remains the single-node fallback when
  `storage_nodes` is empty.
- **The main node is a storage node by default (v0.2.140)** — `cluster up`
  writes the leader's hostname into `storage_nodes` when the setting is empty,
  so a fresh cluster pins every stateful stack to the main node from day one.
- **`pmcluster join --storage-node` (v0.2.140)** — opt-in flag on `join`: after
  joining the Swarm the node's hostname is appended to the manager's
  `storage_nodes` setting (over ssh). **Workers qualify — storage is not
  leader-only.** If the ssh registration fails the join still succeeds and
  prints the manual `pmcluster cluster settings set storage_nodes=...` command.
  `storage_nodes` is hostname-based and role-agnostic: round-robin placement,
  the outage pause, and `stack move` all treat a worker storage node exactly
  like a manager one.
- **`io.pmcluster.node` label (v0.2.140)** — every deployed service carries a
  label naming the node its placement pin targets (a resolved storage node or
  an explicit hostname pin; role-based and unconstrained services carry none).
  The console Services table, the stack-detail services panel, and
  `pmcluster service ps` display this node, so you can see at a glance where
  every stateful service runs.
- **Outage pause (control loop)** — when a pinned storage node is down
  (absent from the swarm, `Status != ready`, or `Availability != active`), the
  reconcile loop **skips syncing/deploying** stacks pinned to it and logs a
  warning; it clears automatically when the node returns or the stack is moved.
- **`pmcluster stack move <stack> --to <node>`** — backs up the volume root,
  restores the stack's `<stack>/` subtree on the target (locally, or via a
  one-shot swarm mover for remote nodes), writes the per-stack
  `stack_pin_<stack>` pin, and re-deploys. The pin outranks the round-robin, so
  the loop never moves the stack back. This is the *manual* data-migration
  story Path 1 gives operators for node maintenance / failure.

**Why not LINSTOR (the honest cost that rejected it):**

| Cost center | What it actually costs |
| :--- | :--- |
| LINSTOR controller | JVM, ~0.5–1 GB RAM, and it needs its own HA (embedded etcd) |
| Per-node satellites | ~50–150 MB each + DRBD9 kernel module (DKMS — rebuilds on kernel updates, needs kernel headers) |
| Disk provisioning | Builds LVM thin pools — must **NEVER** auto-provision on a live disk without explicit operator confirmation of the target device |
| Failure mode: footprint | ~1 GB RAM consumed before any app data is stored |
| Failure mode: complexity | DRBD/LVM complexity with zero benefit on a single node |
| Failure mode: operational | A replicated device is a second copy to keep consistent — and still needs the backup pipeline on top |

For the target deployments (single VPS, or a small multi-node swarm), Path 1's
pinned placement + nightly backups + on-demand `stack move` delivers the same
recovery outcomes (restore onto any node from the archive, or move live) at a
fraction of the footprint and with no kernel modules. Revisit block replication
only if a workload genuinely cannot tolerate a minutes-scale restore window.

---

## Storage Decision Flowchart

Use the flowchart below to choose the right storage paradigm for your workload:

```mermaid
flowchart TD
    Start([What is your deployment topology?]) --> SingleNode[Single Node / Single VPS]
    Start --> MultiNode[Multi-Node Swarm Cluster]

    %% Single Node Branch
    SingleNode --> LocalMount[1. Direct Local SSD Mount]
    LocalMount --> BackupLitestream[Pair with Litestream or pgBackRest to S3/R2]

    %% Multi Node Branch
    MultiNode --> Stateful{Stateful service?<br/>writes to a volume}
    Stateful -->|Yes, and you don't want<br/>storage machinery| Pin[Pin it: placement: &lt;hostname&gt;<br/>runs on one node, data never migrates]
    Stateful -->|Yes, must run on<br/>any node| WorkloadType{What type of workload?}

    WorkloadType --> DBWorkload[Database / Transactional Data]
    WorkloadType --> SharedFiles[Shared Files / Media Uploads]

    %% DB Workload Branch
    DBWorkload --> DBEngineChoice{Where should replication happen?}
    DBEngineChoice --> DBLevel[Database Level <br/>CockroachDB / Galera / Patroni / rqlite]
    DBEngineChoice --> OSLevel[OS / Block Level <br/>DRBD + LINSTOR]

    %% Shared Files Branch
    SharedFiles --> NetworkType{Are nodes on the same LAN or across WAN?}
    NetworkType --> LAN[Same LAN / Private VPC]
    NetworkType --> WAN[Global WAN / Multi-Cloud]

    LAN --> HostNFS[2. Host-Level NFS / GlusterFS]
    WAN --> JuiceFS[3. JuiceFS / SeaweedFS + S3]
```

---

## Database Architectural Strategies

When hosting databases without paying for expensive cloud-managed database services (RDS, Cloud SQL), choose one of the following approaches based on your consistency and availability needs.

### 1. Embedded / Lightweight Databases (SQLite)

If you want zero database complexity and ultra-low RAM usage (~20 MB):

* **SQLite + Litestream (Recommended):** Run SQLite on a local host volume inside `/var/stack/data`. **Litestream** streams Write-Ahead Log (WAL) changes continuously to cheap S3-compatible object storage (e.g., Cloudflare R2). If a node fails, the database restores automatically in seconds.
* **Distributed SQLite (rqlite / LiteFS):** Uses Raft consensus or FUSE page replication across nodes for live high availability without traditional database servers. rqlite speaks an HTTP API (`POST /db/execute?q=…`), so an app must talk to it over HTTP rather than opening a SQLite file — the replication happens inside the engine, on node-local volumes, so **no shared storage is needed**.

### 2. Native Cluster-Replicated Databases (CockroachDB / MariaDB Galera)

Instead of replicating storage disks at the OS level, let the database handle multi-node replication natively across standard local host mounts (`/var/stack/data`):

* **CockroachDB / YugabyteDB:** Distributed SQL databases using Raft consensus. Each node uses its own local SSD mount inside `/var/stack/data`. All nodes act as active, fault-tolerant gateways.
* **MariaDB Galera Cluster:** Multi-master synchronous replication where writes to any node's local disk sync instantly across the cluster over internal overlay networks.

### 3. Primary-Replica with Host-Level Block Replication (DRBD + LINSTOR)

If you run standard single-instance databases (PostgreSQL, MySQL) and need transparent, byte-for-byte block replication across low-latency private nodes:

* **DRBD + LINSTOR:** Acts as "RAID 1 over the network" inside the Linux kernel. It presents a synchronous block device (e.g., `/dev/drbd1000`) mounted at `/var/stack/data`. If Node 1 dies, Node 2 mounts the identical replicated disk with **0% data corruption risk**.
* **Operational note:** LINSTOR costs roughly 1–1.5 GB RAM cluster-wide (JVM controller + satellites) plus the DRBD kernel module and two firewall port ranges (`3370/tcp`, `7788–7799/tcp`) between the node IPs. Raw DRBD9 without LINSTOR is lighter (~150 MB) but requires manual resource management.

---

## How pmcluster enforces the layout

The DSL and deploy pipeline **force every container volume under the volume root**, so the storage story is enforced, not aspirational:

* **`volume_root` setting** — a single host directory (default `/var/stack/data`), configurable via `pmcluster setup --volume-root` or `pmcluster cluster settings set volume_root=…`.
* **Named volumes** are auto-collected from service mounts (`db_data:/var/lib/postgresql/data`) and declared with `driver_opts: {type: none, o: bind, device: <root>/<app>/<name>}` — Docker's first-use ownership copy still runs for DB images, so Postgres/MySQL initialize correctly.
* **Host bind mounts** (`/host/config:/etc/app/config`) are relocated to `<root>/<app>/<basename>`.
* **Every volume — named or bind — lands under `<root>/<app>/…`**, and no app can reach another app's data through pmcluster (raw `docker stack deploy` bypasses this, but that user is root on the host already).

---

## Comprehensive Storage Matrix

| Engine / Option | Topology | Best For | DB Safe? | Operational Complexity |
| :--- | :--- | :--- | :---: | :---: |
| **Direct Local Host Mount** | Single Node | Single-server setups, SQLite + Litestream | **YES** | **Minimal (Default)** |
| **DRBD / LINSTOR** | Multi-Node (LAN) | PostgreSQL/MySQL block-level failover | **YES** | Medium (Host Kernel Modules) |
| **CockroachDB / Galera / rqlite** | Multi-Node (Any) | Native DB-level multi-master consensus | **YES** | Low–Medium |
| **Host NFS / GlusterFS** | Multi-Node (LAN) | Concurrent shared file uploads (RWX) | **NO** | Low–Medium |
| **JuiceFS / SeaweedFS** | Global WAN | Multi-region shared storage backed by S3 | **YES*** | Medium |

\* JuiceFS/SeaweedFS are S3-data-path filesystems, not NFS; single-writer SQL on them still needs care (metadata server + object store), so treat them as safe only for single-writer workloads — same rule as any shared filesystem.

---

## Automated Backup Architecture & Rule

`Poor Man's Cluster` ships a **backup stack** (offen/docker-volume-backup) that runs nightly on every node it is scheduled on:

* **Source:** `/var/stack/data` (read-only) — the enforced volume root captures every app's data.
* **Destination:** `/var/stack/backup` per node (control-plane archives get a `pmcluster-ctlplane-` prefix, 30-day retention; app data tarballs use `backup_retention_days`, **default 15**).
* **Discovery:** scheduled archives on disk are **discovered** into the backup list (console + `pmcluster backup list`), so the full backup picture is visible — nightly runs and on-demand runs alike.
* **Retention:** pmcluster prunes rows **and archive files** older than `backup_retention_days`.
* **Restore:** `pmcluster backup restore <id>` extracts a SUCCEEDED stack-scoped run under `<dest_root>/<stack>`, strips the `/backup/data` prefix, and refuses control-plane archives into the volume root. See `pmcluster/docs/restore-design.md`.

```mermaid
sequenceDiagram
    autonumber
    participant App as Application / DB
    participant LocalDisk as /var/stack/data (Host Volume)
    participant Backup as offen Backup Container
    participant Archive as /var/stack/backup (Host)
    participant S3 as Optional Remote Object Storage (S3 / R2)

    App->>LocalDisk: Write data / logs
    Backup->>LocalDisk: Read snapshot (nightly cron)
    Backup->>Archive: Write compressed tarball (per-node)
    Backup->>S3: Optionally stream to object storage
    pmcluster->>Archive: Discover archives into backup list
```

### Critical Rule for Backup Containers in Replicated Environments

When deploying multi-node Swarm clusters:

1. **Independent Local Disks (Default):** Run the backup container on **all primary and standalone nodes** to ensure local snapshots of `/var/stack/data` are safely archived to S3/R2.
2. **Synchronous Replicated Disks (DRBD / LINSTOR / Shared Storage):** **DISABLE or PIN the backup container to the Primary Node ONLY.**
   * If DRBD/LINSTOR is active, Node 2's underlying block storage is an exact byte-for-byte clone of Node 1.
   * Running active backup jobs simultaneously on secondary nodes will cause resource contention, duplicate backup snapshots, or file lock conflicts.

---

## Quickstart: A pmcluster-managed database stack

This is the same pattern as the `storage-and-databases` architecture above, expressed as a pmcluster **DSL manifest** (see [dsl.md](dsl.md)) — volumes land under `/var/stack/data/<app>/<name>` automatically:

```yaml
app: myapp
domain: myapp.example.com
services:
  db:
    image: postgres:16-alpine
    env:
      POSTGRES_DB: "config(db_name)"
      POSTGRES_PASSWORD: "config(db_password)"   # secret stored in the pmcluster store
    volumes:
      - db_data:/var/lib/postgresql/data
    healthcheck: pg_isready
  web:
    image: ghcr.io/acme/myapp
    expose:
      port: 8080
      host: myapp.example.com
    env:
      DATABASE_URL: "postgres://app:$(config(db_password))@myapp_db:5432/app"
```

* `db_data` is auto-declared and lands at `/var/stack/data/myapp/db_data` (via `driver_opts: none/bind`), so Postgres initializes correctly.
* `db` has no `expose` → it gets no Traefik router and stays on the app's private overlay network, reachable only at `myapp_db:5432`.
* `config(db_password)` resolves from the pmcluster store at deploy time — the manifest never carries secrets.
* Point the backup stack at `/var/stack/data` and the nightly tarball covers `myapp`.

**BYO backup tooling** (e.g. `restic/restic`): perfectly fine as a replacement for the platform's offen agent, but it is *your* service — the platform's discovery, retention, and restore APIs only know about the shipped backup stack. If you bring your own, keep writing to `/var/stack/backup` so the control plane can still see and restore archives.
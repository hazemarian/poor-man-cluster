# TC7 — Two-Node Cluster (1 manager + 1 worker)

**Date:** 2026-10-05
**Version under test:** v0.2.156 (setup) → v0.2.157 (after BUG-014 fix)
**Cluster:** nxt-sw-1-m (leader/manager) + nxt-sw-2-m (worker)

## Objective (user directive)

Select the device with more memory as the leader, install the cluster, join the
worker **without** `--storage-node`. Verify:

1. Storage apps land only on the leader (the storage node).
2. Metrics and logs are flying (OpenObserve ingestion healthy).
3. Platform placement is correct: **Traefik on all nodes, OTel collector on all
   nodes, edge and OpenObserve only on the leader**.

## Procedure (reproducible)

### Leader bring-up — nxt-sw-1-m (2 cores / 3.7Gi, Docker 29.8.2)

```bash
# install + init
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.156 bash
pmcluster init

# admin token (one-time)
pmcluster user create tc7-admin   # -> token saved

# self-signed wildcard cert (90d, SAN nextrum-sy.de + *.nextrum-sy.de + localhost + 127.0.0.1)
openssl req -x509 -newkey rsa:2048 -nodes -keyout /root/certs/key.pem \
  -out /root/certs/fullchain.pem -days 90 -subj "/CN=nextrum-sy.de" \
  -addext "subjectAltName=DNS:nextrum-sy.de,DNS:*.nextrum-sy.de,DNS:localhost,IP:127.0.0.1"

# setup (BYO-cert, no SSO, no offsite backup)
pmcluster setup --domain=nextrum-sy.de --cert=/root/certs/fullchain.pem \
  --key=/root/certs/key.pem --openobserve-email=admin@nextrum-sy.de \
  --hostname=nxt-sw-1-m
```

Result: cluster up complete; `storage_nodes=nxt-sw-1-m` (leader auto-defaulted
as storage node); node label `pmcluster.storage=true` on the leader.

### Worker join — nxt-sw-2-m (1 core / 1.8Gi, Docker 29.8.2)

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.156 bash
pmcluster init
pmcluster join --token=<swarm-token> --manager=10.7.224.12:2377 \
  --role=worker --hostname=nxt-sw-2-m
```

Result: joined as worker, **no `--storage-node`** → node labels empty `{}`,
not a storage node. `docker node ls`: nxt-sw-1-m Leader + nxt-sw-2-m Ready/Active.

### Test stacks (deployed on leader)

- `tc7-stateful.yaml`: `tc7db` postgres:14-alpine, volume `db_data`,
  `POSTGRES_PASSWORD: tc7secretpw`, pg_isready healthcheck, **no placement**.
- `tc7-stateless.yaml`: `tc7web` nginx:1.27-alpine replicas 2, expose
  `tc7web.nextrum-sy.de:80`.

## Verification matrix

| Check | Expected | Result |
|---|---|---|
| `cluster status` | 2 nodes / 1 manager | ✅ |
| storage_nodes setting | `nxt-sw-1-m` (leader) | ✅ |
| Worker storage label | none (`{}`) | ✅ |
| `infra_traefik` | **global on ALL nodes** | ✅ 2/2 (nxt-sw-1-m + nxt-sw-2-m) after v0.2.157 |
| `observability_otel-collector` | **global on ALL nodes** | ✅ 2/2 |
| `observability_openobserve` | replicated, leader only | ✅ 1/1 on nxt-sw-1-m |
| `edge_pmcluster-edge` | replicated, leader only | ✅ 1/1 on nxt-sw-1-m |
| `backup_volume-backup` | global (storage-node constrained) | ✅ 1/1 on nxt-sw-1-m (single storage node) |
| `tc7db_db` (stateful) | leader only (storage pin) | ✅ 1/1 on nxt-sw-1-m |
| `tc7web_web` (stateless) | spreads across both nodes | ✅ 2/2 on both |
| OO log ingestion | records flowing | ✅ 1,568 records |
| OO metrics | reconcile counter present | ✅ `pmcluster_reconcile_total` |

## Bug found

### BUG-014 — Traefik did not run on all nodes (fixed in v0.2.157)

- **Symptom:** `infra_traefik` was `mode: global` but carried the placement
  constraint `node.role == manager` (or `node.hostname == <platform_node>`
  when set) → on a 1-manager swarm Traefik only ran on the leader,
  contradicting the requirement "Traefik on all nodes".
- **Root cause:** the infra-stack embed constrained the ingress proxy to
  manager-role nodes, so global mode could not place a replica on workers.
- **Fix:** removed the placement from the `traefik` service in
  `internal/cluster/embeds/infra-stack.yml` (now global + placement-free).
  Tests: `TestTraefikRunsOnAllNodes` (global, no node.role/node.hostname/
  constraints, with and without PlatformNode); `TestPlatformNodePinsAllStacks`
  and `TestPlatformNodeEmptyKeepsManagerRole` narrowed to the 4 non-ingress
  stacks (Observability, Edge, Backup, SSO).
- **Shipped:** v0.2.157 (commit 2b2b341), verified live — Traefik 2/2 on both
  nodes after redeploy.

## Outcome

All TC7 checks pass on v0.2.157. Platform placement is correct: Traefik and the
OTel collector are global (every node), edge + OpenObserve are pinned to the
leader, backups run on the storage node, storage-bound app stacks land on the
leader only, and stateless stacks spread across both nodes. Observability
(logs + metrics) is healthy.
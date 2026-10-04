# Test Report — TC2: Single manager, custom volume root, Let's Encrypt ACME, backup/restore local + S3

- **Date:** 2026-10-04
- **Release under test:** v0.2.142 (commit `b92f6b5`)
- **Node:** `nxt-sw-3-w` (217.160.128.29, priv 10.7.224.15) — 1 core / 1.8 GiB / 58 G, Ubuntu 26.04.1, Docker 29.8.2, Swarm manager (single node)
- **Goal:** exercise `pmcluster setup` with **Let's Encrypt ACME** (through the load balancer), a **custom volume root** (`/srv/stack/data`), **offsite S3 backup** (IONOS S3, bucket `nextrum-bu`), and full **backup/restore round-trips** (local disk and S3). Record any bugs.
- **Result:** ✅ PASS (2 real product bugs found + 1 environment limitation — see BUG-002, BUG-003, NOTE-1)

---

## How to reproduce (repeatable)

### 0. Prerequisites

- Clean node (previous test cluster destroyed: `cluster down --yes --purge`, daemon stopped, store wiped — see TC1 teardown notes).
- DNS: wildcard `*.nextrum-sy.de` → load balancer `195.20.228.29`.
- Load balancer forwards **TCP 80 → 80** and **TCP 443 → 443** to the node.

```bash
# 1. Install the binary on a clean Ubuntu node (Docker pre-installed, swarm inactive)
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.142 bash

# 2. Init (prints admin API token ONCE)
pmcluster init

# 3. Custom volume root BEFORE bring-up
pmcluster cluster settings set volume_root=/srv/stack/data
```

> ⚠ The volume root set here is **cleared by `cluster up`** (see BUG-002) — you must set it **again after** the cluster is up.

```bash
# 4. Bring up the cluster — Let's Encrypt (HTTP-01 through the LB), no SSO
pmcluster setup --domain=nextrum-sy.de --acme-email=admin@nextrum-sy.de --hostname=nxt-sw-3-w
#   → stacks: infra + edge + observability + backup (all 1/1 healthy)
#   → ACME mode: NO cert_v/key_v secrets (Traefik holds the LE cert internally)

# 5. Re-apply custom volume root + create storage dirs (BUG-002 workaround)
pmcluster cluster settings set volume_root=/srv/stack/data
mkdir -p /srv/stack/data /var/stack/backup
pmcluster cluster update            # re-render backup stack with /srv/stack/data bind

# 6. Configure offsite S3 (IONOS)
pmcluster cluster settings set backup_s3_endpoint=https://s3.eu-central-3.ionoscloud.com
pmcluster cluster settings set backup_s3_bucket=nextrum-bu
pmcluster cluster settings set backup_s3_region=eu-central-3
pmcluster cluster settings set backup_s3_access_key=<access>
pmcluster cluster settings set backup_s3_secret_key=<secret>
pmcluster cluster update            # re-deploy backup stack with S3 env

# 7. Deploy the stateful test stack
#   bookdb: postgres:14-alpine, volume db_data, POSTGRES_DB=bookdb, POSTGRES_USER=bookuser,
#           POSTGRES_PASSWORD=tc2secretpw, healthcheck pg_isready
pmcluster deploy /root/tests/stateful.yaml
#   → volume lands at /srv/stack/data/bookdb/db_data (verified)

# 8. Seed data
docker exec <bookdb_db-container> psql -U bookuser -d bookdb -c \
  "CREATE TABLE orders (id serial PRIMARY KEY, note text); INSERT INTO orders (note) VALUES ('tc2-seed-1'),('tc2-seed-2'),('tc2-seed-3');"

# 9. Backup (local + S3 upload in one shot)
pmcluster backup create
#   → Backup id=1 succeeded, archive /var/stack/backup/backup-<node>-<ts>.tar.gz
#   → offen agent auto-uploads to S3 nextrum-bu (verified via signed ListObjectsV2)

# 10. Local restore round-trip
docker service scale bookdb_db=0 && sleep 3
rm -rf /srv/stack/data/bookdb
pmcluster backup restore 1 --volume bookdb/db_data --dest-root /srv/stack/data
docker service scale bookdb_db=1 && sleep 12
docker exec <bookdb_db-container> psql -U bookuser -d bookdb -c "SELECT count(*) FROM orders;"
#   → count = 3, rows tc2-seed-1/2/3 present ✅

# 11. S3 restore round-trip (local archive deleted → forces S3 fetch)
docker service scale bookdb_db=0 && sleep 3
rm -rf /srv/stack/data/bookdb
rm -f /var/stack/backup/backup-*.tar.gz
pmcluster backup restore 1 --volume bookdb/db_data --dest-root /srv/stack/data --from-s3
docker service scale bookdb_db=1 && sleep 12
docker exec <bookdb_db-container> psql -U bookuser -d bookdb -c "SELECT count(*) FROM orders;"
#   → count = 3 ✅ (1259 files fetched from S3)
```

---

## Verification matrix

| # | Item | Result |
|---|------|--------|
| 1 | Single-manager cluster up (setup command, one manager) | ✅ |
| 2 | Let's Encrypt ACME certs through the LB (HTTP-01 via TCP 80) | ✅ all 3 domains: `pmcluster`/`traefik`/`observ.nextrum-sy.de` — `openssl s_client` Verify return code 0, issuer `Let's Encrypt CN=YR2` |
| 3 | Custom volume root `/srv/stack/data` honored (post-up) | ✅ stateful stack bind `driver_opts` → `/srv/stack/data/bookdb/db_data` |
| 4 | Offsite S3 backup configured + upload verified | ✅ archive object present in bucket via signed ListObjectsV2 |
| 5 | Local backup → restore round-trip | ✅ 3/3 rows recovered |
| 6 | S3 restore (`--from-s3`, local archive deleted) | ✅ 3/3 rows recovered, 1259 files from S3 |
| 7 | Traefik admin-auth middleware still gates the console | ✅ `/web/` → 401 without basic creds |
| 8 | No-SSO mode | ✅ `sso_*` settings empty, no sso stack |

---

## Bugs found

### BUG-002 (product) — `cluster update` does not create storage dirs when `volume_root`/backup dir change

**Severity:** High (breaks any cluster that moves `volume_root` after bring-up; silent until tasks get rejected).

**What happened:** `cluster up` clears the pre-set `volume_root` (the setting is written during `setup`). After re-setting `volume_root=/srv/stack/data` and running `cluster update`, the backup stack was re-rendered with a bind source `/srv/stack/data` that did not exist → `backup_volume-backup` tasks were REJECTED: `bind source path does not exist: /srv/stack/data`.

**Root cause (in code):** `ensureStorageDirs` (`internal/cluster/up.go:49-55`, does `MkdirAll` of volumeRoot + backupRootDir) is called **only** in `up.go` (line 169) and **never** in `update.go`. Any `volume_root` change post-up leaves the new directory missing.

**Fix (applied on node, not in code):** `mkdir -p /srv/stack/data /var/stack/backup` + `docker service update --force backup_volume-backup` → converged 1/1 Running.

**Product fix (todo):** call `ensureStorageDirs` from `cluster update` as well.

### BUG-003 (product) — `pmcluster backup restore` `--dest-root` defaults to hardcoded `/var/stack/data`

**Severity:** Medium (silent wrong-root restore on clusters with a custom `volume_root`).

**What happened:** `pmcluster backup restore 1` without `--dest-root` restored 1259 files to `/var/stack/data` (the hardcoded default) instead of the cluster's actual `volume_root=/srv/stack/data`. The postgres container (bound to `/srv/stack/data`) saw nothing; the restore "succeeded" but was useless.

**Root cause (in code):** `internal/cli/backup.go:80` — `backupRestoreCmd.Flags().String("dest-root", "/var/stack/data", ...)`. The default ignores the cluster's `volume_root` setting.

**Fix (applied on node):** re-ran with explicit `--dest-root /srv/stack/data`.

**Product fix (todo):** default `--dest-root` from the `volume_root` cluster setting when the flag is unset.

---

## Notes / non-bugs

### NOTE-1 — `backup_all_nodes=true` then `cluster update` fails (Swarm limitation, not a bug)

Setting `backup_all_nodes=true` switches the backup stack from replicated → global. `docker stack deploy` refuses in-place mode changes: `service mode change is not allowed`. Reverted to `""` (manager-only) and re-ran `cluster update` successfully. Swarm won't change a service's mode in place — expected Docker behavior, worth documenting.

### NOTE-2 — Let's Encrypt HTTP-01 requires the LB to forward TCP 80

With the LB forwarding only 443, ACME HTTP-01 fails (`Timeout during connect` — LE cannot reach the challenge). After adding the TCP 80 → 80 rule, ACME succeeded. Also note: failed challenge attempts count against LE's rate limit (`too many failed authorizations (5) per domain in last 1h`) — wait for the retry-after window or use a staging CA for iterations.

### NOTE-3 — Restore must happen while the stack is stopped

Restoring a postgres data dir under a **running** container is invalid (postgres re-checkpoints/overwrites the restored files). Correct sequence: `docker service scale <svc>_db=0` → wipe → restore → scale back to 1.

### NOTE-4 — No `cert_v`/`key_v` secrets in ACME mode

With Let's Encrypt, Traefik holds the certificate internally; `docker secret ls` shows no cert/key secrets. Expected.

---

## Follow-ups

- [x] Fix BUG-002 in code (`cluster update` → `ensureStorageDirs`) — shipped in `0acde7a` (v0.2.143)
- [x] Fix BUG-003 in code (restore `--dest-root` defaults from `volume_root` setting) — shipped in `0acde7a` (v0.2.143), both CLI and REST API
- [ ] Re-run TC2 after fixes to confirm both are resolved
# TC4 — Single-Manager Misc Feature Tests

- **Date:** 2026-10-04
- **Version under test:** v0.2.145 (commit ed089b9)
- **Node:** nxt-sw-3-w (217.160.128.29, 1 vCPU / 1.8 GiB / 58 GB, Ubuntu 26.04.1, Docker 29.8.2)
- **Cluster:** single manager, ACME (Let's Encrypt) certs, `volume_root=/srv/stack/data`, offsite S3 backup (IONOS `nextrum-bu`)
- **Result:** 7/9 verified working, **2 real bugs found** (BUG-007, BUG-008)

---

## Repeatable procedure

1. `pmcluster setup --domain=nextrum-sy.de --acme-email=<you>@nextrum-sy.de --hostname=nxt-sw-3-w`
2. `pmcluster cluster settings set volume_root=/srv/stack/data` + `pmcluster cluster update`
3. Configure `backup_s3_*` settings (endpoint/bucket/region/access/secret)
4. Deploy test stacks (webhookapp, bookdb, secretsvc, secondapp, load1..load12)
5. Run each scenario below
6. Cleanup: `docker stack rm <stack>...`

---

## Verification matrix

### 1. Rollback ✅
- webhookapp at rev 1791077364 (replicas 2) → `pmcluster rollback webhookapp 1791077249`
- New rollback revision 1791079844 recorded; service spec updated; replicas 2→1.
- **Caution:** killing the CLI mid `docker stack deploy` leaves the swarm half-applied (only force-updated tasks). Re-run to completion.

### 2. Secret/Config rotation — partial, **BUG-007**
- Config rotation ✅: `pmcluster config edit app_config --value cfg-version-2` → redeploy → container sees `cfg-version-2`. `pmcluster config rollback app_config` restores v1 (history preserved).
- **Secret rotation ❌ (BUG-007):** `pmcluster secret edit app_pass value-two` updates the DB but warns
  `⚠ could not remove existing swarm secret "app_pass" ... in use by the following service: secretsvc_web — mirror skipped`
  → the Docker secret is never updated → redeployed containers still read the old value.
  Root cause: Docker secrets are immutable and in-use secrets cannot be removed. The mirror-skip silently defeats rotation for live stacks. **Needs a versioned-swarm-secret approach or a loud block.**

### 3. Webhook retry ❌ (BUG-008)
- Signed POST with bad image → 202 accepted; delivery row stays `accepted retries=0` even though the deploy fails asynchronously.
- Parse-error payload → 502 with `retries:0`.
- **Code verdict:** the Q3 retry policy is dead code in production — `receiver.go:185` calls `h.Deploy.DeployAsync` directly; `RetryDeployer.DeployAsync` (retry.go:76-78) delegates without retrying; `DeployWithRetries` is only exercised in unit tests. Background-apply failures never re-record the delivery row. **Real bug to fix.**

### 4. Per-stack token scoping ✅
- `pmcluster user create scope-probe --stack webhookapp` → fresh token:
  - `/api/stacks/webhookapp` → 200
  - `/api/stacks/bookdb` (wrong stack) → 403
  - `/api/stacks` (all), `/api/backups`, `/api/webhooks`, `/api/me` → 403
  - admin token → 200
- Old pre-WAL-fix scoped tokens return 401 (stale artifacts of the BUG-004/005 era) — recreate them.

### 5. Backup retention pruning ✅
- Set `backup_retention_days=1`, placed an 11-day-old fake archive → `pmcluster backup list` pruned it from disk; fresh archive retained.

### 6. Multi-domain ✅ (cert pending DNS)
- secondapp with `domain: alt-nextrum-sy.de`, `expose: {host, port}` → Traefik router labels correct (`Host(\`secondapp.alt-nextrum-sy.de\`)`, `certresolver=letsencrypt`).
- Routing works (HTTP 200 via LB); LE cert fails with NXDOMAIN until DNS for the new domain is added — expected, not a bug (serves Traefik default cert meanwhile).

### 7. Disaster recovery drill ✅ (the money test)
- Stopped daemon, moved `data.db` + `.encryption_key` aside (full wipe), started daemon.
- Log: `control-plane data restored from Raft config config=pmcluster_state_... files=4`
- All 5 stacks, all users (incl. scoped tokens), settings restored; `PRAGMA integrity_check = ok`.

### 8. Load sanity ✅ (control plane survives)
- 12× nginx stacks deployed sequentially. 10 succeeded fast; 2 first attempts hit `DeadlineExceeded` but the daemon retried and recorded revisions — resilient.
- Node load reached **22 on 1 vCPU**; services flapped 0/1 (resource exhaustion — environment limitation, not a product bug). **Zero daemon ERR lines**; API responded 200 throughout.

---

## Bugs found

### BUG-007 — Secret rotation silently defeated for live stacks (Medium/High) — FIXED in v0.2.146 (interim; see L4)
`pmcluster secret edit` updates the DB but cannot update the mirrored Docker secret when it is in use by a running service. Containers keep the old value with only a WARN line. **Fix shipped:** versioned swarm secret names — every edit bumps `swarm_rev`, the CLI mirrors into `name_v<rev>` (never removing the in-use original), and the compose writer emits the versioned external name while the container path stays `/run/secrets/<logical-name>`. **Follow-up (design directive, L4 in improvements.md):** unify this DB-counter mechanism with the platform `_v%03d` content-aware versioning — one mechanism for cluster and user objects.

### BUG-008 — Webhook retry policy is dead code (Medium) — FIXED in v0.2.146
`MaxRetries`/`RetryDelay` are wired but never exercised on the receive path; `DeployAsync` never retries; delivery rows stay `accepted`/`server_error` with `retries:0`. **Fix shipped:** the receiver now calls `DeployAsyncWithRetries` under a phase budget; synchronous failures retry (2×, 30s apart) and the real retry count lands on the delivery row and in the 502 body. Background apply failures still surface via stack `last_error` + reconcile.

---

## DSL gotchas learned (documented, not bugs)
- `env:` must be a map, not an array.
- Secrets/config refs require the name in the service `secrets:`/`configs:` array.
- `expose:` is a single object `{host, port}` — arrays invalid; host required.
- No service-level `configs:` array field in the DSL.
- Traefik router labels live on service `Spec.Labels`, not `ContainerSpec.Labels`.

## Environment limits (not product bugs)
- 1-core / 1.8 GiB node OOM-killed `docker ps` during backup under load 17.
- Swarm scheduling degrades (tasks flap) under sustained load 20+.
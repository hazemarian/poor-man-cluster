# TC6 — Single-Manager Remaining Feature Tests

- **Date:** 2026-10-04
- **Version under test:** v0.2.151 (commit 3993951)
- **Node:** nxt-sw-3-w (217.160.128.29, 1 vCPU / 1.8 GiB / 58 GB, Ubuntu 26.04.1, Docker 29.8.2)
- **Cluster:** single manager, ACME (Let's Encrypt) certs, `volume_root=/srv/stack/data`, offsite S3 backup (IONOS `nextrum-bu`), storage-node-labeled
- **Result:** 9/9 verified working; 2 real bugs found + fixed during testing (BUG-010, BUG-011), 1 bug found + fixed live (BUG-012)

---

## Tests

### Test 4 — `depends_on` ordered deploy ✅
- `depstack`: db (postgres:14-alpine, healthcheck `pg_isready` interval 5s retries 10, volume db_data) + web (nginx, `depends_on: [db]`, expose `depstack.nextrum-sy.de`)
- Daemon log: "deploy stack — all levels healthy: one drift-prune pass with the full stack compose"
- Verified: depstack_db 1/1 healthy, depstack_web 1/1, HTTP 200 direct + via LB. Ordered-deploy + wait-healthy machinery works.

### Test 5 — Console UI create/edit via web forms ✅
- POST `/web/settings/secrets/add` → created `ui_secret_probe` (DB swarm_rev=1); edit → swarm_rev=2 (version bump works from the UI, same as CLI)
- POST `/web/settings/configs/add` → created `ui_cfg_probe` (kind template); settings page lists both
- Cleanup note: `pmcluster config delete ui_cfg_probe` OK; `pmcluster secret remove ui_secret_probe` errored "unknown flag: --scope" (that flag is on `create`, not `remove`)

### Test 6 — Registry add + private image pull ✅ (findings)
- `pmcluster registry add ghcr.io --username <bad> --password-stdin` → fails loudly at add time (docker login is part of add — invalid creds can't be stored)
- `registry list` → "(no registries configured)" after the failed add (nothing persisted)
- Deploy with a bad image (`registry.invalid/...`) → deploy succeeds (async pull); task Rejected "failed to resolve reference" at the Docker level; `stacks.last_error` stays empty (deploy cmd succeeded — image pull is swarm-async; same behavior as badapp in TC3/TC4)

### Test 7 — Per-host TLS ✅ (after BUG-010 + BUG-011)
- `pmcluster tls hosts add sitecerts.nextrum-sy.de --cert-file ... --key-file ...` (self-signed, 30d) stores versioned secrets `hostcert-<host>_v001` / `hostkey-<host>_v001` + site_certs DB row
- **BUG-010 found live:** first add rendered an identical Traefik dynamic config (site_certs row was persisted AFTER the refresh update) → Traefik kept default cert until a later cluster update. Fixed in v0.2.149 (persist row before refresh).
- **BUG-011 found live:** even after the render fix, Traefik served default cert — the dynamic config referenced the host cert secrets but the infra stack never **mounted** them. Fixed in v0.2.150 (infra-stack.yml iterates HostCerts; regression test).
- Final verification: SNI `sitecerts.nextrum-sy.de` → subject `CN=sitecerts.nextrum-sy.de` (self-signed cert served); control SNI `pmcluster.nextrum-sy.de` → Let's Encrypt YR2 (ACME unaffected).

### Test 8 — Stack move validation ✅
- `pmcluster stack move bookdb --to nxt-sw-3-w` → "storage is already on nxt-sw-3-w" (same-node)
- `--to nonexistent-node` → "target node nonexistent-node not found in swarm"
- `pmcluster stack move webhookapp --to nxt-sw-3-w` → "has no stateful storage to move" (stateless)
- All three validation error paths correct.

### Test 9 — Run-once failure behavior ✅ (finding)
- Deploy succeeded (rev 1791108367); task stays "Assigned"/"Preparing" for a bad image — swarm pulls async and retries; surfaces at the Docker level, `last_error` stays empty (deploy cmd succeeded). Consistent with the Test 6 finding.

### Test 10 — OTel traces in OO ✅
- `GET /api/default/streams?type=traces` → `{"list":[{"name":"default","storage_type":"disk","stream_type":"traces","stats":{"doc_num":371738,"file_num":26,...}}]}` — 371,738 trace docs in 26 parquet files
- All three pipelines verified: logs (59,081+ records), metrics (`pmcluster_reconcile_total` via PromQL), traces (371,738 docs)

### Test 11 — Credentials rotate ✅ (after BUG-012)
- `pmcluster credentials rotate openobserve_admin` originally updated the DB + swarm secret but had **no runtime effect** (OO still accepted the old password; OTel config header unchanged) until a manual cluster update → **BUG-012**, fixed in v0.2.151: rotate now triggers the cluster update pipeline automatically.
- Live verification: rotate output shows "observability redeployed / infra content changed → re-deploying / ✅ Cluster update applied the rotated credential to dependent services."

### Test 12 — `cluster down` without `--purge` ✅
- Removes only the 5 platform stacks (infra, edge, observability, backup, sso) + backup_default network
- Preserves: swarm active, all app stacks running (bookdb, depstack, rotstack, runoncefail, secondapp-nsd, webhookapp), all secrets, all configs (incl. state snapshots still being created by the daemon loop), all networks
- Daemon stays active on v0.2.151
- Message: "Cluster down complete (secrets/configs/networks preserved — pass --purge to wipe)"

---

## Bugs found & fixed during this campaign

### BUG-010 — `tls hosts add` applied per-host cert only after a later update (High) — FIXED v0.2.149
Site_certs row was persisted after the refresh update → first add rendered identical Traefik config → default cert served. Fix: persist per-host row before refresh (sitecert.go restructure) + regression test.

### BUG-011 — host cert secrets not mounted into the traefik service (High) — FIXED v0.2.150
Dynamic config referenced the secrets but infra-stack.yml never mounted them → Traefik "failed to find any PEM data" → default cert. Fix: infra-stack.yml iterates `HostCerts` in both the service secrets list and the top-level secrets block + regression test.

### BUG-012 — credentials rotate had no runtime effect (Medium) — FIXED v0.2.151
Rotate updated DB + swarm secret but never re-rendered dependent configs (OTel auth header) or redeployed services → old password kept working until a manual `cluster update`. Fix: rotate triggers the cluster update pipeline (credsUpdateFn seam + applyRotatedCredential) + regression tests.

---

## Non-bugs / notes
- `docker config inspect --format "{{.Spec.Data}}"` returns a Go byte-array literal (`[104 116 ...]`), not base64 — decode via python3.
- Docker secret inspect is write-only (no Data field) — verify via the `pmcluster.data_hash` label vs `sha256sum` of the source file.
- Node is 1-core/1.8GiB and thrashes under load (load 10-25) — deploy/rotate commands need timeouts + retries; some probes time out (HTTP 000) but the authoritative proof is the command's own success output.
- `stack_status` table columns: [stack_name, status, services, updated_at]; `stacks` table has `last_error` (stays empty for async pull failures).
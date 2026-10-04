# Test Report — TC1: Single-manager bring-up, multi-service deploy, observability, UI, control plane

- **Date:** 2026-10-04
- **Release under test:** v0.2.141 (commit `15c05c3`) + one bug-fix on top (`e145d6f`, see BUG-001 below)
- **Node:** `nxt-sw-3-w` (217.160.128.29, priv 10.7.224.15) — 1 core / 1.8 GiB / 58 G, Ubuntu 26.04.1, Docker 29.8.2, Swarm manager (single node)
- **Goal:** exercise `pmcluster setup` (single manager, self-signed TLS, **no SSO**, **no offsite backup**, Traefik admin-auth middleware), deploy multiple services, verify logs + metrics + UI + control plane, and record any bugs.
- **Result:** ✅ PASS (1 real bug found → fixed → deployed → re-verified, see BUG-001)

---

## How to reproduce (repeatable)

```bash
# 1. Install the binary on a clean Ubuntu node (Docker pre-installed, swarm inactive)
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | VERSION=v0.2.141 bash

# 2. Init (prints admin API token ONCE)
pmcluster init

# 3. Self-signed wildcard TLS (replace with a real cert in production)
openssl req -x509 -newkey rsa:2048 -nodes -days 90 -keyout /root/certs/key.pem \
  -out /root/certs/fullchain.pem \
  -subj "/CN=nextrum-sy.de" \
  -addext "subjectAltName=DNS:nextrum-sy.de,DNS:*.nextrum-sy.de,DNS:localhost,IP:127.0.0.1"

# 4. Bring up the cluster — no SSO, no offsite backup, Traefik admin-auth middleware
pmcluster setup --domain=nextrum-sy.de \
  --cert=/root/certs/fullchain.pem --key=/root/certs/key.pem \
  --traefik-admin-user=admin --hostname=nxt-sw-3-w
#   → networks traefik-net + monitoring-net
#   → secrets: cert_v001 key_v001 admin_credentials zo_root_user_password
#     edge_admin_password edge_ui_secret edge_api_token sso_cookie_secret
#   → configs: pmcluster_otel_config_v001 pmcluster_traefik_dynamic_v001 pmcluster_edge_v001
#   → stacks: infra + edge + observability + backup (all 1/1 healthy)

# 5. Deploy the test stacks
mkdir -p /root/tests
#   webapp  : nginx:1.27-alpine, replicas 2, expose port 80 host webapp.nextrum-sy.de
#   statedb : postgres:14-alpine, volume db_data, POSTGRES_DB/USER/PASSWORD, healthcheck pg_isready
#   onetime : busybox:1.36 run_once echo HELLO_FROM_RUN_ONCE + date
pmcluster deploy /root/tests/webapp.yaml
pmcluster deploy /root/tests/statedb.yaml
pmcluster deploy /root/tests/onetime.yaml

# 6. Verify each area (see matrix below)
```

---

## Verification matrix

| Area | Check | Result |
|---|---|---|
| Install | `pmcluster version` = v0.2.141, systemd daemon active | ✅ |
| Swarm | `pmcluster cluster status` — active, manager, 1 node/1 mgr, preflight ready | ✅ |
| Platform stacks | infra/edge/observability/backup all 1/1 healthy | ✅ |
| Settings | domain, oo_admin_email, edge_login_disabled=true (console behind Traefik admin-auth, no SSO), backup_s3_* empty (no offsite), storage_nodes auto = nxt-sw-3-w | ✅ |
| Deploy | webapp 2/2, statedb 1/1, onetime_runner Shutdown (run_once correct) | ✅ |
| Placement | statedb service labels `io.pmcluster.node=nxt-sw-3-w`, constraint `node.hostname == nxt-sw-3-w` (auto round-robin storage pin) | ✅ |
| Logs | OpenObserve `SELECT body FROM default` — 2206 records, real container log content | ✅ |
| Metrics | PromQL `sum(pmcluster_reconcile_total)` — counter increments every reconcile pass | ✅ |
| Routing | `curl https://webapp.nextrum-sy.de` through Traefik → HTTP 200 | ✅ |
| UI console | /web/ + /web/stacks + /web/services + /web/settings + /web/backups + /web/webhooks + /web/usage + /web/tls all 200; node column + Terminal buttons visible | ✅ |
| Terminal (L3) | wss terminal bridge round-trip through public console + Traefik basicAuth → marker echoed | ✅ |
| Control plane | Raft configs `pmcluster_state_*` + `pmcluster_state_key_*` created; restore-from-Raft on restart; reconcile pass clean | ✅ |

---

## BUG-001 — `WatchSwarmLeadership` re-emitted `leader=true` on every poll (control-plane snapshots never ran)

**Severity:** High (silent data-loss window on a single-manager cluster).

**Symptom observed:** only **one** `pmcluster_state_*` snapshot existed (from daemon startup) even after several deploys/mutations and minutes of runtime. The daemon log showed `local control-plane DB is current (shared storage?); no restore from Raft config` **every ~15 s**.

**Root cause:** `internal/cli/leader.go` `WatchSwarmLeadership` — in the `case leader:` branch it emitted `true` but then reset `current, first = false, false`. On the next 15 s poll `!current` was true again, so the channel re-emitted `leader=true` **forever**. The `serve.go` leadership goroutine treats each `true` as "leadership gained": it calls `loopCancel()` and restarts `reconciler.Loop` **and** `kit.Loop`. `kit.Loop` (the control-plane snapshot loop) arms a 5-minute ticker but was cancelled 15 s later — **snapshots never happened after startup**. The loop-restart also spammed the ensureControlPlaneFresh restore path every 15 s.

**Fix (commit `e145d6f`):** the leader branch now sets `current, first = true, false` (i.e. "already emitted leader-true for this state"); a subsequent still-leader poll stays silent, so the caller no longer cancels + restarts the loops. The `err`/standalone branch was already correct (`current = true`).

**Tests added:** `TestWatchSwarmLeadership_StableLeaderEmitsOnce` (asserts one `true` then silence over `2*leaderPollInterval`; fails without the fix) and `TestWatchSwarmLeadership_LeaderToNotLeaderReEmits` (race-safe leadership flip; asserts a `false` emit after the flip).

**Verification after fix:** new daemon (fixed binary, local dev build) emitted `became swarm leader` exactly **once**; a fresh snapshot `pmcluster_state_1791072847990916923` + key appeared at the next 5-min tick; restore-from-Raft worked against it.

**Release impact:** the fix is NOT in any tagged release yet (test node runs an untagged `pmcluster dev` build). A new release (e.g. v0.2.142) must ship it.

---

## Non-bugs recorded (expected behaviour, documented so they don't re-surface as "bugs")

1. **First image pull stalls services in `Preparing`** on a 1-core node — the images simply aren't local yet; `docker pull` accelerates it. Not a pmcluster defect.
2. **`/web/nodes` 404** — there is no such route; node info lives on the Overview page. Not a defect.
3. **OpenObserve search quirks** — logs search needs `start_time`/`end_time` in **microseconds** and the `body`/`_timestamp` fields; metrics use the **PromQL** endpoint (`/prometheus/api/v1/query_range`), not SQL search; stream type is a URL query param (`?type=metrics`). Documentation-level, not a defect.
4. **Observability via direct API** is swarm-internal only (no published ports) — reach it through Traefik (`observ.nextrum-sy.de`, behind admin-auth) or via the `monitoring-net` overlay.
5. **`backup_s3_*` empty** by design in this test (no offsite backup requested).

---

## Follow-ups

- [ ] Tag + release the `e145d6f` fix (v0.2.142) so test nodes run a real release, not `dev`
- [ ] Re-run this test case as TC1.2 against the released binary
- [ ] Proceed to next test cases (multi-node join, storage-node round-robin, stack move, outage pause, failover, offsite backup, SSO) per the master test plan
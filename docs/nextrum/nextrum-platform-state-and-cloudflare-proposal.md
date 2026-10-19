# Nextrum Platform — Stack, Current State & Cloudflare Proposal

**Compiled:** 2026-10-16, ~12:00–13:30 UTC · by live inspection over SSH (all four nodes) + external probes
**Scope:** the `nxt-sw-*` Docker Swarm serving `nextrum-sy.com` client sites, plus the idlebbookfair.com site
**Status:** every service running (post-incident recovery complete) — see §2 and §9 for what is still fragile
**Conventions:** facts verified by command output unless marked *(DCD — needs the IONOS dashboard)* or *(unverified)*

---

## 1. Executive summary

| | |
|---|---|
| Cluster | 4-node Docker Swarm (Docker 29.8.2) on IONOS VMs, managed by **pmcluster v0.2.187** |
| Client sites | 13 app stacks on `*.nextrum-sy.com` + `www.idlebbookfair.com` |
| Edge | IONOS-side **L4 load-balancer VIP 195.20.228.29** → node ingress → **Traefik 3.6.5** → apps |
| Current state | **All 25 swarm services running** (2 are completed one-shot jobs). Recovery after a ~6.5 h outage on 2026-10-15 |
| Biggest risks | (1) no alerting anywhere → outages are discovered by hand; (2) 11 stacks hard-pinned to one node (nxt-sw-1-m) with node-local data; (3) observability store (Parseable) is OOM-prone and pinned to that same node; (4) the public front is an unmanaged IONOS black box with no WAF/caching/health visibility; (5) TLS renewal path via pmcluster is broken (`tls_cert_path` points at a legacy migration dir) while the Sectigo wildcard expires **2026-12-05** |
| Cloudflare opportunity | Health-checked, WAF-protected, cache-accelerated edge that removes the single-IP front; free-tier covers most of it (proxied wildcard + Free Managed Ruleset + Access ≤50 users + Tunnel); Load Balancing is the paid upgrade |

---

## 2. What just happened (incident log, 2026-10-15/16)

| Time (UTC) | Event |
|---|---|
| 15 Oct ~16:20 | `observability_openobserve` on nxt-sw-1-m OOM-killed (`exit 137`) |
| 15 Oct 16:44:50 | Peers' gossip: `memberlist: Marking <id> as failed, suspect timeout reached (2 peer confirmations)` |
| 15 Oct 16:46 | nxt-sw-1-m marked `down / heartbeat failure`. Node is half-dead: ICMP + TCP SYN-ACK on 22/2377/7946 answer, but SSH dies at banner exchange → **no shell path, no in-guest fix** |
| 15 Oct 16:46–23:13 | **11 client stacks Pending** (`no suitable node (scheduling constraints not satisfied on 3 nodes; 1 node not available for new tasks)`); public hosts return Traefik `404 page not found` or hang; kernel logs `IPVS: rr: FWM … no destination available`. Unaffected: syrian-halls, idlib-book-fair (unpinned) |
| 15 Oct 23:13 | **Hard reboot from the IONOS console** — node rejoins, becomes leader, all services `1/1` |
| 16 Oct | pmcluster upgraded **v0.2.184 → v0.2.187** (daemon + edge); the cert reconcile error that had fired every minute disappeared; observability backend switched **OpenObserve → Parseable v2.8.0** |
| 16 Oct 09:42 / 09:46 | **Parseable OOM-killed twice** on nxt-sw-1-m (768 MiB cap, node has no swap) |

Root cause of the wedge: not proven (no logs readable while frozen); the OOM minutes earlier on a 4 GB node with **zero swap** is the most likely trigger.

---

## 3. Nodes

| Node | Role | Private IP | Public IP | Resources | Labels | Notes |
|---|---|---|---|---|---|---|
| **nxt-sw-1-m** | manager, **Leader**, storage_leader + platform_node | 10.7.224.12 | 217.160.143.22 | 2 vCPU / 3.7 GiB / 116 GB (18 % used) | `pmcluster.storage=true` | Carries ~13 pinned services + platform (parseable, edge, sso, control-plane-backup); holds the live `/var/stack/data/*`; no swap; memory hot spot |
| **nxt-sw-2-m** | manager, Reachable | 10.7.224.13 | 217.160.143.24 | 1 vCPU / 1.8 GiB / 58 GB (13 %) | `pmcluster.storage=true` | Runs the **pmcluster daemon** (`/usr/local/bin/pmcluster serve`, systemd) + SQLite state; ingress + otel + volume-backup |
| **nxt-sw-4-m** | manager, Reachable | 10.7.224.14 | 217.160.143.26 | 1 vCPU / 1.8 GiB / 58 GB (12 %) | `pmcluster.storage=true` | Ingress + otel + volume-backup + the only two served apps that are not pinned elsewhere (idlib-book-fair, syrian-halls) + demoenv |
| **nxt-sw-3-w** | worker | 10.7.224.15 | 217.160.128.29 | 1 vCPU / 1.8 GiB / 58 GB (27 %) | — | Backup S3 store (seaweedfs) + otel collector; no app workloads |

**Placement reconciliation note:** this is a historical snapshot, and the node
notes and service inventory disagree about the two unpinned apps. The node table
places `idlib-book-fair` and `syrian-halls` on `nxt-sw-4-m`; the service table
and the original query notes place them on `nxt-sw-2-m`. Treat their placement
as **unknown** until a single timestamped `docker service ps` capture resolves
it; do not use either table alone for a failover decision.

All four: Ubuntu 26.04.1 LTS, kernel 7.0.0-38, Docker 29.8.2, **no swap**, 19 pending OS updates (~10 security), `unattended-upgrades` active, root SSH **key-only** (`PermitRootLogin prohibit-password`), fail2ban (sshd jail), UFW default-deny in.
Region: all IONOS `fr-nbz` (same DC as the IONOS LB VIP → ~0.5 ms, 1 hop from the edge).
SSH access from deshli works **directly** to all four public IPs (the deshli key is in root's `authorized_keys` everywhere); the nodes themselves hold no private key, so node→node hops need agent forwarding.

---

## 4. Platform layer (pmcluster)

- **Daemon**: `pmcluster.service` on nxt-sw-2-m — REST/webhook API on `0.0.0.0:9090` (UFW-restricted to 172.16.0.0/12), SQLite state at `/root/.pmcluster/data.db`, reconcile loop every ~60 s.
- **Edge**: swarm service `edge/pmcluster-edge v0.2.187`, `:8042`, `UPSTREAM=http://host.docker.internal:9090` (→ the node's own daemon), `EDGE_SSO_ENABLED=true`, `EDGE_LOGIN_DISABLED=true`; routes `pmcluster.nextrum-sy.com` (+ an `/sso-bridge` path on `observ.nextrum-sy.com`); pinned to nxt-sw-1-m with 50m CPU / 64 MiB reservation.
- **Tracked stacks**: 14 (13 app + demoenv). The five **platform stacks** (`infra`, `edge`, `observability`, `backup`, `sso`) are created by `cluster up/update` and are *not* tracked in the DB — nothing reports their health.
- **Cluster settings (selected)**: `domain=nextrum-sy.com`, `storage_nodes=nxt-sw-1-m,nxt-sw-2-m,nxt-sw-4-m`, `storage_leader=nxt-sw-1-m`, `platform_node=nxt-sw-1-m`, `stack_pin_<stack>=nxt-sw-1-m` for 10 stacks, `storage_failover` **unset**, `sso_provider=github`, `sso_github_org=nextrum-sy`, `tls_mode=cert`, `tls_cert_path=/root/migration/certs/nextrum-sy.com.crt` (files exist on nxt-sw-1-m only), `backup_s3_*` (IONOS S3 `eu-central-3`, bucket `pmclusterbackup`).
- **Credentials**: the DB `cluster_settings`/`managed_credentials` tables hold **plaintext** secrets (S3 keys, Parseable admin, SSO client secret); the Parseable password is additionally in the service's plain env and as a base64 Basic header inside the collector config. Anyone with `docker service inspect` / config read access sees them.
- **Reconcile**: currently clean (0 errors in 12 h). Historic error (until the 0.2.187 upgrade) — `ensure cert secret: read /root/migration/certs/nextrum-sy.com.crt: no such file` on node 2 — blocked the cert-ensure step on every pass.

---

## 5. Service inventory (25 services, 19 stacks)

### 5.1 Platform

| Stack / service | Image | Replicas | Placement | Purpose |
|---|---|---|---|---|
| `infra/infra_traefik` | traefik:v3.6.5 | 3/3 (global, managers) | 1, 2, 4 | Edge router: swarm provider + file provider, 80/443, OTLP traces+metrics |
| `observability/observability_otel-collector` | otel/opentelemetry-collector-contrib:0.157.0 | 4/4 (global) | all | Receives OTLP (:4317/:4318), docker_stats, filelog; exports to Parseable |
| `observability/observability_parseable` | parseable/parseable:v2.8.0 | 1/1 | **1 (pinned)** | Log/metric/trace store + UI (`observ.nextrum-sy.com`) |
| `observability/observability_retention` | curlimages/curl:8.10.1 | job (0/1 when idle) | 1 | Applies retention via Parseable API (30 d logs / 14 d metrics / 10 d traces) |
| `edge/edge_pmcluster-edge` | ghcr.io/hazemarian/pmcluster-edge:v0.2.187 | 1/1 | **1 (pinned)** | pmcluster UI/API gateway |
| `sso/sso_oauth2-proxy` | bitnami/oauth2-proxy:v7.11.0 | 1/1 | **1 (pinned)** | SSO forward-auth for pmcluster/observ (`sso-auth@file`) |
| `backup/backup_seaweedfs` | chrislusf/seaweedfs:4.48 | 1/1 | 3 (pinned) | S3 backup store (15 GB) |
| `backup/backup_volume-backup` | offen/docker-volume-backup:v2 | 3/3 (global on storage nodes) | 1, 2, 4 | Per-node volume dumps → S3 |
| `backup/backup_control-plane-backup` | offen/docker-volume-backup:v2 | 1/1 | **1 (pinned)** | pmcluster state/DB backups |
| `demoenv/demoenv_web` | nginx:alpine | 1/1 | 4 | Demo environment |

### 5.2 Client applications (all `ghcr.io/nextrum-sy/*`, pinned `node.hostname == nxt-sw-1-m` unless noted)

| Stack | Image | Host(s) | Public status |
|---|---|---|---|
| abbas | abbas:v20261013-770dcfc | abbas.nextrum-sy.com | 200 |
| abbas-cms | simple-cms:v20261013-76603b1 | abbas-cms.nextrum-sy.com | 302 (login) |
| dg-steps-erp | simple-erp:v20261014-1692554 | dg-steps-erp.nextrum-sy.com | 302 |
| horizon-erp | simple-erp:v20261014-1692554 | horizon-erp.nextrum-sy.com | 302 |
| shash-erp | simple-erp:v20261014-1692554 | shash-erp.nextrum-sy.com | 302 |
| jammo-cms | simple-cms:v20261013-76603b1 | jammo-cms.nextrum-sy.com | 302 |
| khaal-pvc-cms | simple-cms:v20261013-76603b1 | khaal-pvc-cms.nextrum-sy.com | 302 |
| property-management | property-management:v20261013-bb67346 | property-management.nextrum-sy.com | 303 |
| test-lms | simple-lms:v20261001-e3a4852 | test-lms.nextrum-sy.com | 302 |
| donation-campaign-stg | donation-campaign:25e0170 (api) + postgres:14-alpine (db) + migration job | donation-campaign-stg.nextrum-sy.com | `/` 404 is the app's own (gin); `/health` 200 |
| donation-stg-web | donation-campaign-frontend:2248c45 | campaign-stg.nextrum-sy.com | 200 |
| **idlib-book-fair** | idlib-book-fair:v20261013-46ea434 | idlib-book-fair.nextrum-sy.com, **www.idlebbookfair.com** | 200 (unpinned, runs on node 2) |
| **syrian-halls** | syrian-halls:v20261013-ec9ee20 | syrian-halls.nextrum-sy.com | 307 (unpinned, runs on node 2) |

Two services show `0/1` and are **normal**: `donation-campaign-stg_migration` (one-shot DB migration) and `observability_retention` (periodic job).

### 5.3 Domain → service map (all client hostnames resolve to the LB VIP)

`*.nextrum-sy.com` is a **single wildcard**; routers exist for the 17 hostnames above plus `pmcluster.`, `observ.`, `sso.`, `traefik.` (404 – no dashboard router), `demoenv.`, `campaign-stg.`.
**Missing routers / records:** apex `nextrum-sy.com` (NXDOMAIN — no DNS record), `www.nextrum-sy.com` (resolves, no Traefik router → 404).
**Port 80:** every real hostname returns Traefik `404` (no HTTP→HTTPS redirect router configured).

---

## 6. Data, storage and backups

- Every stateful stack renders `driver: local` + `driver_opts: {type: none, device: /var/stack/data/<stack>/<vol>}` → **a bind on one node's disk, not shared storage**. That is why stacks are hard-pinned and why a node loss is an outage, not a failover.
- Live data currently on nxt-sw-1-m: `abbas`, `abbas-cms`, `dg-steps-erp`, `donation-campaign-stg/db_data`, `horizon-erp`, `jammo-cms`, `khaal-pvc-cms`, `property-management`, `shash-erp`, `test-lms` (+ platform). Nodes 2/4/3 host only their own platform volumes.
- **Stale pre-pin copies** exist on nodes 2 and 4 (`/var/stack/data/{abbas-cms,horizon-erp}` on 2; `{dg-steps-erp,property-management,shash-erp}` on 4 — created 2026-10-13 14:26, before pinning at 14:32). A repin/failover without moving data would mount that old data.
- **Backups**: `volume-backup` (global on the three storage nodes) dumps each node's volumes into `/var/stack/backup` and pushes to IONOS S3; `control-plane-backup` covers pmcluster's state. Last successful runs: **15 Oct 19:00 UTC**. A node that is down contributes nothing that night — so 15 Oct's backup set is incomplete for node 1 (rebooted 23:13).
- **Sizing reality**: 194 MB of Parquet in Parseable total; app data volumes are kilobytes-to-low-megabytes. Storage is not the constraint — availability is.

---

## 7. Ingress path and what the front actually does

```
client → DNS (*.nextrum-sy.com → 195.20.228.29)
       → IONOS L4 load-balancer VIP  (VIP 195.20.228.29; egress/health-check IPs 195.20.237.152/.153;
                                      listeners 80+443 only; 22/25/143/993 closed; ICMP dropped;
                                      net `fr-nbz-kw-ngcs-public`)
       → node public IP (ingress routing mesh, published 80/443 on all four nodes)
       → Traefik task on a manager (TLS terminated HERE, not at the LB)
       → per-host router → service VIP → container
```

Verified front properties:
- **TCP/L4 passthrough, no TLS termination**: identical TLS fingerprint to a direct node hit (TLS 1.3, `TLS_AES_128_GCM_SHA256`, X25519, ALPN h2) and it serves the origin's per-host certs (Sectigo `*.nextrum-sy.com`; **Cloudflare Origin CA** for `www.idlebbookfair.com`). Unknown SNI/Host → Traefik default cert + `404 page not found`.
- **No HTTP-layer behaviour**: headers byte-identical through the LB vs direct; no `Via`/`Server`/XFF injected; HTTP/1.1 and WebSocket upgrades pass.
- **Backends = the nodes, with active TCP health checks**: a capture on nxt-sw-3-w (worker, no Traefik/apps) while idle showed SYNs from 195.20.237.152/.153 into :443 immediately FIN'd. Node-level TCP checks — they cannot detect "node up but apps Pending" (exactly what happened).
- **Client IPs are lost inside the cluster**: containers see `10.0.0.x` (ingress VIP); e.g. oauth2-proxy logs `10.0.0.11`/`10.0.0.25` for real visitors. Traefik access logs are off (`--accesslog=false`), so the LB is invisible from inside.
- **TLS material**: Sectigo DV wildcard `*.nextrum-sy.com` (issued 2026-06-08, **expires 2026-12-05**); per-host **Cloudflare Origin CA** cert for idlebbookfair.com (long-lived); pmcluster `tls_mode=cert` with `tls_cert_path=/root/migration/certs/…` (files present on node 1; node 2 has no such path).
- *(DCD)* the LB's backend pool, algorithm, stickiness and monitor configuration live in the IONOS dashboard, not in anything we can read.

---

## 8. Observability (what is actually running)

**Pipeline:** apps/logs + Traefik → **OTel Collector (global)** → **Parseable** (store + UI) ← **retention job**.

- **Collector** `otel/opentelemetry-collector-contrib:0.157.0`, global (4/4), config `pmcluster_otel_config_c4455752`:
  receivers `otlp` (gRPC 4317 / HTTP 4318), `docker_stats` (10 s, docker.sock, swarm labels promoted to `service_name`/`stack_name`/`swarm_node_id`), `filelog` (Docker JSON logs, excluding otel/parseable/the three backup services/traefik; `start_at: end` — the config documents that `beginning` re-sent all history on restart and pushed Parseable to 764 MB against its 768 MB cap);
  processors `memory_limiter` (320 MiB + 80 spike, first in every pipeline), `filter/skip_filelog`, `resource`, `transform/metrics`, `resourcedetection`, `batch`;
  exporter `otlphttp` → `http://observability_parseable:8000/v1/{logs,metrics,traces}` with `encoding: json` (Parseable rejects protobuf), `X-P-Stream: pmcluster-logs|metrics|traces`, bounded retry/queue (128);
  limits 500m CPU / 768 MiB; publishes **4318 on 0.0.0.0** (kept closed by the external firewall only).
- **Parseable v2.8.0**: single replica pinned to nxt-sw-1-m, Parquet on `observability_parseable_data` (194 MB) + `_staging`, streams `pmcluster-logs`, `pmcluster-metrics`, `pmcluster-traces`, `pstats`; ingest confirmed live (72 new objects in 10 min); UI behind `sso-auth` + `observability-auto-auth`.
- **Retention**: Parseable has no env/flag/config retention — a one-shot curl job applies it via API: logs 30 d, metrics 14 d, traces 10 d (re-armed by the reconcile pass, ~hourly).
- **Traefik**: sends traces **and** metrics over OTLP gRPC (10 s push, `serviceName=traefik-proxy`).

**Gaps**
1. **No alerting at all** — no Alertmanager/Prometheus, no notification path; Parseable logs `Failed to load targets` (its alert-targets file is unconfigured). Nothing would have paged during the 6.5 h outage.
2. **App-side OTLP is broken where configured**: `abbas`/`abbas-cms` set `OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317` (DNS resolves → 10.0.2.12) but the SDKs fail with `delegating_resolver: invalid target address ""` — **162 errors in 15 min** on abbas. Traces effectively come from Traefik only.
3. **No node-level metrics** — container stats only; no node_exporter/cAdvisor → host CPU/RAM/disk/IO invisible.
4. **Single point of failure** — store + retention pinned to nxt-sw-1-m: yesterday's outage took logging/metrics with it (collectors only buffer 128 batches before dropping).
5. **Config/secret drift** — 3–4 collector configs exist (one stale copy still `start_at: beginning` + OpenObserve exclusions), plus three orphaned `zo_root_user_password` secrets from the OpenObserve era.
6. **Credential handling** — Parseable admin in plain service env; same credential base64'd into the collector config.

---

## 9. Issue register

| # | Severity | Issue | Impact | Suggested action |
|---|---|---|---|---|
| 1 | **High** | No alerting on swarm task state / node health / OOM | Outages persist unnoticed (6.5 h precedent) | Arming watchdog (Hermes cron → Telegram) + CF health-check notifications |
| 2 | **High** | 11 stacks pinned to nxt-sw-1-m; node-local `/var/stack/data`; `storage_failover` unset | One node = 11 client sites down, no automatic failover | Enable `storage_failover` and/or spread pins across nodes 1/2/4 after moving data |
| 3 | **High** | Sectigo wildcard expires **2026-12-05**; pmcluster cert-ensure path points at a legacy dir | Cert expiry = whole-fleet TLS failure | Fix `tls_cert_path` / move to CF Origin CA behind Cloudflare |
| 4 | **Medium** | Parseable OOM-prone (768 MiB cap, no swap, on the busiest node) | Loss of observability exactly when needed | Raise limit + reserve, add swap, or move it / add node metrics |
| 5 | **Medium** | No HTTP→HTTPS redirect; apex NXDOMAIN; `www` has no router | Broken front door, SEO damage, users typing the bare host see 404 | Cloudflare "Always Use HTTPS" + apex/www records + redirect rule |
| 6 | **Medium** | Public front is an unmanaged IONOS VIP; no WAF/rate limit/bot/caching; client IPs lost | Abuse and volumetric risk; blind forensics | Cloudflare front (§10) |
| 7 | **Medium** | Backups incomplete for a down node; last full run 15 Oct 19:00 | RPO unbounded during incidents | Off-node backup target + nightly verification; cross-check `pmcluster stack show` "Last backup" |
| 8 | **Low** | otel 4318 + seaweedfs 8333 published on 0.0.0.0; only the external firewall blocks them | Exposure if firewall rules change | Bind to internal networks or restrict at the IONOS firewall |
| 9 | **Low** | Plaintext credentials in service env / collector config / DB | Any node compromise leaks S3 + admin creds | Move to swarm secrets; rotate |
| 10 | **Low** | Config/secret leftovers (otel configs, `zo_*` secrets) | Wrong config could be picked on a redeploy | Prune |
| 11 | **Low** | No swap on any node; 1 vCPU workers; 19 pending updates each | OOM kills; unpatched security fixes | Add swap, schedule maintenance, apply security updates |
| 12 | **Low** | App OTLP endpoints invalid (162 errors/15 min on abbas) | Missing app traces/logs | Fix endpoint construction / drop the env if unused |

---

## 10. Cloudflare proposal

### 10.1 Goals
1. **Availability at the edge**: health-checked multi-origin front so one node (or the IONOS VIP) can fail without client-visible downtime.
2. **Security**: WAF, bot and rate limiting in front of client sites and admin surfaces; admin auth at the edge instead of a single pinned container.
3. **Speed**: cache static assets at the edge, modern TLS/HTTP3, fewer origin round-trips.
4. **Operability**: visibility (analytics, health-check notifications), one place for DNS, and a TLS story that does not expire in 7 weeks.
**Non-goals:** fixing node-local storage pins, pod-level scheduling, or in-cluster metric gaps — those are pmcluster workstreams (§12).

### 10.2 Phase 1 — Move `nextrum-sy.com` to Cloudflare (free, ~1–2 h + NS propagation)
Prerequisites: CF account, "Add site → nextrum-sy.com" (Free plan), and an API token with **Zone:DNS:Edit, Zone:Zone Settings:Edit, Zone:Cache Purge** (plus Account:Access later) if the work is to be scripted.
1. **Recreate the zone records before the NS change** — the plan below is the authoritative list (§10.8).
2. **Change NS at IONOS** to the two Cloudflare nameservers. Keep the IONOS zone intact until propagation is verified (`dig NS` + per-host checks) so rollback = revert NS.
3. **SSL/TLS = Full (strict)** — the origin already serves a valid Sectigo wildcard, so strict works immediately. Do **not** use Flexible (would break-origin and invite loops).
4. **Enable**: Always Use HTTPS (**fixes the port-80 404**), Automatic HTTPS Rewrites, min TLS 1.2, TLS 1.3, HTTP/3, Brotli, HSTS (`max-age=15552000`, no preload at first), Opportunistic Encryption.
5. **DNS**: proxy the wildcard `*` (Cloudflare explicitly allows proxied wildcard records on **all plans**), and fix the front door: apex + `www` records with a **Single Redirect** rule (www → apex, or apex → www).
6. **Security (free tier)**: Cloudflare **Free Managed Ruleset** (available on all plans; the full "Cloudflare Managed Ruleset" needs Pro), Bot Fight Mode, Security Level = Medium, Browser Integrity Check, DNSSEC (enable + publish DS at IONOS), CAA records (`letsencrypt.org`, `sectigo.com`, `pki.goog`).
7. **Notifications**: enable SSL/TLS expiry alerts, DNS record change alerts, health-check (Phase 2) and traffic anomaly notifications to the ops mailbox.
8. **Certificate note**: Universal SSL covers `nextrum-sy.com` + `*.nextrum-sy.com` (one level). All client hosts are single-level ✓. Two-level hostnames (e.g. `a.b.nextrum-sy.com`) would need Advanced Certificate Manager (~$10/mo) — currently unused, so a rule to keep it that way is enough.

### 10.3 Phase 2 — Replace the single-IP front with a health-checked pool
Today: one VIP (IONOS LB) → nodes. Options, best first:

**Option A (recommended): Cloudflare Tunnel, one tunnel with four connectors.**
- Deploy `cloudflared` as a **global swarm service** (one connector per node) on the same overlay as Traefik; tunnel ingress → `http://infra_traefik:80` (or the node's ingress) so Traefik keeps doing host routing.
- DNS: wildcard `CNAME → <tunnel-id>.cfargotunnel.com`, proxied. Cloudflare load-balances across connectors automatically; a dead node's connector simply drops out — **automatic failover with no LB add-on**.
- Then **close inbound 80/443** at the IONOS firewall (and drop the UFW 80/443 rules), leaving only 22 (ideally restricted to admin IPs) and the swarm ports on the private network: origin IPs disappear from the internet, the IONOS VIP becomes redundant, and TLS is terminated at Cloudflare (origin can use a CF Origin CA cert).
- Cost: free. Effort: ~3–4 h (service + tunnel config + firewall change + verification).

**Option B: Cloudflare Load Balancer in front of the four node public IPs (or the IONOS VIP as one origin).**
- Pool with an HTTP monitor per origin; steering "dynamic"/least-outstanding; failover on non-2xx/timeout.
- Use a **dedicated health hostname** (e.g. `health.nextrum-sy.com` routed to a tiny always-on service) so the monitor proves app-level routing, not just port liveness — this is the check that would have caught yesterday's Pending state.
- Cost: Load Balancing has a free entry point on Cloudflare's current product page (verify origin/monitor limits for the account); paid tiers add origins/analytics. Effort: ~2–3 h. Keeps the current origin architecture (nodes stay public).
- Immediately retires the IONOS black box from the DNS path if the pool is built from node IPs (keep the VIP only as a documented fallback).

**Option C: free multi-A round-robin (not recommended alone).** Four proxied A records for the wildcard — Cloudflare spreads connections, but without health checks a dead node still receives ~25 % of traffic. Use only as a stopgap.

**Recommended sequence:** Phase 1 → Option A (or B if you prefer keeping public origins) → retire the IONOS VIP from DNS.

### 10.4 Phase 3 — Cloudflare Access for admin surfaces (free ≤ 50 users)
- Applications: `pmcluster.nextrum-sy.com`, `observ.nextrum-sy.com` (+ `traefik.` if it is ever exposed).
- IdP: **GitHub** (the platform already standardises on `sso_github_org=nextrum-sy`); policy = allow that org (optionally specific teams), require MFA, plus **service tokens** for automation/webhooks.
- Effect: auth happens at the edge, **before** the origin — and the node-pinned `sso_oauth2-proxy` becomes redundant (it can stay as a second layer, or be retired to remove a single-point-of-failure service).
- Cost: free up to 50 users, $7/user/month beyond.

### 10.5 Phase 4 — Origin lockdown & abuse control
- IONOS firewall + UFW: allow 80/443 **only** from Cloudflare IP ranges (or from nowhere if Phase 2A tunnels are used); restrict 22 to admin IPs/VPN; keep 2377/7946/4789 on the private interface only.
- Stop publishing internals on 0.0.0.0: `otel` 4318 and `seaweedfs` 8333 should be overlay-only.
- Cloudflare: one **rate limiting rule** (free tier includes one) aimed at the busiest abuse surface (webhooks/API host), WAF custom rules for obvious junk paths, and email-security/DNS hardening as needed.

### 10.6 Phase 5 — Performance & caching
- Cache Rules: cache `/_next/static/*`, `/_astro/*`, `/brand/*`, and image paths (long TTL, immutable); bypass `/api/*`, `/admin/*`, webhook endpoints and anything with `Set-Cookie`.
- Enable Tiered Cache + Smart Tiered Cache, Brotli, HTTP/3, Early Hints, and (optional, paid) Image Resizing / Argo Smart Routing.
- Result: client sites get static assets from the nearest PoP; the 1 vCPU nodes do far less work.

### 10.7 Phase 6 — Monitoring & alerting
- Cloudflare notifications: health-check failures (Option B) / tunnel health (Option A), origin 5xx spikes, DNS changes, SSL expiry, WAF/rate-limit events.
- **Swarm-side watchdog (independent of CF)** — a Hermes cron every 5 min that reads `docker service ls` (replica mismatch), `docker service ps` Pending/Failed tasks, kernel OOM lines and pmcluster reconcile errors, and messages this chat on change. This closes the "no alerting" gap that cost 6.5 h.
- Add node-level metrics later (node_exporter or equivalent) so host saturation is visible.

### 10.8 DNS record plan (what to recreate in Cloudflare, and what to fix/drop)

| Record | Type | Current value (IONOS) | Cloudflare target | Proxy | Notes |
|---|---|---|---|---|---|
| `nextrum-sy.com` | A | **missing** | LB/tunnel/apex origin | 🟠 | Fixes NXDOMAIN; add redirect rule www↔apex |
| `www` | A | 195.20.228.29 | LB/tunnel | 🟠 | Currently resolves but 404s (no router) |
| `*` | A/CNAME | 195.20.228.29 | LB IPs or `<tunnel>.cfargotunnel.com` | 🟠 | Covers all client hosts (single-level cert) |
| `mail` | A | 195.20.228.29 | IONOS mail front *(DCD)* or remove | ⚪ | Points at the web VIP today; no mail ports there → dead record |
| `imap`, `smtp` | A | 195.20.228.29 | same as above | ⚪ | Same |
| `autodiscover` | CNAME | adsredir.ionos.info | keep | ⚪ | IONOS mail client config |
| `_domainconnect` | CNAME | _domainconnect.ionos.com | keep or drop | ⚪ | IONOS convenience record |
| `@` (MX) | MX ×2 | mx00/mx01.ionos.de | keep exactly | ⚪ | Mail delivery — never proxy |
| `@` (SPF) | TXT | `v=spf1 include:_spf-eu.ionos.com ~all` | keep exactly | — | Preserve verbatim |
| `_dmarc` | TXT | `v=DMARC1; p=none;` (via dmarc.ionos.de) | keep (consider `p=quarantine` later) | — | Email auth |
| `default._domainkey` | TXT | **absent** | add if IONOS provides DKIM | — | Deliverability |
| `@` (CAA) | CAA | **absent** | add for the CAs in use | — | Prevent rogue issuance |
| `idlebbookfair.com` | zone | already on Cloudflare NS | migrate the remaining gaps | 🟠 | Its **apex has no record** either; per-host CF Origin CA cert already on the swarm |

### 10.9 Rollback & risk
- Keep the IONOS zone export (zone file/records list) and the current DNS answers before the NS change; rollback = restore IONOS nameservers.
- Introduce changes in this order: records first (DNS-only), verify → flip proxy on → enable SSL/security features → then Phase 2/3.
- Avoid enabling HSTS preload until everything is proxied and verified.
- Tunnels/origin lockdown are the only steps that touch the network path — do them in a maintenance window with the LB VIP still available as a fallback record.
- Watch-outs: proxied wildcard requires the client hosts to stay single-level; `mail.*` records must not be proxied; some Next.js apps set cookies on `/` (keep those routes on bypass so caching never leaks sessions).

### 10.10 Effort / cost / impact

| Phase | Cost | Effort | Impact |
|---|---|---|---|
| 1 — Zone move + baseline | free | 1–2 h | Fixes redirect, apex/www, adds WAF/bot/DNSSEC, TLS policy; single-IP still exists |
| 2A — Tunnel (recommended) | free | 3–4 h | Removes public origins and the IONOS VIP; automatic failover; kills the black box |
| 2B — Load Balancer | free entry / paid tiers | 2–3 h | Health-checked failover; keeps public origins |
| 3 — Access | free ≤ 50 users | 1–2 h | Edge auth + MFA; retires a node-pinned SSO container |
| 4 — Origin lockdown | free | 1 h | Origin only reachable via Cloudflare; internals unpublished |
| 5 — Caching/perf | free (extras paid) | 1 h | Fewer origin hits, faster client sites |
| 6 — Monitoring | free | 1 h | Outages page instead of being discovered |
| **Total** | **$0 (Free plan path)** | **~10–15 h** | Front door fixed, failover possible, abuse controls, visibility |

---

## 11. What Cloudflare does **not** fix (parallel workstream)

1. **Storage locality**: pins + `/var/stack/data` binds mean a node loss is still an outage for its stacks. Needs `storage_failover` + data movement + spread pins (or shared storage such as the existing seaweedfs/IONOS S3 for stateful data).
2. **Observability sizing**: Parseable OOMs on the busiest node (768 MiB cap, no swap) — raise limits/reservations, add swap, or move it.
3. **No node-level metrics**; app OTLP endpoints are broken (abbas: 162 errors/15 min).
4. **Cert lifecycle**: even with Cloudflare in front, fix `tls_cert_path`/`tls_mode` or move origin certs to Cloudflare Origin CA so renewal is not a manual ritual.
5. **Backup completeness** for a down node; verify restores (`pmcluster stack show` prints "Last backup").
6. **Credential hygiene**: rotate and move plaintext creds (S3, Parseable, SSO secret) into swarm secrets.
7. **Host maintenance**: 19 pending updates/node, ~10 security; no swap; single-vCPU workers.

---

## 12. Immediate action list (owner: Tarek / automated by Hermes)

| Priority | Action | Effort |
|---|---|---|
| P1 | Arm the swarm watchdog cron (Pending tasks, replica mismatch, OOM, reconcile errors → this chat) | 30 min, no infra |
| P1 | Confirm the IONOS LB identity/settings in the DCD; export the IONOS DNS zone | 30 min |
| P1 | Create the Cloudflare account/zone, paste the record list (§10.8), stand by for the NS change | 1 h |
| P2 | Enable `storage_failover` **or** move two or three pins off nxt-sw-1-m (move data first) | 2 h |
| P2 | Fix `tls_cert_path` / move origin certs to CF Origin CA | 1 h |
| P2 | Raise Parseable memory limit + add swap on nxt-sw-1-m | 1 h |
| P3 | Fix apex/www + Cloudflare "Always Use HTTPS" | 30 min |
| P3 | Replace oauth2-proxy with Cloudflare Access | 1–2 h |
| P3 | Prune stale otel configs + `zo_*` secrets; move creds to swarm secrets | 1 h |
| P4 | Apply security updates (19/node) in a maintenance window | 1–2 h |
| P4 | Fix app OTLP endpoints; add node metrics | 2 h |

---

## Appendix A — Access & credentials (no secret values here)

- **SSH**: `ssh root@217.160.143.22|.24|.26` and `root@217.160.128.29` from deshli (key `tarek@deshli` authorized on all four). Node→node hops need agent forwarding (the nodes hold no private key).
- **pmcluster CLI**: `/usr/local/bin/pmcluster` (v0.2.187) on nxt-sw-1-m/2-m; API `:9090` restricted to docker networks (`172.16.0.0/12`); bearer tokens via `pmcluster user`/`--api-token`.
- **State**: `/root/.pmcluster/data.db` (SQLite; contains stacks, revisions, settings, and **plaintext** secrets), `/root/.pmcluster/bootstrap-token.txt`.
- **TLS material**: `/root/migration/certs/nextrum-sy.com.{crt,key}` and `idlebbookfair.com.{crt,key}` on nxt-sw-1-m; also stored as swarm secrets (`cert_ab64977f`, `key_a6411269`, `hostcert-idlebbookfair-com_*`).
- **Backups**: IONOS S3 (`eu-central-3`, bucket `pmclusterbackup`) with credentials in `cluster_settings`; seaweedfs holds the store on nxt-sw-3-w.
- **Observability**: Parseable admin (`admin@nextrum-sy.com`) in the service env + collector config; UI behind SSO.

## Appendix B — Key commands

```bash
# node/service state
ssh root@217.160.143.24 'docker node ls; docker service ls'
ssh root@217.160.143.24 "for s in \$(docker service ls --format '{{.Name}}'); do docker service ps \$s --filter desired-state=running --no-trunc --format '{{.Node}} {{.CurrentState}} {{.Error}}'; done"
# pmcluster
ssh root@217.160.143.24 '/usr/local/bin/pmcluster cluster status; /usr/local/bin/pmcluster stack list'
ssh root@217.160.143.24 'journalctl -u pmcluster --since "30 min ago" | grep -E "ERR|failover"'
# ingress / front checks (from deshli)
dig +short abbas.nextrum-sy.com; curl -sI https://abbas.nextrum-sy.com/
echo | openssl s_client -connect 195.20.228.29:443 -servername nextrum-sy.com 2>/dev/null | openssl x509 -noout -subject -dates
# data / backups
ssh root@217.160.143.22 'ls -la /var/stack/data; du -sh /var/stack/*'
ssh root@217.160.143.24 '/usr/local/bin/pmcluster stack show abbas-cms | sed -n "/Last backup/p"'
```

## Appendix C — Unknowns / to confirm (IONOS DCD or vendor)

1. The LB's product type, backend pool, algorithm, stickiness, monitor config, and who owns the VIP/DNS record.
2. IONOS cloud-firewall rules per VM (what actually blocks 2377/7946/8333/4318 externally).
3. Whether `mail`/`imap`/`smtp` were created by an IONOS template (they point at the web VIP with no mail ports open) and what the intended mail client endpoint is.
4. Registrar/ownership and renewal dates for `nextrum-sy.com` and `idlebbookfair.com`; RTO/RPO expectations per client site.
5. Which of the "stale pre-pin" data directories are safe to delete (nodes 2/4).

## Appendix D — Why the outage was invisible for 6.5 h (root-cause summary)

pmcluster's `stack_status` reported **all 14 stacks "healthy"** and `pmcluster cluster status` reported a healthy cluster (2/3 managers, quorum intact) while 15 tasks sat in `Pending`. The IONOS LB kept ticking its TCP health checks (nodes were alive), the alerting path did not exist, and nobody was watching `docker service ls`. Everything needed to catch it in minutes is available — it just is not wired to notify anyone. That is P1 in §12 regardless of what Cloudflare ends up doing.

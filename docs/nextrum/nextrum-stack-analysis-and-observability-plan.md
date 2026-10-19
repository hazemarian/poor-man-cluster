# Nextrum × poor-man-stack — Full Stack Analysis & Observability / Dashboard Plan

**Compiled:** 2026-10-16, ~15:00–17:00 UTC
**Sources:** repo `github.com/hazemarian/poor-man-stack` (working checkout at `/home/tarek/projects/nextrum/pmcluster`, current HEAD `9da35a8`; the original live analysis used historical commit `aa0bc31`), live SSH inspection of all 4 nodes, live Parseable queries over its SQL API.
**Companion doc:** `nextrum-platform-state-and-cloudflare-proposal.md` (infra audit + Cloudflare plan).
**Decisions taken (Tarek):** dashboard = native page in the pmcluster console · alerting = external watchdog *and* in-cluster · scope = analysis + plan, with the bounded log-attribution repair now implemented in the working checkout and awaiting live Docker acceptance.

---

## 0. TL;DR

1. **The stack is well-engineered** — leader-only control loop, content-addressed secrets/configs, self-healing platform reconcile, DSL→Compose pipeline, embedded console. The observability *pipeline* plumbing (retention, dashboard provisioning, bounded queues) is converged as of v0.2.196.
2. **But container log collection is broken in a silent way**: only the pmcluster daemon + 4 OTel-SDK-instrumented apps land in `pmcluster-logs`. Every other service's stdout/stderr is read, parsed and **dropped** — the label-attribution path was removed in `ada9c7c`, and the v0.2.192 "drop unattributable" filter then hid the loss. Verified live: 24 h log rows = **10,279** across **5 service names**, vs **546 K metric rows/hour**.
3. **No host-level metrics** (`hostmetrics` receiver absent) and **no alerting of any kind** — the two gaps that made the 6.5 h outage possible.
4. **Metrics volume dominates** (≈13 M rows/day from `docker_stats` @10 s) and is the memory pressure that OOM-killed Parseable at 773 MiB today 09:46 (memcg cap 768 MiB, no swap).
5. The plan: **fix log attribution → add hostmetrics → cut metric volume (~4×) → swap + resize → wire alerting (external Hermes watchdog + in-cluster notifier) → build the console Observability page** (uPlot client-side, Parseable queried server-side by the daemon). ~4 phases, mostly free, ordered by risk.

---

> **Snapshot note:** the live-state sections below describe the 2026-10-16
> deployment before the pmcluster log-attribution repair. The collector topology
> and row counts are historical evidence, not a claim about the post-fix state.

## 1. How everything works (the whole system, end to end)

### 1.1 The control plane — `pmcluster` (Go, one static binary)

- **Layout:** `cmd/pmcluster` (CLI) → `internal/cli` → `internal/remote` (local|remote adapters) → domain packages; `cmd/edge` is the *second* binary = console + reverse proxy. Daemon runs as `pmcluster serve` (systemd on nxt-sw-2-m), REST/webhook API on `:9090`, state in SQLite (`~/.pmcluster/data.db`, CGO-free `modernc.org/sqlite`, `MaxOpenConns(1)`, journal DELETE — deliberate no-WAL choice to avoid split-brain).
- **Control loop** (`internal/reconcile/reconcile.go`, leader-only, 60 s tick + Docker events with 2 s debounce, single-flight TryLock): three passes per run —
  (a) platform stack reconcile = re-render templates, hash-compare, redeploy on drift (`internal/cluster/update.go`, named-step workflow);
  (b) app-stack drift sync per stack with a **storage-node-outage pause** and an **opt-in storage failover** (`storage_failover` setting; moves data via rclone transit, 5-min cooldown, records `stack_failover` marker);
  (c) health snapshot written to `stack_status` — the table that feeds the public SVG badges.
- **Content-aware, self-healing:** every config/secret is content-addressed (`<name>_<sha8>`); missing swarm objects are **re-materialized from the DB index** on every `cluster update`; rendered hash stamped only after successful apply (drift detection compares stored + live labels).
- **Secrets:** AES-256-GCM at rest (`~/.pmcluster/.encryption_key`), scopes cluster/service, rotation keeps old version mounted until convergence. Control-plane survivability: state tarball + key published as two Raft-replicated Docker configs (split on purpose), restore at startup only.
- **Stacks/webhooks/TLS:** DSL YAML → interpolate → validate → IR → Compose v3.9 → `docker stack deploy` level-by-level (Kahn topsort on `depends_on`), wait-healthy per level; revisions + rollback from stored source; HMAC-SHA256 inbound webhooks (CI deploy triggers) with delivery history; per-host certs + cluster site cert as content-addressed swarm secrets wired into Traefik's default TLS block.
- **Platform stacks** (created by `cluster up/update`, not tracked in the stacks table):
  `infra` = Traefik v3.6.5 (global on managers, 256 M cap, swarm + file provider, OTLP traces/metrics out);
  `observability` = Parseable v2.8.0 (1 replica pinned to platform node, 768 M cap) + OTel collector (global, 320 M cap, host-published 4318 for the daemon's own telemetry) + `provision` run_once (retention + dashboard via API);
  `backup` = offen volume-backup (global on storage nodes, hourly, SeaweedFS WebDAV + offsite S3) + SeaweedFS on a *non-*-storage node + control-plane-backup (daily, manager-only);
  `edge` = pmcluster-edge console/proxy (pinned platform node, 256 M);
  `sso` = oauth2-proxy (GitHub org, forward-auth for `pmcluster.` and `observ.`).

### 1.2 The DSL (what app teams write)

`pkg/dsl`: strict YAML (unknown fields rejected) — `app/env/domain/version/services[]` with `image, replicas, run_once, mode:global, placement, volumes, secrets, ports, expose, configs, resources, healthcheck, depends_on, logging…` plus a reference language: `secret(x)` (value), `secrets(x)` (mount path), `config(x)`, `config_path(x)`, `settings(x)`. The translator injects the private overlay network, external secret declarations, volume binds under `/var/stack/data/<app>/<vol>`, standard labels (`io.pmcluster.*` incl. `rendered_hash`, `skip_filelog`), Traefik router labels, `$`-escaping. **Platform stacks use the same pipeline** after a Go-template pre-pass — one code path, two consumers.

### 1.3 The observability pipeline (as deployed today)

```
containers stdout/stderr ── filelog (docker json) ─┐
apps SDKs ────── OTLP :4317/:4318 ─────────────────┼─ otel-collector (global ×4)
docker_stats (10 s, labels→service.name) ──────────┤   memory_limiter 320/80 → resourcedetection → resource
traefik (OTLP metrics+traces) ─────────────────────┘   → filter/skip_filelog → batch(1000/10s)
                                                       → otlphttp → Parseable :8000/v1/{logs,metrics,traces}
                                                          (X-P-Stream + X-P-Log-Source, encoding: json,
                                                           queue 128, retry ≤60 s, Basic auth header)
Parseable v2.8.0 (Parquet, local-store) ── retention convergence (run_once curl job: 30 d logs / 14 d metrics / 10 d traces — verified applied live)
                                       └─ dashboard "pmcluster" (6 builder tiles — provisions at 14:05 today)
                                       └─ SPA behind oauth2-proxy + /sso-bridge (seeds localStorage authToken with the *admin* credential)
```

Console = `pmcluster-edge` (gin + embedded htmx templates, EN/AR, RBAC viewer/operator/admin). It shows stacks/services/tasks/logs(live tail)/backups/current certs — **nothing from Parseable** beyond a link.

---

## 2. Live verified state (2026-10-16, cluster + Parseable)

| Check | Result |
|---|---|
| Swarm services | 25 services; all good (2 are normal one-shots: `donation-campaign-stg_migration`, `observability_provision`) |
| Pending/Failed tasks | none |
| nxt-sw-1-m memory | 2.0 / 3.8 GiB used, **0 swap**; OOM killer fired 09:46 UTC on `parseable` at **773 MiB anon RSS** (memcg cap 768 MiB) |
| Parseable now | 152 MiB / 768 MiB cap, 0.04 % CPU; streams: `pmcluster-logs`, `pmcluster-metrics`, `pmcluster-traces`, `pstats` |
| Collector now | 105 MiB / 320 MiB cap (~2 % CPU per node) |
| Retention | `30d / 14d / 10d` — applied and verified via API |
| Dashboard | "pmcluster", id `01M52GH57EEE8EJB8SEDFZ75RM`, created today 14:05 |
| **Log rows, last 24 h** | **10,279 total**; by service: `pmcluster` 7,684 · `horizon-erp` 650 · `dg-steps-erp` 649 · `donation-campaign` 648 · `shash-erp` 648 — **nothing else** |
| Null-service logs | 0 (filter working — by deleting the world) |
| Severities | INFO 10,272 / WARN 4 / ERROR 3 |
| **Metric rows, last 1 h** | **546,465** (≈13 M/day) |
| Metric services | all of them (labels work on `docker_stats`) |

Sample of the surviving logs: the 4 app services ship via their **own OTel SDKs** (`telemetry.sdk.*`, `scope_name`, structured fields like `path=/health`) — i.e. the only logs that arrive are the ones that never went through `filelog` at all.

---

## 3. Findings register

| # | Sev | Finding | Evidence | Impact |
|---|---|---|---|---|
| 1 | **Critical** | **Container stdout/stderr logs are silently dropped.** `filelog` reads Docker json logs but nothing attaches `container.labels.*` → `resource["service.name"]` is null on every record → `filter/skip_filelog` (v0.2.192) deletes them. The old `docker_observer`+`receiver_creator` attribution was removed in `ada9c7c`, leaving the filter to mask the regression. | Live query §2; config: `otel-collector-config.yml` filelog has only `json_parser`+`move` operators; filter drops `service.name == nil` | 20 of 25 services have **zero** searchable history. The 6.5 h outage logs (Traefik 404s, orchestrator errors) would not have been searchable either. Dashboards' "Log volume by service" is 4 stray apps + the daemon. |
| 2 | **High** | **No host-level metrics.** No `hostmetrics` receiver anywhere. | grep of rendered config | Node CPU/mem/disk/swap invisible — exactly the failure mode of the outage (host memory pressure → heartbeat loss). Parseable's 764/768 MiB near-miss today was found by hand. |
| 3 | **High** | **No alerting path, in-cluster or out.** Zero outbound notification code; Parseable alert targets unconfigured. | grep `telegram/slack/smtp/notify`; `Failed to load targets` in Parseable logs | Outages are discovered by a human. Root cause of the 6.5 h exposure. |
| 4 | **High** | **Metric volume dominates ingest** (~13 M rows/day vs ~10 K log rows). `docker_stats` @10 s × ~40 containers × dozens of series. | Live counts §2 | This is what pushes Parseable into its 768 MiB cap; OOM'd today at 09:46. Also wastes node CPU/disk on 1-vCPU nodes. |
| 5 | **Medium** | **Parseable OOM'd today** (773 MiB anon RSS vs 768 MiB cap, no swap on the node). The `memory_limiter` in the collector shed load; Parseable itself has no ingest limiter. | `dmesg` 09:46:30; `docker stats` | Observability dies exactly when it is needed. |
| 6 | **Medium** | `filelog` `exclude:` globs are **inert** (they glob on container *names* but the paths contain container *IDs*) — including the exclusions for traefik/backup/collector themselves. Dead config that lies. | `exclude:` block vs actual path shape `/var/lib/docker/containers/<id>/…` | The enumerated `filter` services list is doing the real work; exclusions give false confidence. Also: filelog parses everything (CPU) only to drop ~all of it. |
| 7 | **Medium** | **Parser-per-parseable credential** — the admin Basic header is embedded as base64 in the *rendered Docker config* and in the Parseable service env (no read-only user model). Anyone with `docker config/secret inspect` on any node has it. | rendered `pmcluster_otel_config_*`; Parseable service env | Credential spread across 4 nodes × 2 config versions. **Rotate** (it also passed through this session's terminal output) and move to env-ref (`${env:…}`) from a mounted swarm secret. |
| 8 | **Medium** | `provision` is **fire-once**: if Parseable isn't answering within its 300 s wait, retention + dashboard are skipped until the next observability-stack redeploy. No periodic re-convergence. | `observability-provision.sh:36-47` + run_once semantics | Silent config drift on every Parseable slow boot. |
| 9 | **Low** | Traces pipeline has no `resource`/`filter` processors (inconsistent with the "drop unattributable" policy); effectively only daemon + Traefik send traces. App OTLP endpoints broken on abbas/abbas-cms (162 errors/15 min). | rendered config; audit §8 | Trace data is thin; noise risk if apps start sending SDK traces. |
| 10 | **Low** | Docs drift (`README` still says OpenObserve), stale `zo_*` secrets, duplicate otel config versions (`pmcluster_otel_config_*` ×2, `otel-collector-config_*` ×5) from the migration era. | `docker config ls`; repo README | Confuses operators; tiny storage cost; wrong-config-picked risk on redeploy is bounded by content-addressing. |
| 11 | **Low** | Console has **zero** observability surfaces (no node metrics, no error counts, no charting); the only bridge is the External link into Parseable's SPA with a second login. | `internal/ui/*` | Operators hop between two tools; the light-est fix is a native page (§4.3). |

---

## 4. The plan

### 4.1 Phase 1 — Repair the pipeline (the data has to be true)

**1a. Log attribution — use the pinned `docker_observer` + `receiver_creator` path.**
The checked-in collector image is `otel/opentelemetry-collector-contrib:0.157.0`.
That distribution has the beta Docker observer and the alpha receiver creator;
the standalone `docker` log receiver/schema was not assumed without a pinned
component check. The observer watches the local Docker API, emits one portless
endpoint per running container, and the receiver creator expands a filelog
receiver with the container ID plus Swarm labels as resource attributes. This
restores the service attribution the static container-ID glob lost.
Details: keep `skip_filelog` label opt-out honored, keep the enumerated
platform-noise drop list, keep `start_at: end`, and keep bounded queue +
`memory_limiter`. The stdout/filelog and SDK/OTLP logs use separate pipelines so
the stdout filter cannot drop structured SDK records. A live Docker daemon test
is still required before deployment.

**1b. Validate the existing `hostmetrics` receiver** before expanding it. The current checkout already has the contrib receiver configured for `cpu`, `memory`, and `load`; the live acceptance check must confirm its host `/proc` and `/sys` mounts and `resourcedetection(system)` produce honest `host.name` values. Add `disk`, `filesystem`, or `network` only as a follow-up bounded change after the log repair is accepted — no new service or cAdvisor is needed.

**1c. Cut metric volume ~4×** (do this *with* 1b, before resizing anything):
- `docker_stats.interval`: 10 s → 30 s (config knob `metrics_interval` surfaced as a cluster setting).
- Allowlist transform in `transform/metrics`: keep `container.cpu.utilization`, `container.memory.percent`, `container.memory.usage.total`, `container.network.io.usage.tx/rx_bytes`, `container.pids.count`, `container.restarts`, `container.filesystem.usage`; drop the rest (blockio detail, per-interface duplicates, etc.).
- Effect: ~13 M → ~3 M rows/day, which buys back Parseable headroom *and* gives space for the recovered logs.

**1d. Swap + sizing (operational, no repo change):**
- 2 GiB swapfile + `vm.swappiness=10`, `vm.vfs_cache_pressure=50` on all 4 nodes (also add to `harden-host.sh` in-repo so new nodes inherit it) — turns a memcg OOM kill into slow-but-alive.
- Parseable cap 768 M → **1 GiB** (it genuinely hit 773 M; 24 h peak deserves headroom), reservation 128 → 256 M; revisit after 1c lands (expect steady ~200–350 M).
- Collector: current 320 M cap is fine for hostmetrics (adds ~5–10 M); re-measure after 1a (docker receiver ≈ filelog cost, minus the wasted parse-then-drop).

**1e. Credential hygiene:** rotate `observability_admin_password`; switch the collector config to `Authorization: Basic ${env:PARSEABLE_BASIC_AUTH}` fed from a **swarm secret → service env** (no more base64 in an inspectable config; rotation touches only the secret + collector redeploy). Remove the same credential from the Parseable service env in favor of `P_USERNAME`/`P_PASSWORD` via secret mount (it already supports secret refs — verify v2.8 argv/env pair). Prune the duplicate `pmcluster_otel_config_*` / `otel-collector-config_*` / `zo_*` objects after convergence.

**1f. Provision convergence:** make retention + dashboard reconcile on every observability-stack render (or a daemon-side "ensure" that hits `/api/v1/logstream/{s}/retention` + dashboard upsert each pass, hash-guarded). Cheapest robust version: keep the run_once script but make the *daemon's* periodic platform reconcile re-run it when Parseable's `/api/v1/about` was unreachable the last time (state flag in settings).

### 4.2 Phase 2 — Alerting (both layers, as decided)

**2a. External watchdog (build first — survives cluster failure).** Hermes cron on deshli, every 5 min, single Python script with a JSON state file for dedupe/cooldown, Telegram bot output. Checks:
- SSH (node 2 as control point, fallback any node): `docker node ls` (Down/Unknown), `docker service ls` replica mismatch (**excluding expected one-shots**: `*_migration`, `observability_provision`, `observability_retention`), any `Pending|Rejected|Failed` task, `dmesg -T` OOM lines since last run, `free` (warn < 300 MiB available), `df` (> 85 %), collector task count == 4, Parseable task `Running`.
- HTTPS probes: `pmcluster.`, `observ.`, 3 client hosts (one per node-2 resident app + pinned app), status+latency+TTFB; TLS expiry via openssl (`*.nextrum-sy.com` expires **2026-12-05** → alert at ≤21 d).
- Data side: last backup age per node (via pmcluster CLI or S3 listing), Parseable reachable, retention present (API), rows arriving (query count last 15 min > 0 for logs+metrics).
- Output discipline: alert on **state transitions** (ok→bad, bad→ok recovery), not every tick; one digest/day heartbeat.
*Needed from you: Telegram bot token + chat id (or point it at an existing Hermes bot config).*

**2b. In-cluster notifier (repo feature, phase 2 after 2a proves the signal list).** New `internal/notify` domain wired to seams the daemon *already* computes:
- reconcile pause/failover events (`storage node down`, `failover completed`, `pending tasks`),
- backup results (`backup.last_unix` stale, offsite-missing),
- container OOM events (the daemon already consumes the Docker events stream — `oom` is a native event),
- cert expiry warning hook (`warnSiteCertExpiry`),
- delivery: configurable outbound webhook (generic JSON + Telegram-shaped preset), retry-bounded, no new container — **the daemon is the notifier**. This keeps "light": one new package + settings, no sidecar.
Secondary (optional third layer): Parseable alert rules → same webhook for log-derived conditions once logs are attributable again.

### 4.3 Phase 3 — The console Observability page (the dashboard)

**Principles:** one login (console RBAC), server-side queries (the daemon holds the credential and sits on `monitoring-net`), client renders with a vendored ~45 KiB **uPlot** (same philosophy as the vendored htmx), every tile reads from the cheapest correct source, and *every tile shows `—` when data is unknown rather than 0* (the console's existing zero-vs-unknown discipline).

**New daemon endpoints** (Bearer-auth, viewer-readable, served from the leader; Parseable queries via its SQL API with one shared client + 15 s cache):
- `GET /api/observability/summary` → health strip: nodes up/down, services at desired, pending tasks, OOM events 24 h (from hostmetrics `system.uptime` jumps + docker `oom` counter), cert days-left (site_certs), backup freshness (backups table), ingestion health (rows/min per stream from Parseable).
- `GET /api/observability/nodes?range=1h|6h|24h` → per-node series: CPU util, mem used %, swap used, disk used %, load1 (from `hostmetrics`).
- `GET /api/observability/logs?range=…` → log volume + error counts grouped by `service.name` (from `pmcluster-logs`).
- `GET /api/observability/ingest?range=…` → rows/min per stream + exporter queue/failed counters (enable collector **self-telemetry** → `pmcluster-metrics` with `service.name=observability_otel-collector`).

**Page layout (`/web/observability`, i18n EN/AR, auto-refresh toggle like the services page):**
1. **Health strip** — status pills: Nodes 4/4 · Services 23/23 (+2 one-shot) · Pending 0 · OOM 24 h 1 · Cert 50 d · Backup 19 h · Ingest ✓ (each click-through to its detail page).
2. **Node resource grid** — 4 small-multiple uPlot charts (CPU %, Mem %, Disk %, Swap) with a shared 1 h/6 h/24 h range selector.
3. **Logs** — stacked/line: volume/min by service (top 6 + other); bar: errors by service 24 h.
4. **Ingestion** — rows/min logs/metrics/traces; annotation marks for collector restarts (from `process.start_time`).
5. **Backups & certs** — freshness bars per node + countdown cards (renders from SQLite; no Parseable needed).
6. Deep-dive links: service row → "open in Parseable" prefilled search (and, once 1a lands, a console-side "last 200 error lines" card that queries Parseable directly instead of the live-only tail).

**After the page exists:** shrink the provisioned Parseable dashboard to 2–3 "deep-dive" tiles (numbers/table), since the console becomes the front door.

### 4.4 Phase 4 — polish & de-drift

- Fix or delete the inert `filelog` excludes (done as part of 1a).
- README/docs: replace OpenObserve naming; document the receiver topology + settings knobs (`metrics_interval`, retention keys in the settings form).
- App OTLP endpoints: fix or drop `OTEL_EXPORTER_OTLP_ENDPOINT` on abbas/abbas-cms; add `resource`+`filter` processors to the traces pipeline for symmetry.
- Consider `otlphttp` exporter `compression: gzip` (Parseable accepts gzip; lowers CPU on 1-vCPU nodes).
- Optional later: node_exporter-grade detail (processes, conntrack) only if hostmetrics proves insufficient.

---

## 5. Sequenced roadmap

| Phase | Work | Effort | Risk | Repo touch |
|---|---|---|---|---|
| **1a** | Docker observer + receiver_creator attributes per-container filelog (+ keep opt-out + noise drop; stdout and OTLP pipelines split) | 2–3 h | Low (one stack redeploy; live daemon check required) | `embeds/otel-collector-config.yml`, `observability-stack.yml` mounts |
| **1b** | hostmetrics receiver + host mounts | 1–2 h | Low | same files |
| **1c** | metrics allowlist + 30 s interval + setting | 2 h | Low | config + settings plumbing |
| **1d** | swap/sysctl on 4 nodes; Parseable cap 1 GiB | 1 h | None | `harden-host.sh` (repo) + ops |
| **1e** | rotate Parseable admin; env-ref auth; prune duplicates | 1 h | Low (brief Parseable restart) | config/templates |
| **1f** | provision re-convergence | 1–2 h | Low | provision script + reconcile hook |
| **2a** | Hermes watchdog → Telegram | 2–3 h | None (external) | Hermes scripts (deshli) |
| **2b** | `internal/notify` daemon feature | 1–2 d | Medium | new package + settings + wiring |
| **3** | Console Observability page + daemon query client + uPlot | 2–3 d | Medium | daemon endpoints + UI page (+ i18n) |
| **4** | docs, traces symmetry, gzip, app endpoints | 1 d | Low | assorted |

Ordering rationale: 1a–1c make the *data* correct and cheap; 2a restores *noticing* within days with zero cluster risk; 2b/3 are the durable product work; 4 keeps the repo honest.

---

## 6. What I need from you

1. **Telegram bot token + chat id** for the watchdog (or say "use Hermes' existing bot" and I'll read its config).
2. **Collector path:** the pinned 0.157 observer + receiver_creator path is the current implementation. Validate it against a real Docker daemon before any production rollout; revisit a standalone receiver only if a later pinned distribution proves its schema and behavior.
3. **Upstream workflow:** the repo is `hazemarian/poor-man-stack` — do I (a) prepare PRs against a fork of it, (b) prepare patches for you to forward, or (c) keep everything as deploy-time config until Hazem weighs in?
4. **Maintenance window** for 1d/1e (Parseable restart + swap activation): any 30-min slot where an observability blip is acceptable (client sites are unaffected by both).
5. **Confirm retention values** 30/14/10 are the intended long-run numbers (repo defaults are 7 d; live cluster is 30/14/10) — they'll be written into settings + docs as part of 1f.

---

## Appendix A — Evidence commands (reproducible)

```bash
# one-time: extract the collector's Basic auth from the rendered config (node 1)
ssh root@217.160.143.22 'docker config inspect pmcluster_otel_config_1792143932 > /tmp/cfg.json'
ssh root@217.160.143.22 'python3 -c "
import json, base64
d = json.load(open(\"/tmp/cfg.json\")); spec = d[0][\"Spec\"] if isinstance(d, list) else d[\"Spec\"]
open(\"/tmp/otelcfg.yml\",\"w\").write(base64.b64decode(spec[\"Data\"]).decode())"'

# attribution probe: log rows by service, last 24 h
# (SQL notes: stream names need backticks, intervals are INTERVAL '24 hours',
#  startTime/endTime must be ISO-8601 strings)
ssh root@217.160.143.22 'AUTH=$(grep "Authorization:" /tmp/otelcfg.yml | head -1 | sed "s/.*Authorization: *//;s/\"//g");
docker run --rm --network monitoring-net -e AUTH="$AUTH" curlimages/curl:8.10.1 sh -c '"'"'
curl -s -X POST -H "Authorization: $AUTH" -H "Content-Type: application/json" \
  --data "{\"query\":\"SELECT \`service.name\` AS svc, COUNT(*) AS n FROM \`pmcluster-logs\` WHERE p_timestamp > NOW() - INTERVAL '"'"'24 hours'"'"' GROUP BY svc ORDER BY n DESC LIMIT 20\",\"startTime\":\"'$(date -u -d "25 hours ago" +%Y-%m-%dT%H:%M:%SZ)'\",\"endTime\":\"'$(date -u +%Y-%m-%dT%H:%M:%SZ)'\"}" \
  http://observability_parseable:8000/api/v1/query'"'"''

# metric volume, last hour
#   SELECT COUNT(*) AS n FROM `pmcluster-metrics` WHERE p_timestamp > NOW() - INTERVAL '1 hour'
# OOM evidence
ssh root@217.160.143.22 'dmesg -T | grep -i -E "oom_kill|Memory cgroup out of memory" | tail'
# dashboard + retention (inside the swarm)
docker run --rm --network monitoring-net -e AUTH="$AUTH" curlimages/curl:8.10.1 sh -c \
  'curl -s -H "Authorization: $AUTH" http://observability_parseable:8000/api/v1/dashboards; \
   curl -s -H "Authorization: $AUTH" http://observability_parseable:8000/api/v1/logstream/pmcluster-logs/retention'
```

## Appendix B — Exact config drift list (cleanup targets)

- Docker configs to prune after 1a/1e convergence: `pmcluster_otel_config_89e35194` (+ `_1792143932`), `otel-collector-config_*` (5 versions), `backup-stack_*`/`edge-stack_*`/`observability-stack_*`/`sso-stack_*`/`traefik-dynamic_*` historical versions where GC hasn't caught up.
- Orphaned `zo_root_user_password*` secrets (OpenObserve era) — confirmed still present in the audit; nowhere referenced.
- Retention settings keys currently live: `observability_{logs,metrics,traces}_retention_days` = 30/14/10 (set); repo defaults 7 d — align defaults to decision in §6.5.

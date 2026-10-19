# Nextrum handover

## Current state — 2026-10-19

Deshli's public edge now runs `nextrum-observability-20261019-4`. The main
`/web/observability` page defaults to all three hosts across Overview, Logs,
Metrics and Traces, with a persistent node selector and host-scoped filters,
pagination, polling and trace detail. `/web/fleet` is a compatibility alias.
The public edge and all three standalone collector configurations were updated;
daemons and private edges retain their prior releases. The filtered Metrics
empty-node bug and Docker metric resource-label normalization are fixed. See
[the follow-up verification](verification/observability-filter-2026-10-19/README.md). Fleet-mode investigation routes are disabled. See
[the unified UI verification](verification/observability-2026-10-19/README.md).

The three standalone observers and the private console binaries retain
`nextrum-fleet-20261019-2`. Deshli's existing authenticated console defaults to
the combined Observability dashboard. Each server retains local collection and
storage; persistent restricted SSH links pair Crestline/Damarkets inventory
and telemetry with the Deshli presentation process. The 62 original application
containers retain their original IDs/start times. No Swarm migration or paid AI
calls. AI remains disabled on these servers.

Active source: this `poor-man-stack` checkout, branch
`nextrum/three-node-observability`, based on `f047df6` plus the preserved
working-tree observability changes.
Latest build/test artifacts are on Crestline under
`~/nextrum-build-observability-filter-20261019`; the prior unified UI artifacts
remain at `~/nextrum-build-observability-20261019`, and earlier fleet artifacts at
`~/nextrum-build-fleet-20261019`. The latest vet/race checks and public four-tab,
node/filter, mobile RTL and connection failure/recovery checks passed; the
verification report records the initial template failure and corrected reruns.
The earlier fleet full race suite passed 44 packages. Independent source
and operational review, all-host live checks, collector restart recovery,
synthetic traces, fleet link failure/recovery and desktop/mobile RTL checks
passed. No server reboot was performed.

Post-recreate history was separately recovered after finding a Parseable v2.8.0
local-store hostname/index bug: each backend was fully archived, its affected
day manifests were rebuilt from Parquet metadata, and Compose was pinned to the
original hostname (`ff9de8f45b39` Crestline, `46fe8a5ace7f` Deshli,
`28227f3ad801` Damarkets). Final read-only dry-runs on all three hosts report
no partitions requiring repair; independent checks report one canonical
manifest per day with zero orphan/missing/duplicate Parquet references. The
recovery archives and null-aware stats audit are recorded in the verification
report. Crestline's Parseable credential was rotated after recovery: an old
auth value had appeared in earlier tool stderr, so it was replaced; the old
credential returned 403 and the new one 200, with no secret values included in
this handover.

Read [the fleet runbook](../../deploy/fleet/README.md) and
[the verification report](verification/fleet-2026-10-19/README.md) first.
Historical details below describe the older deployment and are not current
branch/path/version instructions.

## Historical handover — 2026-10-17

## Current objective and authorization

User requested deploying BOTH pmcluster and monitoring to tarek@deshli,
deploy@crestline and tarek@damarkets. All are standalone Docker/Compose, not
Swarm. User chose additive monitoring, no migration. Deployment is now live.
Existing apps and damarkets' prior OTel/Prometheus/Loki stack are preserved.
User subsequently requested building on Crestline instead of this laptop:
continue builds/tests there. AI remains disabled; no provider key or paid calls.

## Workspace and implementation

Project /home/tarek/projects/nextrum; nested independent Git checkout pmcluster/,
branch nextrum/observability-foundation; module pmcluster/pmcluster. Changes are
uncommitted. Root repo ignores nested checkout. Preserve parked obsai/ prototype.
Read [BACKLOG.md](BACKLOG.md), [the standalone runbook](../../deploy/standalone/README.md)
and the [native observability guide](../../pmcluster/docs/native-observability.md).
Native Go UI has bounded/redacted Parseable evidence, EN/AR, async compatible
provider investigations, validated citations, SQLite budgets, no model execution.

Explicit pmcluster observe entrypoint opens dedicated store/token, real Docker
container inventory and read-only server; never calls normal serve/reconcile/
repair/Swarm initialization. Admin workload mutations including websocket and
storage are denied; accounting policy writes remain supported. Compose groups
replicas/stopped predecessors, desired counts unknown, identity collisions fail
closed. Tasks/log-tail adapter unsupported; logs through Parseable evidence.

## Live deployment

Each host: ~/nextrum-monitoring, Compose project nextrum-monitoring. Services
parseable, collector, daemon, edge. Only loopback18042 published. SSH tunnels:
Crestline localhost18142, Deshli18242, Damarkets18342 currently running locally.
Each opens /web/observability; no public hostname configured. Console login is
disabled behind private loopback/SSH access; don't expose without auth.
.env0600 in private0700 directory stores generated perhost secrets; never print.
New named volumes preserve backend history, collector offsets and daemon state.
One-day retention applied to correctly typed logs/metrics/traces streams.
User cron every5min pauses only new collector when backend/deployment filesystems
fall below10GiB free; not a hard disk quota. Rollback operations in deployment
README remove only new cron/project; preserve volumes (no down -v).

All three live acceptance checks passed: stdout marker with correct service,
host/container metrics, native UI evidence, unauthorized reads and admin workload
mutations blocked, Swarm stays inactive. All62 original running app containers
remain running (deshli41, crestline6, damarkets15). Temporary probe containers
have been removed. Perhost verification.txt records live acceptance output.
Desktop/mobile/Arabic live browser checks pass with no overflow or JS errors.

## Builds/checks and pending finish

Crestline build root ~/nextrum-build/repository (complete upstream repo), module
repository/pmcluster. Caches are named Docker volumes nextrum-gomod/nextrum-gobuild;
release binaries and SHA256SUMS under ~/nextrum-build/release. Deployment helper
build-on-crestline.sh records the command. Source archiving must include parent
README/agent.md/docs tree: docscheck requires full repo layout. First module-only
remote check was stopped and replaced with proper full repo layout.

Final Crestline source builds, go vet and full race suite PASSED (session86219,
exit0; release/tests-passed present). Full source tree /repository was used.
Logs/manifests saved locally in `docs/nextrum/verification/crestline-*`. Earlier local
full suite passed; final typed UTC timestamp correction also passed focused race
on Crestline. Independent source review is clean; live checks were primary-run.

Live bugs fixed: generic pre-created metrics/traces streams rejected OTLP;
verification now supplies X-P-Telemetry-Type and X-P-Log-Source. Only wrong-type
new EMPTY streams were recreated (asserted no firstEventAt). Parseable UTC
p_timestamp lacksZ; typed timestamp redaction now preserves it, with regression.

Final source artifacts are deployed and hashes match on ALL three hosts:
- pmcluster:055b7d9d9d540760cd66639d818c61ce9b2122e7fe7afe3c0fba69ee3e9047f9
- edge:929e9ea6223d83d5f54e75f52d6ba6e3a85c9dcd61ec4e967bc5ca48a4ecee6e
Source-built with Go1.25.14 on Crestline. Public origin settings match current
localhost tunnel ports18142/18242/18342 (CONSOLE_ORIGIN in perhost.env); changing
tunnel ports requires updating that origin and recreating only edge.
Final smoke: inventory/auth/readonly/native UI/typed retention pass, original
62 apps still running, probes removed, one guard cron entry perhost. Numeric
CPU and container-memory values confirmed nonnull in each backend. Remote check
container exited and removed automatically. Old synthetic laptop preview,
fixture and isolated Chromium have been stopped; three SSH tunnels remain.
No required deployment work remains. See [the standalone runbook](../../deploy/standalone/README.md)
for rollback.

Global workflow: bounded executor → primary integration → independent reviewer,
one writer. Primary currently owns writes after executor adapter work handed
back/interrupted. No agent currently implementing. Avoid extra scope; don't
configure paid AI without actual provider credentials and explicit budget setup.

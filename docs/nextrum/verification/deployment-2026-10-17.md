# Deployment verification — 2026-10-17

Authorized additive deployment of pmcluster and monitoring to three standalone
Docker hosts. No Swarm migration or app deployment changes.

| Host | Existing running containers preserved | Local SSH tunnel |
|---|---:|---:|
| deshli (tarek) |41|18242|
| crestline (deploy) |6|18142|
| damarkets (tarek) |15|18342|

All bind only127.0.0.1:18042 on the server. The three tunnels run on this laptop;
open http://localhost:PORT/web/observability. No public route was configured.

Each has four services in nextrum-monitoring: daemon in explicit observe mode,
existing native edge console, OTel0.157.0 and Parseable2.8.0. Existing damarkets
monitoring is preserved. Credentials are generated independently and remain in
private host-local .env files. AI disabled; no provider key or paid request.

Live acceptance passed all three: real stdout marker with service identity,
host and container metrics, marker visible through native UI, unauthorized reads
rejected, admin workload mutations denied, Swarm inactive. Probe containers were
removed after checks. Typed logs/metrics/traces have one-day retention. A user
cron checks actual backend filesystems every5min and pauses only the new
collector below10GiB free (periodic safeguard, not a hard quota).

Desktop1440px, mobile390px and Arabic RTL browser checks on live Crestline pass
without overflow, unresolved keys or JS errors. A live redaction issue with
Parseable UTC timestamps lackingZ was fixed and regression-tested on Crestline.

Canonical source builds now run on Crestline with Go1.25.14 in golang:1.25,
persistent module/compiler caches and CPU/memory limits. Full repo layout under
~/nextrum-build/repository is required by documentation tests. Build script is
available locally at deploy/standalone/build-on-crestline.sh and remotely at
~/nextrum-build/build-on-crestline.sh. No further laptop builds are needed.

All three deployed binary hashes match the final Crestline source build:

- pmcluster: `055b7d9d9d540760cd66639d818c61ce9b2122e7fe7afe3c0fba69ee3e9047f9`
- edge: `929e9ea6223d83d5f54e75f52d6ba6e3a85c9dcd61ec4e967bc5ca48a4ecee6e`

Source builds, go vet, full race suite on Crestline, focused races, independent
source review and live checks all passed. Exit0 confirmed; full log and release
manifest are saved beside this report. CPU/container-memory metric values were
also checked nonnull in all three backends.
The earlier local full race suite passed. Initial module-only remote check was
stopped to restore complete repository layout; it is not a passing test run.

Metrics are stored in Parseable; native CPU/memory charts remain future work.
No SDK instrumentation was added to apps. Standalone mode does not manage their
deployments, infer desired replicas, or supply Swarm/managed backup/TLS state.

Rollback: remove only the new user cron entry using install-disk-guard.py
--remove, then docker compose down in ~/nextrum-monitoring. Preserve named
volumes; do not use down -v. Existing apps are outside this Compose project.

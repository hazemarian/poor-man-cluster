# Nextrum three-node observability workbench

This directory is the operating record for the Nextrum observability work that
now lives in the `poor-man-stack` checkout. The live system presents one
authenticated dashboard for three independent Docker/Compose servers:

| Host | Role in the view | Local monitoring placement |
| --- | --- | --- |
| Deshli | Public presentation edge and one telemetry source | User-systemd observer and edge; Compose collector and Parseable |
| Crestline | Telemetry source and canonical build/test host | Compose observer, edge, collector and Parseable |
| Damarkets | Telemetry source; existing monitoring is retained | Compose observer, edge, collector and Parseable |

Open the combined dashboard at
[`https://pmcluster.deshli.site/web/observability`](https://pmcluster.deshli.site/web/observability).
The Overview, Logs, Metrics and Traces tabs default to all three hosts. The
node selector, filters, pagination and trace details preserve host identity.
`/web/fleet`, `/web/` and `/web/overview` remain compatibility entry points.

The three servers are federated for presentation and read-only telemetry. They
are not being converted into a Swarm workload cluster: each host keeps its own
Docker applications, observer state, collector offsets and Parseable data.
AI investigations are disabled, and the monitoring deployment does not restart
or mutate existing application workloads.

## Start here

- [Fleet operations and rollback](../../deploy/fleet/README.md)
- [Standalone installation and health checks](../../deploy/standalone/README.md)
- [Latest filtered Metrics and collector verification](verification/observability-filter-2026-10-19/README.md)
- [Unified four-tab UI verification](verification/observability-2026-10-19/README.md)
- [Initial three-host deployment verification](verification/fleet-2026-10-19/README.md)
- [Native observer implementation guide](../../pmcluster/docs/native-observability.md)
- [Current UI preview](previews/observability-overview.png) and [mobile RTL preview](previews/observability-mobile-ar.png)
- [Current handover](HANDOVER.md)
- [Build status and remaining work](BACKLOG.md)

The branch containing this consolidation is local and intentionally unpushed:
`nextrum/three-node-observability`. It is based on the latest fetched
`origin/main` ancestor (`e91f3e4`) plus the ten existing Nextrum commits and
the working-tree observability changes. Deployment files and verification
reports are included here so a fresh checkout has the implementation context,
but no server credentials, runtime environment files or host-local state are
part of the repository.

## Architecture

Each server runs the same read-only monitoring shape. Docker container logs,
host resources and Docker stats are collected locally by the OpenTelemetry
contrib collector (`0.157.0`) and written to that host's Parseable backend
(`2.8.0`). The observer exposes authorized node/service inventory and the edge
renders the native UI. Deshli's edge constructs separate, immutable clients for
each configured host and merges bounded results while retaining the originating
host on every section.

```text
Per host:
  Docker Engine ───────────────► pmcluster observer ───────┐
  Docker/apps ─► OTel collector ─► Parseable backend ───────┼─► Deshli edge
                                                            │   (paired,
                                                            │    loopback SSH)
                                                            └─► /web/observability
```

The observer path supplies authorized inventory and service identity. The
collector path supplies bounded telemetry. The edge queries both paths through
paired endpoints and keeps their host identity attached while rendering the
combined page.

The remote links use restricted, loopback-bound SSH forwarding from Deshli to
Crestline and Damarkets. Deshli's fleet configuration pairs each host's daemon
endpoint, backend endpoint and credential. It is created and maintained on the
server at `~/nextrum-fleet/hosts.json`, mode `0600`; it must never be copied
into Git.

The Parseable container hostname is pinned per host because Parseable `2.8.0`
uses its process hostname in local-store manifest paths. Keep these values
unchanged across backend recreation:

| Host | Pinned backend hostname |
| --- | --- |
| Deshli | `46fe8a5ace7f` |
| Crestline | `ff9de8f45b39` |
| Damarkets | `28227f3ad801` |

## Tool inventory and node placement

The following is the complete installed shape for the current standalone
deployment. Version numbers are the deployed versions, not floating upgrade
requests.

| Tool | Deshli | Crestline | Damarkets | Purpose |
| --- | --- | --- | --- | --- |
| Docker Engine and Compose | required | required | required | Runs the monitoring services alongside existing apps |
| `pmcluster` observer | user-systemd | Compose | Compose | Read-only Docker inventory and service API on port `9090` |
| `pmcluster` edge | user-systemd, public HTTPS | Compose, loopback | Compose, loopback | Native console; Deshli presents the combined view |
| OTel contrib collector `0.157.0` | Compose | Compose | Compose | Host, container, log and OTLP collection |
| Parseable `2.8.0` | Compose, loopback `18080` | Compose, loopback `18080` | Compose, loopback `18080` | Local telemetry storage and bounded SQL queries |
| OpenTelemetry app SDK/exporter | only for instrumented apps | only for instrumented apps | only for instrumented apps | Optional application traces, metrics or logs |
| systemd user units | fleet links and public edge | optional local service management | optional local service management | Restart policy and Deshli federation links |
| Go `1.25` build container and persistent caches | no | build/test authority | no | Reproducible Linux/amd64 pmcluster and edge builds |
| Python `3.12+` | health/operations scripts | health/operations scripts | health/operations scripts | Safe verification and recovery tooling |

The default Compose project is `nextrum-monitoring`. On Crestline and
Damarkets, the observer listens on loopback `19090`, Parseable on loopback
`18080`, and the private edge on loopback `18042`; use an SSH tunnel for the
console. Deshli's host-run observer listens on `9090`, its Compose Parseable
backend on loopback `18080`, and its authenticated public edge serves HTTPS
through the existing user-service setup. The collector's OTLP ports are not
published by default.
`compose.otlp.yaml` is an explicit opt-in override for applications that must
send OTLP to a host-local loopback endpoint.

The parked `obsai/` prototype is not a live dependency. It is a bounded,
read-only evidence service with no model client, credentials, mutation endpoint
or deployment manifest. Keep it separate until its evidence contract is
exercised against live data.

## Repository layout

```text
deploy/standalone/       Per-host Compose files, collector config, checks,
                         disk guard and Crestline build helper.
deploy/fleet/            Three-host federation runbook and Parseable recovery.
deploy/public-deshli/    Deshli public edge verification/integration helpers.
obsai/                   Parked evidence-only Python prototype.
docs/nextrum/            This guide, handover, backlog, design notes and
                         dated live verification artifacts.
pmcluster/               The Go application and native observability UI.
```

The upstream project documentation remains under `docs/`; Nextrum deployment
material is deliberately under `docs/nextrum/` so it cannot overwrite the
upstream document map.

## Build and test workflow

Crestline is the build and test authority. Sync this complete repository to
`~/nextrum-build/repository` on Crestline and run:

```sh
cd deploy/standalone
./build-on-crestline.sh
```

The helper builds static Linux/amd64 `pmcluster` and `edge` binaries in a
Docker Go `1.25` container, uses the named `nextrum-gomod` and
`nextrum-gobuild` caches, writes checksums, runs `go vet`, and runs the full
race suite. The authoritative live build and browser evidence remain in the
dated verification reports; no laptop Go build is required for this branch.

For a private host, copy the standalone deployment directory and release
artifacts to `~/nextrum-monitoring`, generate the host-local `.env` once with
`python3 init-env.py`, build the standalone image, then start the Compose
project. Existing applications must remain outside this project. Never
shell-source `.env`: its values include credentials and an authentication value
with a space.

The regular read-only check is:

```sh
cd ~/nextrum-monitoring
python3 check-health.py
```

After a backend recovery, request a historical check explicitly:

```sh
python3 check-health.py --history-minutes 15 \
  --history-stream metrics --history-stream logs
```

The acceptance checker `verify.py` is for a controlled first install or
re-verification. It creates no permanent application workload; any temporary
probe must be removed after the check. The scripts print statuses and bounded
counts, never credentials, SQL rows or application log bodies.

## Safe operations

On Deshli, the two federation units and public edge use the user service bus:

```sh
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
systemctl --user status nextrum-fleet-crestline nextrum-fleet-damarkets
systemctl --user restart nextrum-fleet-crestline nextrum-fleet-damarkets
systemctl --user restart pmcluster-edge
```

Each server also has a five-minute disk guard. When the backend filesystem has
less than 10 GiB free, it pauses only this deployment's collector. Inspect the
filesystem and resume with `docker compose start collector` after the cause is
understood. Retention limits history age; it is not a disk quota.

If a federation link fails, the combined page reports that host as unavailable
while its local collector and backend continue. Restore the link unit after
repairing connectivity. This is a presentation federation, not replicated
telemetry storage or an HA edge.

## Rollback and data protection

The standard rollback removes or recreates only the `nextrum-monitoring`
services. Preserve named volumes and never run `docker compose down -v`:

```sh
cd ~/nextrum-monitoring
python3 install-disk-guard.py --remove
docker compose down
```

For the current public UI release, Deshli keeps the startup/environment backup
at `~/nextrum-rollbacks/20261019-observability4/`. The prior standalone
collector configuration is at
`~/nextrum-rollbacks/20261019-metric-resource-labels/otel.yaml` on each host.
Restore the appropriate file and recreate only the collector or public edge as
described in the fleet runbook. Do not restart Parseable as part of a UI-only
rollback.

If Parseable is recreated, preserve the pinned hostname before it starts. If a
hostname mismatch already produced duplicate local-store manifests, stop only
the affected collector and Parseable, make a complete local backup, and use
the dry-run-first `deploy/fleet/repair-parseable-history.py` procedure. The
repair is conservative and fails closed when Parquet statistics are ambiguous.
The recovery archives described in the dated fleet report are local rollback
inputs, not an off-host backup policy.

## Verification record and limits

The latest fix addressed two independent Metrics failures: an irrelevant
service selector could hide node resource cards, and resource-level Docker
labels were not normalized into the common `service.name` field. The exact
reported URL now returns all three hosts, resource values and container rows
without a partial-coverage banner. The evidence includes a red/green Go
regression, collector fixture against the real `0.157.0` image, candidate
config validation, live rollout receipts, browser checks and the unchanged
62-container application baseline. See
[the dated follow-up report](verification/observability-filter-2026-10-19/README.md).

The system still has deliberate boundaries:

- application-specific trace instrumentation is opt-in;
- separate backends do not become one distributed trace waterfall;
- external alert delivery and periodic off-host telemetry backups are not
  configured by this deployment;
- historical rows written before service normalization may lack
  `service.name`;
- Swarm-specific scheduling and live Swarm validation remain separate work;
- AI/provider setup remains disabled.

These constraints are part of the operating contract. The dated reports record
what was tested, where it ran, and which state was preserved. Do not infer
cluster scheduling, replicated storage or application instrumentation from the
combined dashboard alone.

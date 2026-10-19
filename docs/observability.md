# Standalone observability operations

This guide describes the additive monitoring deployment used by the current
example installation on Deshli, Crestline, and Damarkets. It is a read-only
observability layer for independent Docker/Compose hosts. It does not turn the
hosts into a scheduling cluster, move application workloads, initialize Swarm,
or replicate telemetry between backends.

The combined authenticated console is available at
[`https://pmcluster.deshli.site/web/observability`](https://pmcluster.deshli.site/web/observability).
The Overview, Logs, Metrics, and Traces tabs default to all three example
hosts. `/web/fleet`, `/web/`, and `/web/overview` are compatibility entry
points. AI investigations are disabled in this deployment.

The public edge currently uses release `nextrum-observability-20261019-4`
(SHA-256
`40e70aed22190b69bcd9973fc84f49284512b83cb643d6ebe51e012291350223`). The
private observer and edge processes use the `nextrum-fleet-20261019-2` release.
The standalone collector configuration deployed to all three hosts has SHA-256
`73dfeb6a9e5ba8bc932f3c1e052d2091bfd21f1de8fec6ae023506818b56f84f`.

## Architecture

Each host collects and stores its own telemetry. The presentation edge on the
public host creates a separate authenticated client for each configured host,
queries them within one UTC window, and keeps the originating host on every
result. A source that is unavailable remains visibly unavailable; an empty
successful query remains empty.

```text
Each Docker host
  Docker Engine ───────────────► pmcluster observe (inventory, read-only)
  containers and host ─► OTel Collector ─► local Parseable (logs/metrics/traces)
                                                   │
                                                   └─► paired endpoint
                                                        on the Deshli edge
                                                              │
                                                              ▼
                                                   /web/observability
```

The two remote hosts are reached through restricted, loopback-bound SSH
forwards maintained by Deshli user services. The forwards carry only the
monitoring daemon and Parseable endpoints. They are a presentation link, not
telemetry replication or an HA control plane. Deshli's local console settings
do not redirect the other hosts' clients.

## Current example topology

The host names below describe the current example deployment. A different
installation can use the same layout with different names and addresses.

| Host | Monitoring placement | Private endpoints | Presentation role |
| --- | --- | --- | --- |
| Deshli | User-systemd observer and edge; Compose collector and Parseable | Observer `127.0.0.1:9090`, Parseable `127.0.0.1:18080` | Public authenticated edge and federation point |
| Crestline | Compose observer, edge, collector, and Parseable; build/test host | Observer `127.0.0.1:19090`, edge `127.0.0.1:18042`, Parseable `127.0.0.1:18080` | Telemetry source and build authority |
| Damarkets | Compose observer, edge, collector, and Parseable; existing monitoring retained | Observer `127.0.0.1:19090`, edge `127.0.0.1:18042`, Parseable `127.0.0.1:18080` | Telemetry source |

Deshli's federation forwards use `19190/19180` for Crestline's observer and
Parseable endpoints and `19290/19280` for Damarkets. They bind to loopback on
Deshli. The private edge on Crestline and Damarkets is reached through SSH when
an operator needs a host-local console. Only Deshli's existing authenticated
edge is public.

Parseable local-store manifests depend on the process hostname. Preserve these
values whenever the backend is recreated:

| Host | Stable Parseable hostname |
| --- | --- |
| Deshli | `46fe8a5ace7f` |
| Crestline | `ff9de8f45b39` |
| Damarkets | `28227f3ad801` |

Changing a pinned hostname can create a second manifest for the same day and
make historical rows appear to disappear. The recovery procedure below is the
safe path when that has already happened.

## Tools and node placement

| Tool | Install on | Role |
| --- | --- | --- |
| Docker Engine and Compose | Every monitoring host | Runs the monitoring services beside existing applications |
| `pmcluster` observer | Every host | Read-only Docker inventory and authorized service identity |
| `pmcluster` edge | Every host; public on the presentation host | Native console and host-local telemetry API client |
| OpenTelemetry contrib collector `0.157.0` | Every host | Collects Docker logs/stats, host metrics, and optional OTLP data |
| Parseable `2.8.0` | Every host | Local logs, metrics, and traces store |
| Python `3.12+` | Every host | Health, disk-guard, and recovery scripts |
| systemd user units | Presentation host | Keeps federation forwards and the public edge running |
| Go `1.25` build container and named caches | Crestline only | Reproducible Linux/amd64 builds and tests |
| OpenTelemetry application SDK/exporter | Only instrumented applications | Optional application traces, logs, or custom metrics |

The Compose project is named `nextrum-monitoring`. The default stack publishes
only the private edge on `127.0.0.1:18042`. The collector listens for OTLP on
4317 (gRPC) and 4318 (HTTP) inside its Compose network but publishes neither
port by default. The optional
[`compose.otlp.yaml`](../deploy/standalone/compose.otlp.yaml) override publishes
those ports on loopback for applications that need host-local OTLP access.

The collector reads Docker's read-only socket and JSON-file logs, host `/proc`
and `/sys`, and its persistent offset volume. Parseable, collector, and daemon
state use named volumes. Existing applications and any pre-existing monitoring
stack remain outside this Compose project.

## Configuration contract

Run [`init-env.py`](../deploy/standalone/init-env.py) once inside the private
deployment directory. It creates a mode-0600 `.env` containing these host-local
values:

```text
HOST_NAME=<this host's stable name>
PARSEABLE_HOSTNAME=<the original Parseable manifest hostname>
PARSEABLE_PASSWORD=<generated password>
PARSEABLE_AUTH=Basic <base64 of monitoring credentials>
DAEMON_TOKEN=<generated observer token>
```

The Compose edge consumes the same token and Parseable authorization through
`PMCLUSTER_API_TOKEN` and `OBSERVABILITY_PARSEABLE_AUTH`. Its other important
values are `UPSTREAM=http://daemon:9090`,
`OBSERVABILITY_PARSEABLE_URL=http://parseable:8000`,
`PMCLUSTER_OBSERVER_MODE=true`, `EDGE_LOGIN_DISABLED=true`, and
`PMCLUSTER_PUBLIC_ORIGIN`. Keep the public origin aligned with the URL used by
the browser so same-origin protection works for investigation endpoints if
they are enabled later.

Never shell-source `.env`: `PARSEABLE_AUTH` contains a space and the file
contains credentials. Let Compose parse it or use a parser that keeps values in
memory. Do not print it, put it in Git, or pass secrets in a command-line
argument.

Deshli's edge reads the fleet file named by `OBSERVABILITY_FLEET_CONFIG`. The
live file is `/home/tarek/nextrum-fleet/hosts.json`, mode `0600`, and is kept
on the server. Its shape is an array of paired host records; the values below
are placeholders and contain no credentials:

```json
[
  {
    "name": "deshli",
    "hostname": "deshli",
    "daemon_url": "http://127.0.0.1:9090",
    "daemon_token": "<host-local observer token>",
    "parseable_url": "http://127.0.0.1:18080",
    "parseable_auth": "Basic <host-local authorization>"
  },
  {
    "name": "crestline",
    "hostname": "crestline",
    "daemon_url": "http://127.0.0.1:19190",
    "daemon_token": "<host-local observer token>",
    "parseable_url": "http://127.0.0.1:19180",
    "parseable_auth": "Basic <host-local authorization>"
  },
  {
    "name": "damarkets",
    "hostname": "damarkets",
    "daemon_url": "http://127.0.0.1:19290",
    "daemon_token": "<host-local observer token>",
    "parseable_url": "http://127.0.0.1:19280",
    "parseable_auth": "Basic <host-local authorization>"
  }
]
```

The `daemon_url`, `daemon_token`, `parseable_url`, and `parseable_auth`
fields are paired. Do not point a daemon client at another host's backend or
reuse a credential across records. The dedicated federation key is restricted
to the two monitoring destinations, cannot execute commands, and is separate
from the operator's SSH key.

## Installation and build workflow

Build and test on Crestline. The laptop does not need a Go installation or a
local Go build. Synchronize the complete repository to
`~/nextrum-build/repository` on Crestline and run:

```sh
cd ~/nextrum-build/repository/deploy/standalone
./build-on-crestline.sh
```

The helper uses a Go `1.25` container, `nextrum-gomod` and
`nextrum-gobuild` named caches, static Linux/amd64 builds, SHA-256 checksums,
`go vet`, and the full race suite. Copy the resulting `pmcluster` and
`edge` binaries beside
[`Dockerfile`](../deploy/standalone/Dockerfile), then install the standalone
directory on each target as `~/nextrum-monitoring` with private permissions.

Install the endpoint override before starting Compose. Crestline and Damarkets
need [`compose.endpoints.yaml`](../deploy/fleet/compose.endpoints.yaml) as
`compose.override.yaml`; it publishes only the daemon on loopback
`127.0.0.1:19090` and Parseable on `127.0.0.1:18080` for the restricted
Deshli forwards. Deshli uses
[`compose.public-deshli.yaml`](../deploy/standalone/compose.public-deshli.yaml)
as `compose.override.yaml`; it publishes only its local Parseable backend and
leaves the authenticated host-run observer and edge in place. The base file's
private edge remains on `127.0.0.1:18042` on Compose hosts. These overrides are
not public listeners.

On a new host:

```sh
cd ~/nextrum-monitoring
python3 init-env.py
docker build -t nextrum-pmcluster:standalone .
docker compose up -d
python3 install-disk-guard.py
```

For a new private Compose host, initialize the three one-day retention
policies with the acceptance script after the collector has started. The
script expects a temporary labelled probe; it does not create one itself.
The probe repeats its marker so the collector can discover it after starting
at the end of its JSON log file:

```sh
set -eu
probe_name=nextrum-check-probe
cleanup_probe() { docker rm -f "$probe_name" >/dev/null 2>&1 || true; }
trap cleanup_probe EXIT INT TERM
docker run -d --log-driver=json-file --name "$probe_name" \
  --label com.docker.compose.project=nextrum-check \
  --label com.docker.compose.service=probe \
  busybox:1.36 sh -c 'while :; do echo NEXTRUM_LIVE_STDOUT_PROBE; sleep 5; done' >/dev/null
python3 verify.py
python3 check-health.py
```

`verify.py` pre-creates any missing telemetry streams, sets one-day retention
on logs, metrics, and traces, checks read-only authorization and fresh host and
container data, and confirms the probe in the native page. The trap removes the
temporary container even when verification fails. Run this private-console
acceptance recipe on Compose hosts where `127.0.0.1:18042` is available; the
Deshli public variant uses the existing authenticated host-run edge and should
be checked with `check-health.py` after its platform provisioning has applied
retention.

For a host that sends application telemetry through loopback, use the explicit
override:

```sh
docker compose -f compose.yaml -f compose.override.yaml -f compose.otlp.yaml up -d
```

The [`check-health.py`](../deploy/standalone/check-health.py) command is the
regular read-only check. It validates observer access, host inventory, one-day
retention, fresh host/container metrics, and the observability tabs without
printing credentials, SQL rows, or application log bodies. After a recovery,
request persisted history explicitly:

```sh
python3 check-health.py --history-minutes 15 \
  --history-stream metrics --history-stream logs
```

The one-time [`verify.py`](../deploy/standalone/verify.py) acceptance recipe
above exercises authorization and mutation denial; use it before relying on a
new private host.

## Federation operations

The Deshli user service bus must be available for status and restart commands:

```sh
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
systemctl --user status nextrum-fleet-crestline nextrum-fleet-damarkets
systemctl --user restart nextrum-fleet-crestline nextrum-fleet-damarkets
systemctl --user restart pmcluster-edge
```

The tunnel units use keepalives, restart automatically, bind their listeners to
loopback, and pin SSH host keys. If one link fails, the combined page marks
only that host unavailable while local collection continues. Check the unit,
the loopback listener, and the paired endpoint in `hosts.json` before changing
credentials. If Deshli is unavailable, connect directly to a private host
through SSH and use its host-local console.

The five-minute disk guard pauses only this deployment's collector when the
backend filesystem has less than 10 GiB free. Inspect the filesystem first,
then resume collection with:

```sh
docker compose start collector
```

The guard is a safety threshold, not a disk quota. Retention limits age, and
Docker log rotation limits local JSON-file growth.

On a private Compose host, reach the host-local console through SSH:

```sh
ssh -N -L 18042:127.0.0.1:18042 <user>@<private-host>
```

Then open `http://localhost:18042/web/observability`. Private standalone edges
use `EDGE_LOGIN_DISABLED=true`, so SSH is the access gate. Do not publish this
port or place it behind a reverse proxy without adding authentication and
setting `PMCLUSTER_PUBLIC_ORIGIN` to the authenticated HTTPS origin.

## Using the combined console

The four tabs share one selected UTC window and default to all configured hosts.
Use the node selector to scope a view to one host; host identity stays attached
to services, logs, metric cards, and traces.

- **Overview** shows daemon health, one resource card per host, and service
  inventory grouped by stack.
- **Logs** shows bounded newest-first pages with service, severity, text, and
  time filters. The live tail rereads the newest page every five seconds.
- **Metrics** shows CPU, memory, and load per host plus average and peak CPU
  and memory for the highest-peaking containers.
- **Traces** lists recent traces per host and opens a bounded span waterfall.
  Traces from separate backends remain separate; the page does not invent a
  cross-host distributed trace.

Service selectors are opaque values resolved against the selected host's
authorized inventory. A service value copied from one host must not be assumed
valid on another. Filters are allowlisted and length-bounded; the browser
cannot provide SQL or an arbitrary backend endpoint. Logs are redacted and
HTML-escaped before rendering. Partial coverage and source failures remain
explicit, while a successful empty query is shown as empty.

Container stderr is classified as `ERROR` and stdout as `INFO` unless the
application supplies a recognized level through OTLP. This means a stored
severity badge can reflect the output stream even when the message body says
`level=info`; message classification and ingestion normalization are separate
concerns.

## Collector identity contract

The Parseable metric queries use the common `service.name` identity. The
standalone [`otel.yaml`](../deploy/standalone/otel.yaml) keeps this identity
stable across collector and application sources:

1. An explicit datapoint-level `service.name` remains authoritative.
2. If it is absent, an explicit resource-level `service.name` is copied to the
   datapoint.
3. If both are absent, resource `stack.name` and `compose.service` are joined
   into the Compose service identity.
4. A legacy datapoint-level `stack.name` plus `compose.service` is accepted
   for older or SDK-specific paths.
5. Unlabelled host metrics do not receive an invented service name.

The Docker stats receiver maps `com.docker.compose.project` to `stack.name`
and `com.docker.compose.service` to `compose.service`. In collector
`0.157.0` those configured labels are resource attributes, which is why the
resource fallback is required. File logs receive the same Compose-derived
service identity and container metadata. Historical metrics written before this
rule was deployed may still lack `service.name`; fresh container metrics use
it.

Applications that need custom logs, metrics, or traces must send OTLP to the
host-local collector and set their own service identity. Docker stdout/stderr
collection does not instrument an application automatically.

## Retention, recovery, and rollback

The current example uses one-day retention for logs, metrics, and traces. Read
the policies back with `check-health.py` after a deployment or recovery. The
collector has bounded memory and export queues; a prolonged backend outage can
lose records after those buffers fill. The local recovery archives described
here are rollback inputs, not an off-host telemetry backup policy.

### Preserve Parseable history

Before recreating Parseable, set `PARSEABLE_HOSTNAME` to the stable value from
the first day manifest. Preserve named volumes and never use
`docker compose down -v`.

If a new hostname has already produced duplicate local-store manifests:

1. Stop only this deployment's collector and Parseable; leave applications
   and unrelated monitoring services running.
2. Make complete mode-0600 backups of the data and staging volumes.
3. Run [`repair-parseable-history.py`](../deploy/fleet/repair-parseable-history.py)
   in its temporary Python environment in dry-run mode first.
4. Apply only after the affected partitions and Parquet metadata are reviewed;
   the tool's apply mode requires `--offline` and keeps old files for rollback.
5. Restore the pinned hostname, start Parseable and the collector, then run the
   normal and explicit historical health checks.

The repair fails closed when non-null Parquet statistics are missing or
conflicting. An all-null row group is accepted only when Parquet reports
`null_count == num_rows`; do not clear statistics globally or infer a range.

### Roll back a monitoring change

The current public edge startup and environment backup is on Deshli at
`~/nextrum-rollbacks/20261019-observability4/`. The previous standalone
collector configuration is at
`~/nextrum-rollbacks/20261019-metric-resource-labels/otel.yaml` on each host.
Restore the relevant files and recreate only the collector for a collector
change, or restart only `pmcluster-edge.service` for a public UI change. Do
not restart Parseable, remove named volumes, or delete telemetry for a
UI/collector rollback.

For a full standalone removal, first remove the disk guard and then bring down
only this Compose project:

```sh
python3 install-disk-guard.py --remove
docker compose down
```

The command intentionally preserves named volumes. Replacing a backend binary
or configuration is not a substitute for a data recovery procedure.

## Optional AI capabilities

AI is currently disabled. Fleet investigation routes are disabled, no provider
key is installed, no provider calls have been made, and the monitoring
deployment does not execute actions. The combined dashboard remains useful
without a provider.

The Go console contains an optional, bounded provider capability for a future
explicit enablement. That path requires a provider-issued secret, an exact
provider base URL and model policy, an administrator's consent to share
redacted evidence, and a normal daemon/edge release. It sends redacted and
bounded evidence, validates citations, accounts usage conservatively, and
cannot restart or deploy workloads. Provider setup is separate from this
standalone deployment and must not be inferred from the dashboard.

## Verification and limits

The authoritative build/test machine is Crestline. The current deployment was
verified with the following checks:

- the affected UI packages passed `go vet` and race testing across all seven
  UI packages;
- the filtered service regression reproduced the old empty-node render and
  passed after the controller fix;
- the real collector `0.157.0` fixture covered explicit point labels, explicit
  resource labels, derived Compose labels, legacy datapoint labels, and
  unlabelled metrics;
- candidate collector configurations validated with the deployed image on all
  three hosts;
- the exact reported Metrics URL was checked twice with all three hosts online,
  resource values, and container rows;
- authenticated browser checks covered all four tabs, host filters, filter
  retention, mobile RTL layout, the compatibility alias, and page errors;
- the 62 pre-existing application containers retained their IDs and start
  times.

The broader unified release also passed the full source vet/race campaign on
Crestline. No local laptop Go build or server reboot is required for normal
source verification. The build helper is the repeatable way to rerun it.

Known boundaries remain:

- this is a presentation federation of independent stores, not a workload
  scheduler, replicated database, or HA public edge;
- application tracing is opt-in and existing tracing destinations are kept;
- external alert delivery and scheduled off-host telemetry backups are not
  configured here;
- old metric rows may lack the normalized service identity;
- collector queues are bounded during backend outages;
- severity normalization at ingestion is separate work;
- Swarm-specific scheduling and live Swarm validation are outside this
  standalone deployment;
- AI/provider enablement remains explicitly off.

The implementation is in `pmcluster/internal/observability`, its Parseable
adapter, the native UI controllers/templates, and the standalone deployment
files under [`deploy/standalone`](../deploy/standalone/). The native
composition and dependency boundaries are documented in
[`pmcluster/internal/ARCHITECTURE.md`](../pmcluster/internal/ARCHITECTURE.md).

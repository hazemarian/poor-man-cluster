# Observability

pmcluster ships a built-in observability stack. `cluster up` deploys it with the
rest of the platform, and the operator console exposes it at
`/web/observability`. There is no separate monitoring installation to run.

The stack answers four questions from one place:

- what is running, and is it healthy?
- what did it log?
- how much CPU, memory and load is each node and container using?
- what traces did instrumented applications emit?

## Architecture

```text
On each Swarm node:
  Docker logs, Docker stats, host metrics ─┐
  Application and daemon OTLP ────────────┴─► OTel collector
                                                  │
                                                  ▼
One per cluster:                               Parseable
                                                  │ bounded queries
                                                  ▼
  pmcluster daemon ── inventory and auth ─────► edge console
```
- **One collector per node.** It is a `mode: global` service, because container
  logs and container metrics are node-local and come from that node's Docker
  engine. A central collector could not read them.
- **One Parseable backend per cluster.** It stores logs, metrics and traces on a
  mounted volume and serves the SQL queries used by the console. It is pinned to
  the platform node.
- **The daemon and edge supply the console.** The daemon supplies
  cluster inventory and authorization; the edge renders the console and queries
  Parseable. The edge never needs the Docker socket.
- **The provisioning job runs once.** It applies per-stream retention and the
  platform dashboard through Parseable's API, then exits.

## What is collected

| Signal | Source | Notes |
| --- | --- | --- |
| Container logs | Docker `json-file` logs | Collected when the global collector can resolve the container identity; opt-out services, unknown identities and selected platform chatter are filtered. stdout/stderr provide the default severity |
| Container metrics | Docker stats API | CPU, memory, network and block I/O, labelled with stack and service |
| Node metrics | Host `/proc` and `/sys` mounted read-only | CPU utilization, memory utilization and load average |
| Traces | OTLP from instrumented applications | Empty until an application sends spans |
| Platform telemetry | pmcluster daemon and Traefik | Deploys, reconciles, backups and proxy spans |

Application traces and custom metrics are **opt-in**. Set
`OTEL_EXPORTER_OTLP_ENDPOINT` to the collector service and instrument the
application with an OpenTelemetry SDK. Docker log collection alone does not
produce spans.

Containers started by an optional Compose workload can be collected by the same
global collector when Docker exposes both Compose project and service labels.
They use the `<project>_<service>` identity; containers without a complete
identity are filtered. This uses the built-in collector and does not add a
second observability installation.

The `observability` stack is deployed by the normal lifecycle:

```sh
pmcluster cluster up       # first install
pmcluster cluster update   # apply configuration changes
```

## Console layout

`/web/observability` has four tabs. Each tab loads only its own data, and each
runs the same authorization probe and inventory resolution before contacting
Parseable.

### Overview

- node and service health from the daemon
- one card per node with CPU, memory and load
- service groups ordered with unhealthy stacks first
- log volume by severity and the most repeated error messages
- trace totals when traces exist

### Logs

- newest first, 50 rows per page
- filters for service, time range, severity and text
- bounded offset paging
- expandable multi-line records
- optional live tail, refreshed every five seconds

Log bodies are redacted and HTML-escaped before rendering.

### Metrics

- per-node CPU, memory and load charts for the selected window
- per-container average and peak CPU and memory
- gaps mean no data was collected; they are never drawn as zero

### Traces

- trace summaries newest first, with a service filter and an errors-only toggle
- span waterfall for a selected trace, ordered by parent/child depth
- error spans highlighted

Traces are capped per view. Span attributes are not displayed.

## Service identity

Logs, metrics and traces share one identity field, `service.name`:

- Swarm services use `<stack>_<service>`.
- Container metrics take the identity from the Swarm service label.
- OTLP telemetry keeps the `service.name` supplied by the application.
- Host metrics have no service identity and remain node-scoped.

The embedded collector copies a resource-level `service.name` onto metric data
points when the data point has none, and derives the Swarm identity for Docker
stats. It never overwrites an identity that the application or receiver already
set.

If an application sends its own logs through OTLP, add
`skip_filelog: true` to its manifest service to avoid collecting the same lines
twice from stdout.

## OpenTelemetry ingestion

The collector accepts OTLP on gRPC `4317` and HTTP `4318` on every node. The
monitoring network endpoint for HTTP is
`http://observability_otel-collector:4318`; gRPC uses the internal
`observability_otel-collector:4317` endpoint. The Swarm stack publishes HTTP
`4318` in host mode so the host daemon and local clients can reach it; the
published host port and the internal gRPC port are different endpoints.

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://<node>:4318
export OTEL_SERVICE_NAME=checkout
```

Port `4318` has no authentication. Firewall it to application networks, or send
telemetry over the cluster's monitoring network using the collector service
name instead of a host port.

`severityNumber` is the reliable level field for OTLP logs. If only
`severityText` is sent, Parseable stores the level as unspecified.

## Native CLI and API

The daemon and CLI expose the same bounded read surface used by the console:

```sh
pmcluster observability status
pmcluster observability logs --service checkout/web --severity error --range 1h
pmcluster observability metrics nodes --range 6h
pmcluster observability metrics services --range 1h
pmcluster observability traces --service checkout/web --errors-only
pmcluster observability trace 0123456789abcdef0123456789abcdef
```

Add `--json` to emit one lower_snake_case envelope suitable for scripts. Every
read validates the time range, service identity, severity, trace ID, page size
and metric bucket before it reaches Parseable. Empty successful results report
`status: empty`; a backend or inventory failure reports `status: unavailable`
or an inventory-specific status. Responses carry the queried window and any
truncation reasons.

Remote commands use the normal `--api-url` and `--api-token` (or their
environment variables), with daemon Bearer role and stack-scope checks. A
local command reads the stored cluster domain, ingress port and managed edge
token, then calls the authenticated native edge route through local HTTPS. The
CLI never accepts a Parseable URL, SQL statement or backend password. Parseable
credentials stay inside the edge service on the monitoring network.

## Configuration

Retention is configured as cluster settings and applied by the provisioning job:

| Setting | Default | Meaning |
| --- | --- | --- |
| `observability_logs_retention_days` | 7 | days of logs |
| `observability_metrics_retention_days` | 7 | days of metrics |
| `observability_traces_retention_days` | 7 | days of traces |

```sh
pmcluster cluster settings set observability_logs_retention_days=14
pmcluster cluster update
```

The retention job is best-effort: an unreachable backend never fails the
platform deploy. Verify the policy in Parseable after changing it.

Collector memory and CPU are bounded in the embedded stack, and the collector
config has a memory limiter below that cap. Export queues are bounded and
retried; a long backend outage can drop records.

The Parseable volume stores telemetry. Back it up like any other platform
volume; the normal cluster lifecycle manages the stack and its volume.

Parseable's local-store catalog also depends on its hostname. pmcluster persists
that identity and captures the existing hostname during upgrades; fresh installs
use `pmcluster-parseable`. Preserve the control-plane database and encryption key
alongside telemetry volumes. An upgrade stops if an installed backend's identity
cannot be recovered, rather than silently assigning a new one.

When Swarm runs over a VPN, allow room for both VPN and VXLAN headers in the
network MTU. Test large requests through published ports as well as direct
overlay traffic. For an existing ingress network that cannot be recreated during
an upgrade, the optional [ingress MSS helper](../pmcluster/contrib/systemd/pmcluster-ingress-mss.sh)
limits TCP segments inside Docker's ingress sandbox. Install it as
`/usr/local/libexec/pmcluster-ingress-mss`, install the adjacent service and timer
under `/etc/systemd/system`, and enable `pmcluster-ingress-mss.timer` on each node.
It requires `nsenter` and `nft` (plus Docker CLI and `iproute2`'s `ip`/`bridge`
for recovery mappings), covers IPv4 and IPv6, and retries after sandbox
recreation. `PMCLUSTER_INGRESS_MSS` in `/etc/default/pmcluster-ingress-mss` overrides
the conservative default of 1200 bytes. It does not change host routes or UDP.
For recovery after a control-plane migration, an operator can also supply
verified missing ingress proxy-ARP mappings in
`/etc/pmcluster/ingress-recovery-peers`: one `network-id overlay-ip mac private-vtep-ip`
row per endpoint. The helper applies a row only to that exact network ID. This
is an explicit recovery override, not a substitute for normal Swarm discovery;
remove it after rebuilding the affected ingress network. Preserve it with the
host configuration while that network remains in use. Mappings do not expire:
revalidate them after node membership or endpoint changes, even when the network
ID stays the same. Remove stale mappings from both the file and the namespace.

Docker-published ports can bypass a host's usual input firewall. Restrict private
Swarm, OTLP, backup and daemon ports in both the input and forwarding paths,
including IPv6. Forwarding rules must match the connection's original published
port because Docker may already have translated its destination. The optional
`pmcluster-private-ports.service` loads an operator-supplied
`/etc/pmcluster/private-ports.nft`; the adjacent Docker drop-in orders this guard
before Docker starts. Keep public HTTPS separate from private native ingress.
Use `systemctl reload pmcluster-private-ports.service` to apply rule changes.
Restarting this required dependency can restart Docker and interrupt workloads.

Create and validate the rule file before installing the Docker dependency. For
example, adapt the interface, exact peer addresses and ports in this minimal
IPv4/IPv6 guard (add private native ingress ports if used):

```nft
define cluster_interface = "wg0"
define peers = { 10.20.0.1, 10.20.0.2, 10.20.0.3 }
define private_ports = { 2377, 7946, 4789, 4317, 4318, 8333, 9090 }
add table inet pmcluster_private_ports
flush table inet pmcluster_private_ports
add chain inet pmcluster_private_ports input { type filter hook input priority -20; policy accept; }
add chain inet pmcluster_private_ports forward { type filter hook forward priority -20; policy accept; }
add rule inet pmcluster_private_ports input iifname "lo" return
add rule inet pmcluster_private_ports input iifname $cluster_interface ip saddr $peers return
add rule inet pmcluster_private_ports input iifname "docker_gwbridge" return
add rule inet pmcluster_private_ports input iifname "br-*" return
add rule inet pmcluster_private_ports input tcp dport $private_ports drop
add rule inet pmcluster_private_ports input udp dport { 7946, 4789 } drop
add rule inet pmcluster_private_ports forward iifname $cluster_interface ip saddr $peers return
add rule inet pmcluster_private_ports forward iifname "docker_gwbridge" return
add rule inet pmcluster_private_ports forward iifname "br-*" return
add rule inet pmcluster_private_ports forward meta l4proto tcp ct original proto-dst $private_ports drop
add rule inet pmcluster_private_ports forward meta l4proto udp ct original proto-dst { 7946, 4789 } drop
```

Run `nft -c -f /etc/pmcluster/private-ports.nft`, install the service under
`/etc/systemd/system`, then run `systemctl daemon-reload` and
`systemctl enable --now pmcluster-private-ports.service`. Verify private peer
traffic and public HTTPS before installing `docker-private-ports.conf` as
`/etc/systemd/system/docker.service.d/20-pmcluster-private-ports.conf` and running
`systemctl daemon-reload` again. Existing workloads need no Docker restart.

The local Parseable store keeps a stable backend identity with its persisted
state. During an upgrade, the lifecycle captures an existing identity before
rendering the replacement configuration; when existing volumes are reused,
keep that identity and its stored setting so the backend is not renamed and
its history remains addressable.

## Security

- The console runs behind the existing Traefik gate (`admin-auth` or SSO).
- The daemon's `/api/me` check and cluster inventory must succeed before any
  query runs. Stack-scoped tokens are denied.
- The browser supplies only allowlisted filters, a length-capped search string
  and a bounded offset. It cannot send SQL or an endpoint.
- Parseable is reachable only from the cluster networks; it publishes no host
  port.
- Telemetry is redacted before it is rendered or sent to an optional model
  provider.

Viewers can read the page. Operators can submit and cancel their own
investigations. Administrators configure the provider.

## Optional AI investigations

AI is off by default. When enabled by an administrator, the edge sends one
bounded, redacted evidence set to a configured provider and validates every
returned citation. The model only recommends actions; it cannot deploy,
restart or execute anything.

Enable it only after configuring a provider secret, an exact base URL and model,
per-call token limits, a daily budget and explicit consent. See the [OpenAPI
reference](../pmcluster/docs/openapi.yaml) for the policy fields.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Page shows no telemetry | `docker service ps observability_parseable` and `docker service ps observability_otel-collector` |
| No node metrics | collector task is running on that node and has host metrics access |
| No container logs | container uses the `json-file` driver; check `docker info` |
| Logs duplicated | application sends OTLP logs **and** stdout; set `skip_filelog: true` |
| No traces | the application is instrumented and points at the collector's OTLP endpoint |
| Charts have gaps | no samples for that interval; a gap is not a zero |
| Retention unchanged | the provisioning job needs `cluster update` and a reachable backend |

Useful commands:

```sh
pmcluster service list --platform
pmcluster service logs observability otel-collector --tail 100
pmcluster cluster status
```

## Limits

- Parseable runs as a single task with local storage. It is not replicated, and
  losing its volume loses telemetry history.
- The collector's queues are in memory. A backend outage longer than the queue
  window loses records.
- "Error rate" is the share of error log records, not failed HTTP requests.
- Traces from different applications are not joined into a distributed trace
  unless the applications propagate trace context.
- Host metrics cover CPU, memory and load. Disk, network and per-process views
  are not part of this version.
- Historical rows written before a collector identity change may not carry the
  current `service.name`.

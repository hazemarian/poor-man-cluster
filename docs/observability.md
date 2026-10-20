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

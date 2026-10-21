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
  container stdout/stderr ──► per-container filelog ─┐
  container + host metrics ──────────────────────────┤
  application and daemon OTLP ───────────────────────┴─► OTel collector (global)
                                                                │ OTLP/HTTP
                                                                ▼
One per cluster:                                            Parseable
                                                       (local Parquet store)
                                                                ▲
                                                                │ bounded SQL
                                                                │ over 127.0.0.1:8000
  CLI ──► pmcluster daemon ─────────────────────────────────────┘
            ▲           │
  console ──┘           └──► /api/observability/*   (bearer + viewer role)
```

- **One collector per node.** It is a `mode: global` service, because container
  logs and container metrics are node-local and only that node's Docker engine
  can see them. A central collector could not read them.
- **One Parseable backend per cluster.** It stores logs, metrics and traces on a
  mounted volume and answers SQL. It is pinned to the platform node.
- **The daemon owns the telemetry connection.** It alone holds the Parseable
  credential, applies redaction and bounds every query. The console and the CLI
  both read telemetry through it, so there is one auth check and one place a
  backend query can be built.
- **The console holds no backend credential.** It renders what the daemon
  returns. The edge never joins the monitoring network and never talks to
  Parseable.
- **The provisioning job runs once.** It applies per-stream retention and the
  platform dashboard through Parseable's API, then exits.

### Why Parseable publishes a host port

Parseable keeps its API on the cluster networks, but its HTTP port is published
on the Swarm routing mesh (`8000`). The daemon is a **host process**: it cannot
resolve `observability_parseable` from the overlay, so the mesh publish — which
answers on `127.0.0.1:8000` on every node — is how it reads telemetry. This is
the same mechanism the backup SeaweedFS service uses for `8333`.

Publishing on the routing mesh also binds that port on each host's public
interface. **Deny it to the WAN on every node.** `ufw` does not cover it — a
published port is DNAT'd past ufw's INPUT chain — so the block belongs in the
`DOCKER-USER` chain, which is evaluated before Docker's own accept rules.

`./harden-host.sh` (repo root) already does this: `8000` sits in
`RESTRICTED_DOCKER_PORTS` alongside `4318`, and the script RETURNs every PRIVATE
source (`127.0.0.0/8`, the overlay pools, and the cluster's private peer network)
before dropping. That distinction matters: an ingress-published port is served by
whichever node hosts the task, so a peer node's request arrives over VXLAN
carrying the *peer's* private address — not a bridge subnet and not loopback.
Allow-listing only `172.16.0.0/12` drops every cross-node request while Swarm
keeps its ESTABLISHED tunnels alive, so the cluster still reports `Ready` and the
only symptom is a port that answers on one node and hangs on the others.
Re-run it on each node after upgrading — it is idempotent. To do it by hand:

```sh
iptables -I DOCKER-USER -s 127.0.0.0/8 -j RETURN
iptables -I DOCKER-USER -p tcp --dport 8000 -j DROP
```

Verify from another host that `8000` is unreachable, and from the node itself
that `curl -sf -u <user>:<pass> http://127.0.0.1:8000/api/v1/logstream` still
answers. The first proves the WAN side is closed; the second proves you did not
cut the daemon off from its own backend.

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

## Log collection and `skip_filelog`

Two log paths are live, and they are different on purpose.

**Container stdout/stderr (default).** One `filelog` receiver is created per
container from a Docker observer, so every record carries that container's
identity: `service.name` (`<stack>_<service>`, or `<project>_<service>` for a
Compose workload), `stack.name`, `container.id` and `container.image.name`. A
container whose labels resolve to nothing is dropped rather than ingested
anonymously — a record with no service identity cannot be grouped, alerted on or
searched.

**OTLP (opt-in).** An application instrumented with an OpenTelemetry SDK sends
its own logs to the collector, and keeps the `service.name` it chose.

A service that ships its own logs must set `skip_filelog: true` in its manifest,
or its lines arrive twice — once from stdout, once from OTLP:

```yaml
services:
  api:
    image: registry.example.com/checkout/api:1.4.0
    skip_filelog: true
```

That opt-out is enforced in two places, deliberately: the collector's
receiver rule refuses to start a receiver for the container at all, and the log
filter drops any record still carrying `io.pmcluster.skip_filelog=true`. A
consent boundary that fails open is worse than a duplicated line.

`skip_filelog` suppresses **logs only**. The container keeps reporting CPU and
memory through Docker stats — you are opting out of log duplication, not out of
monitoring.

**Known limit:** the Docker observer behind this path only reports a container
that exposes at least one port. A container with no `ports:` in its manifest
produces no filelog receiver, so it contributes container metrics but no stdout
logs. Give a workload one exposed port if its stdout matters.

Do not also exclude the container in `filelog`'s `exclude:`: that option matches
file paths, and container log paths are built from container ids, so a
name-based glob silently matches nothing.

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

Preserve the control-plane database and encryption key alongside the telemetry
volumes: the Parseable credential is stored there, and without it the daemon
cannot authenticate its reads. Parseable's own identity is the `deployment_id`
in `.parseable.json` inside its data volume, which is independent of the task
hostname — replacing a task does not create a new backend.

Docker-published ports bypass a host's usual input firewall: they are DNAT'd
past `ufw`'s INPUT chain, so a "deny 8000" in ufw does not close them. The
forwarding path is the one that counts. `./harden-host.sh` (repo root) closes
the published-but-private ports — OTLP `4318` and Parseable `8000` — in
`DOCKER-USER`, RETURNs docker subnets and loopback first (the daemon reads its
own published ports), and installs `pmcluster-docker-user.service` so the rules
come back after every boot. It is idempotent: re-run it on each node after an
upgrade. To do it by hand:

```sh
iptables -I DOCKER-USER -s 127.0.0.0/8 -j RETURN
iptables -I DOCKER-USER -p tcp --dport 8000 -j DROP
```

Two verification steps, and you need both: from **another host**, `8000` must be
unreachable; from the **node itself**,
`curl -sf -u <user>:<pass> http://127.0.0.1:8000/api/v1/logstream` must still
answer. The first proves the WAN side is closed, the second proves you did not
cut the daemon off from its own backend.

Keep public HTTPS separate from private native ingress, and add any further
published-but-private port to `RESTRICTED_DOCKER_PORTS` in the script. Note that
`--dport` in `DOCKER-USER` matches the *container* port; a remapped publish
(host 9000 → container 8000) needs a conntrack match instead, which the script
does not generate today.

The local Parseable store keeps a stable backend identity with its persisted
state. During an upgrade, the lifecycle captures an existing identity before
rendering the replacement configuration; when existing volumes are reused,
keep that identity and its stored setting so the backend is not renamed and
its history remains addressable.

## Security

- **One gate: the daemon's.** `GET /api/observability/*` requires a bearer token
  with at least the viewer role. A stack-scoped token is refused — cluster
  telemetry is not a tenant surface.
- **The console never proxies.** Its observability routes are viewer-level reads
  that call the daemon; the backend credential does not exist in the edge
  process, so a console compromise does not yield telemetry-store access.
- **The browser supplies filters only.** A service filter, an allowlisted
  severity, a length-capped search string and a bounded offset/window. There is
  no SQL, no endpoint and no backend URL in any request.
- **Every query is bounded**: a 24-hour window cap, row caps, a 4 MiB response
  cap, a 10-second deadline and a concurrency gate. A slow backend sheds load
  instead of queueing without limit.
- **Telemetry is redacted before it is rendered.** Values that look like
  credentials — JWTs, cloud keys, bearer/basic headers, assignments to
  password/token/secret fields — are masked, and the managed Parseable password
  is masked exactly.
- **The WAN cannot reach the backend.** See the port note above.

Viewers read the page. There is no write surface: nothing here can deploy,
restart or delete anything.

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

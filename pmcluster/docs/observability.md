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
  application and daemon OTLP ───────────────────────┴─► OTel collector (one task,
                                                          pinned to the leader)
                                                                │ OTLP/HTTP
                                                                ▼
One per cluster:                                            Parseable
                                                       (local Parquet store)
                                                                ▲
                                                                │ bounded SQL
                                                                │ over the platform
                                                                │ node's :8000
  CLI ──► pmcluster daemon ─────────────────────────────────────┘
            ▲           │
  console ──┘           └──► /api/observability/*   (bearer + viewer role)
```

- **One collector task, pinned to the leader.** Container logs and container
  metrics are node-local, so a single task sees only the leader's containers;
  that is the accepted cost of one process, one reservation and one config
  instead of four. Per-node CPU, memory and processes do not come from the
  collector at all — every node's daemon samples its own `/proc` and exports it
  (see "Per-node resources" below). The task carries a `node.role == manager`
  constraint and a **hard** `node.labels.pmcluster.leader==true` constraint, so
  it runs on the leader and nowhere else. The daemon maintains that label as an
  invariant — stamped on leadership gain, re-asserted every leader tick, cleared
  on loss, and cleared from any other node still carrying it — and both
  `cluster up` and `cluster update` treat it as a precondition: `cluster up`
  claims it for the current leader, and `cluster update` refuses to deploy this
  stack when no node carries it (`pmcluster doctor` reports the same condition).
  A constraint alone would not move a running task, so when a render changes the
  collector's placement the reconcile pass force-updates the service.
- **One Parseable backend per cluster.** It stores logs, metrics and traces on a
  mounted volume and answers SQL. It is placed on the same storage
round-robin every stateful stack uses, and stays there.
- **The daemon owns the telemetry connection.** It alone holds the Parseable
  credential, applies redaction and bounds every query. The console and the CLI
  both read telemetry through it, so there is one auth check and one place a
  backend query can be built.
- **The console holds no backend credential.** It renders what the daemon
  returns. The edge never joins the monitoring network and never talks to
  Parseable.
- **The provisioning job runs once.** It applies per-stream retention and the
  platform dashboard through Parseable's API, then exits.

### Why Parseable publishes a host port, and on which node

Parseable keeps its API on the cluster networks, but its HTTP port is published
**in host mode** (`8000`). The daemon is a **host process**: it cannot resolve
`observability_parseable` from the overlay, so a published host port is how it
reads telemetry.

Host mode, not the routing mesh, because that is the difference between one
listener and four. A host publish binds the port **only on the node running the
task**, resolved from the swarm rather than from a placement setting — and
every daemon reads that one address over the cluster's private network (that
node's own loopback included). An ingress
publish would bind all four nodes for a reader that only ever needs one, and each
of those four is another interface that must be kept closed.

`4318` is the opposite case, and it is published with the routing mesh on
purpose: every node runs a daemon that has to reach a collector, so the port has
to exist everywhere and one task serves them all. That whole subject — the
address the daemon dials, what ingress buys and costs, and the one rule that
keeps the port off the WAN — is
[below](#self-telemetry-the-daemons-own-export).

Because one node listens and the rest dial it, that node's host firewall must
allow `8000` from the cluster's private range — `./harden-host.sh` does that, and
still denies it to the WAN. Two things then need verifying, and they are not
interchangeable: the readers (a `curl` from a **peer node** to that node's
private address) and the WAN (unreachable from **outside**).

`./harden-host.sh` (repo root) closes that port: `8000` sits in
`RESTRICTED_DOCKER_PORTS` alongside `4318`, the script RETURNs every PRIVATE
source (`127.0.0.0/8`, the overlay pools, and the cluster's private peer network)
before dropping, and it allows `8000` through ufw from that same private range.
Both halves matter, because a host-published port is reached from a peer's
private address — not from a bridge subnet and not from loopback. Allow-listing
only `172.16.0.0/12` drops every peer read while Swarm keeps its ESTABLISHED
tunnels alive, so the cluster still reports `Ready` and the only symptom is a
port that answers where the task runs and hangs from everywhere else.
Re-run it on every node after upgrading — it is idempotent, and it resets ufw
before rebuilding, which is what keeps the allows ahead of the denies.

Verify both directions, and treat them as different questions: from a **peer
node**, `curl -sf -u <user>:<pass> http://<platform-node-address>:8000/api/v1/logstream`
must answer, and from **outside** `8000` must be unreachable. The first proves the
daemon can still read its own backend; the second proves the WAN is closed.

## What is collected

| Signal | Source | Notes |
| --- | --- | --- |
| Container logs | Docker `json-file` logs | Collected when the global collector can resolve the container identity; opt-out services, unknown identities and selected platform chatter are filtered. stdout/stderr provide the default severity |
| Container metrics | Docker stats API | CPU, memory, network and block I/O, labelled with stack and service |
| Node metrics | Host `/proc` and `/sys` mounted read-only | CPU utilization, memory utilization and load average |
| Traces | OTLP from instrumented applications | Empty until an application sends spans |
| Platform telemetry | pmcluster daemon | Deploys, reconciles and backups |

Traefik contributes **no** traces: the infra stack runs with
`--tracing.otlp=false`. The flag stays explicit next to the (retained) gRPC
endpoint and service name so the decision is visible, and turning tracing back
on is a one-line change; it was switched off because it emitted a span per
proxied request for no consumer. Traefik's access log is off for the same
reason (`--accesslog=false`) — the request path already shows up as container
logs and metrics.

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

**Volume: `LOG_LEVEL` is injected, not requested.** Every service of every app
stack is rendered with `LOG_LEVEL` — the manifest's `log_level`, else the
cluster's `app_log_level` setting, else `warn`. The default is `warn` so a chatty
app does not fill the log stream at INFO; a service that sets `LOG_LEVEL` itself
keeps its value. The platform's own level (`log_level`, default `info`, applied
to the daemon and the edge) is a separate setting — see
[settings.md](../../docs/settings.md#logging-two-settings-one-env-var). A runtime
variable is only honoured if the application reads it; the injection makes a
conventional name available, it cannot change a library's default.

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
`4318` **with `mode: ingress`**, so the routing mesh binds it on every node and
forwards to the collector task, wherever that task runs; the internal gRPC port
is not published at all.

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://<node>:4318
export OTEL_SERVICE_NAME=checkout
```

Applications inside the overlay keep using the collector service name; only a
host process needs the published address, and its rules are in
[Self-telemetry](#self-telemetry-the-daemons-own-export).

`severityNumber` is the reliable level field for OTLP logs. If only
`severityText` is sent, Parseable stores the level as unspecified.

## Self-telemetry: the daemon's own export

`pmcluster serve` ships its own traces, metrics and logs over OTLP, and so does
every CLI command that emits spans. It is the one exporter in the platform whose
endpoint cannot be a service name — the daemon is a systemd process on the host,
outside the overlay — and everything below exists because that difference was
once paid for in journal lines instead of being understood.

### What the endpoint used to be, and what it cost

The endpoint was the constant `http://127.0.0.1:4318`, while the collector
published `4318` in **host** mode — a port that exists only on a node whose
collector task is actually serving. On a node where that task was restarting,
unscheduled, or not yet deployed (the stack is deployed by `cluster up`, so every
node is in that state before its first rollout), the daemon's loopback address
had nothing behind it. The OTel SDK's default error handler writes a line for
every failed export, with no rate limit and no recovery path.

On 2026-11-08 node `nxt-sw-4-m` (1 vCPU) recorded:

| writer | journal lines from one boot |
| --- | --- |
| the daemon | 261,617 |
| five further incarnations (restarts and CLI children) | 238,951 · 213,808 · 196,367 · 151,609 · 71,180 |

1,133,532 lines in total, and `pmcluster.service` started **65 times** in the same
boot. On one vCPU those writes competed with Docker's own heartbeat: the swarm
marked the node `Down`, leadership moved to `nxt-sw-5-m`, and 5-m answered **401**
to an API key created minutes earlier on 4-m — a second, separate failure, fixed
by adopting the control plane on leadership gain
([control-loop-design.md](../../docs/control-loop-design.md)).

That arithmetic is the design constraint, not the drama: one misaddressed
endpoint produced O(retries) journal lines for as long as the condition lasted,
on a box with one core. Three separate bounds now sit on that path — the
endpoint, the log, and the restart policy.

### The endpoint is the collector's: loopback on the leader, the leader's address everywhere else

`otlp_endpoint` defaults to `auto`, and `auto` means "the one collector, wherever it is":

| this node | endpoint |
| --- | --- |
| the leader | `http://127.0.0.1:4318` — the collector runs beside it |
| any other node | `http://<a manager that answers on 4318>:4318` — that answer *is* the leader |

Every daemon in the cluster exports its own logs, metrics and traces
(`internal/config/selftelemetry.go`). The only difference between nodes is where the
collector is: the leader dials loopback, and everyone else dials the leader.

A manager can read the swarm and see which node leads; a **worker cannot** — `docker node
list` and `node inspect` are manager-only, and the swarm API never reports the leader to a
worker. So a follower asks the same question the mTLS assignment channel asks, in the same
way: take every manager address the swarm knows (`docker info`, `RemoteManagers`) and use
the first that **answers** on 4318 (`internal/cli/telemetry_leader.go`). Only the leader
has the collector published, so the first answer is the leader: there is no second
election to keep in sync, and the next probe (a minute later) corrects it after leadership
moves.

The dial is what makes that address safe to hand an exporter. A follower **never** dials a
port nobody has answered on: with no answer it stays disabled and says it is being
re-probed. A missing trace is acceptable; an exporter aimed at a closed port is not — the
arithmetic below is why.

An explicit URL is used verbatim — the operator's decision, never second-guessed. An empty
value disables self-telemetry, from any source, including a deliberately empty
`PMCLUSTER_OTLP_ENDPOINT=`.

Each of the obvious alternatives fails in its own way, which is why the list is worth
writing down once:

| Candidate | Why it is not used |
| --- | --- |
| the overlay service name (`observability_otel-collector:4318`) | the name exists only inside the overlay's DNS. `serve` is a host process and has no overlay DNS, so for it this is a name-resolution failure instead of a refused connection — the same storm behind a different error |
| an address derived from the route or the egress interface (`hostname -I`, a UDP "connect" trick, the default-route address) | yields *an* address of this host, not the address the swarm knows it by. On a node with several interfaces that is a private address nothing listens on: the same closed port, reached by a different route |
| disabling the exporter everywhere but the leader (what shipped in v0.2.323) | four daemons of five exported nothing. Their samples still reached the leader over mTLS and stopped in `node_stats`, so the console showed one node in a five-node cluster and "quiet" became indistinguishable from "not collected" |
| a collector per node (`mode: global`) | it would collect each node's own container logs and stats, which is a real gain — but it multiplies the collector, its Parseable exporter and its memory on every node to solve a problem that is about the *daemon's* telemetry. Left as a future decision, not smuggled in here |

Resolution is **supervised, not decided once**. A startup-only decision was the
shape of the original bug in a second disguise: a node that booted while the
collector was being rescheduled disabled its exporter permanently, and nothing
re-checked — measured on a live worker whose journal still said `self-telemetry
disabled: nothing answers at http://10.7.224.10:4318 (connection refused)` six
hours after that port had started answering `200`. The console then showed one
node in a five-node cluster, which reads as a broken panel rather than as four
daemons that opted out at boot.

So the daemon re-probes **every minute** (`internal/cli/telemetry_supervisor.go`)
and reconciles the exporter with the answer: start it when something answers,
leave it alone while the same endpoint answers, shut the old one down and start a
new one when the address changes (one node never exports under two addresses),
and stop it when the answer goes away. A failed probe is one DEBUG line while the
state does not change and one WARN at the transition — never a line per minute.
The node-stat sampler starts on the **first** successful probe, because a sampler
whose export goes nowhere does work nobody reads.

The cost is one dial a minute per node. The alternative — a five-node panel with
one row, fixed by restarting four daemons by hand — is what that dial buys.

A CLI command resolves the same way, with a five-second bound on the docker
socket so a wedged daemon cannot delay a one-shot command. Run off-cluster — a
laptop, a CI runner — there is no swarm address, so the command exports nothing
and works normally. That is deliberate: `pmcluster` must run anywhere, and the
CLI children were part of the same incident (238,951 and 213,808 of the lines
above came from single incarnations).

### Why 4318 is published host-mode on the leader

The collector is one task on the leader; `filelog` and `docker_stats` therefore describe
the leader's containers only. What every *other* node's daemon needs is not a task beside
it, but a reachable collector: `4318` is published with `mode: host`, so it is bound on the
node that runs the task, and a follower reaches it over the swarm data path at the leader's
own address — the address the routing mesh and every peer already use.

`mode: ingress` was tried and rejected: it binds the port on **every** node and forwards to
one task, so "does anything answer here" stops being a property of the role and becomes a
question about the mesh's health — and `doctor` treats an ingress-published 4318 as a
finding, because every node would then accept OTLP it may never deliver.

- **What it buys:** the daemon dials this node's own address and something answers
  there regardless of where the collector task is or whether the one on this node
  is up. Placement stops mattering, and no node has to host a collector for its
  daemon to be observable.
- **What it costs:** when the task that answers is on another node, the export
  crosses the mesh — one VXLAN hop — instead of a loopback write, for a few KiB
  every few seconds. And the port now listens on every node's public interface,
  which is only acceptable because of the rule below.

An ingress publish of an unauthenticated OTLP receiver is not a small thing to
leave open: it would be an open ingestion endpoint on every node's public
address.

### 4318 is closed by a `DOCKER-USER` rule, never by ufw

Docker-published ports are DNAT'd past `ufw`'s INPUT chain, so `ufw deny 4318` —
which `harden-host.sh` also writes — is not what keeps this port shut, and
`ufw status` is not evidence either way. The gate is the `DOCKER-USER` chain the
same script installs: every private source RETURNs first (loopback, the docker
and overlay pools, the cluster's private peer network), then `4318` and `8000`
are DROPped for everything else.

The private ranges have to be on the allow side because an ingress-published
port is answered by whichever node currently runs the task: a peer's request
arrives carrying the peer's own source address — not a bridge subnet, not
loopback. Allow-listing only the docker and overlay pools (`172.16.0.0/12`) drops
a peer that connects from, say, `10.7.x`, and Swarm's own tunnels are unaffected,
so the cluster still reports `Ready`: the only symptom is a port that hangs from
every node except the one whose task answered locally.

Two consequences follow, and they are operational rather than theoretical:

- **A rebuilt node has no rule until `harden-host.sh` runs there again.** The
  script writes the chain to a unit that re-applies it after Docker starts, so an
  installed node keeps the rules across reboots; a node rebuilt from a plain
  image, or one whose Docker was reinstalled over an emptied chain, answers
  `4318` on its public interface until it is hardened.
- **Verify from outside the cluster.** A TCP connect from a peer proves the
  daemon can reach a collector; it proves nothing about the WAN, because the peer
  is on the allow side by design.

### A failing export costs a bounded number of lines

`internal/telemetry/failures.go` wraps the exporter's error handler: the first
failure is logged with its error and the interval it will be quiet for, then at
most one summary per five minutes carrying the suppressed count, then one
recovery line when the endpoint answers a dial again. A one-hour outage therefore
costs thirteen lines (one first, twelve summaries) plus the recovery line when it
ends — not one per export. The daemon and the CLI wire the same reporter; each
renders its own wording from the same three events.

The reporter asks the endpoint only whether it answers a dial, not what the
error says, deliberately: a collector that is up but rejecting payloads still
produces a failure per export and must be summarised, not narrated.

### The unit restarts at most five times in five minutes

The rendered systemd unit is `Restart=always` with `RestartSec=10`,
`StartLimitIntervalSec=300` and `StartLimitBurst=5`
(`internal/cli/daemon.go`). After five failures inside five minutes systemd
stops trying and leaves the unit `failed`, which is a state an operator can see
and act on. The cost is that the daemon stays down until somebody looks — the
right failure mode for a control plane that has already shown it cannot start,
and the alternative to the 65 starts recorded above.

`systemctl status pmcluster` shows the loop; `systemctl reset-failed pmcluster`
re-arms the unit once the cause is fixed.

### What was verified, and what was not

Measured on the deployed fleet after the change: `4318` is closed from the
internet on all four nodes (a TCP connect from outside times out), the collector
task is `Running`, and the daemon journal shows **0** occurrences of
`context deadline` or `connection refused` since the restart.

Not measured: whether the daemon's spans and metrics actually arrive in
Parseable. The absence of export errors is what was checked, and it is the
question this change was about — an exporter with nowhere to send is silent, not
broken-looking.

```sh
# from OUTSIDE the cluster: a timeout is the pass, a completed connect is the bug
nc -z -w3 <node-public-address> 4318; echo "exit $?"

# on the node: the rule that is doing the blocking (empty = unprotected)
sudo iptables -S DOCKER-USER | grep 4318

# the collector task, and whether the daemon is complaining
docker service ps observability_otel-collector
journalctl -u pmcluster -S "1 hour ago" | grep -c "context deadline"
```

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

## What the console page costs

A Parseable query costs time in proportion to the window it scans — there are no
rollups, so a log page over 24 hours is roughly ten times the work of the same
page over one hour. The console is built around that fact:

- **The default window is one hour.** It is the range that answers "what is
  happening now" at a fraction of the cost, and the selector still offers 15m,
  6h and 24h for when you need history.
- **The two log queries that make up an investigation run in parallel** — newest
  errors, then newest of any level — instead of queueing, so the wait is one
  query rather than two.
- **Live mode is incremental.** Each poll asks only for rows newer than the
  newest row on screen (the row's `data-ts` becomes the query's lower bound) and
  appends them, so the panel is never re-read or re-rendered. A backgrounded tab
  issues no requests at all, and neither does one where the search box has focus.
- **Evidence responses are cached for about ten seconds**, keyed by service,
  window, severity, search and offset. Live and incremental requests bypass the
  cache — they are fresh by definition.

### One wave per page, not a phase per panel

Every read a page makes is independent of every other read, with two data
dependencies that are real rather than incidental:

- the evidence read must not resolve a service against an inventory that never
  answered, so it starts when the inventory lands and fails closed on its error;
- a node card is one inventory node plus its metric series, so metrics are
  fetched with everything else and projected after the inventory resolves.

Everything else — cluster info, nodes, backups, certificates, log volume, trace
stats, the newest errors, node metrics, container metrics — is issued at the same
time. The page therefore costs its slowest read, not the sum of its panels. The
distinct-telemetry-identity read (which feeds the logs tab's service picker) is
only issued on the logs tab; on the overview, metrics and traces tabs its answer
was never rendered.

Measured on this repository's console against a local stub that answers every
daemon call after a fixed 250 ms — the round-trip time measured from a laptop to
a production cluster — with five interleaved samples per build and the median
reported. One backend wave costs 0.3–0.4 s in this harness (the fixed delay plus
request overhead), so the "after" column is one wave per page, and the "before"
column is the number of waves each page used to pay for:

| Page / fragment | before | after |
| --- | --- | --- |
| `/web/overview` | 1.77 s | 0.40 s |
| `/web/stacks` | 0.74 s | 0.30 s |
| `/web/observability` (overview tab) | 1.10 s | 0.38 s |
| `/web/observability?tab=logs` | 0.77 s | 0.42 s |
| `/web/observability?tab=metrics` | 1.56 s | 0.39 s |
| `/web/observability?tab=traces` | 1.09 s | 0.38 s |
| `/web/observability/summary` | 1.14 s | 0.43 s |
| `/web/observability/evidence` | 0.76 s | 0.42 s |
| `/web/observability/traces` | 1.11 s | 0.41 s |

Against the live cluster the numbers are different, because the harness charges
every call the same 250 ms while production does not. The live before/after table
— the one to quote to an operator — is in
[console.md](../../docs/console.md#loading-states): `/web/overview`
1.09–1.29 s → 0.42–0.60 s, `/web/stacks` 0.50–0.65 s → 0.13–0.16 s,
`/web/observability` 4.72 s → 1.83–2.43 s.

One row disagrees between the two, and the disagreement is the interesting part:
the harness shows `summary` improving (1.14 s → 0.43 s), while on the live cluster
it did not move at all (1.45–1.60 s before and after) and is now the page's floor.
In the harness a panel made of several 250 ms calls gains from being issued
together; in production that panel's wall time is dominated by one long Parseable
scan, which no amount of concurrency removes. A per-query breakdown of `summary`
was not taken, so how much of its time is round trips and how much is one
expensive scan is an inference from those two measurements, not a measurement.

Absolute numbers on a live cluster vary with the link, so compare builds over the
same interval — never one page load against another hours later.

`internal/ui/controllers/page_concurrency_test.go` pins this down: it holds every
daemon read for a fixed time, counts the peak number in flight per family, and
fails if a page goes back to issuing its reads one after another.


## Configuration

Retention is configured as cluster settings and applied by the provisioning job:

| Setting | Default | Meaning |
| --- | --- | --- |
| `observability_logs_retention_days` | 30 | days of logs |
| `observability_metrics_retention_days` | 14 | days of metrics |
| `observability_traces_retention_days` | 10 | days of traces |

Each value must be a whole number of days, one or more; the settings API rejects
zero, negative and non-numeric values. The three defaults also act as the
fallback inside the provisioning script, so a missing environment variable
preserves the built-in policy.

```sh
pmcluster cluster settings set observability_logs_retention_days=14
pmcluster cluster update
```

Because retention is applied by the one-shot provisioning job (not read from
settings at ingest time), a change takes effect on the next `cluster update` —
or the next provisioner run — not immediately.

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
past `ufw`'s INPUT chain, so a "deny 8000" in ufw alone does not close them. Both
paths are covered by `./harden-host.sh` — see "Why Parseable publishes a host
port, and on which node" above for the rule shape, the ufw allow for the private
range, and the two verification steps. Keep public HTTPS separate from private
native ingress, and add any further published-but-private port to
`RESTRICTED_DOCKER_PORTS` in the script. Note that `--dport` in `DOCKER-USER`
matches the *container* port; a remapped publish (host 9000 → container 8000)
needs a conntrack match instead, which the script does not generate today.

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

## API and MCP surface

Every signal is exposed over the bearer-authenticated daemon API, and the same
routes are derived into MCP tools under `/api/mcp`. All reads are bounded —
never raw SQL — and require at least the viewer role; a stack-scoped token is
refused.

| REST route | Bounds |
| --- | --- |
| `GET /api/observability/logs` | window ≤ 24h, `limit` ≤ 200, `offset` ≤ 2000, allowlisted `severity`, `search` ≤ 100 runes, optional `service` and `since` |
| `GET /api/observability/metrics/nodes` | window ≤ 24h, fixed node metric set, `bucket` duration |
| `GET /api/observability/metrics/services` | window ≤ 24h, fixed container metric set, `bucket` duration |
| `GET /api/observability/traces` | window ≤ 24h, `limit` ≤ 50, `offset` ≤ 1000, optional `service` and `errors_only` |
| `GET /api/observability/traces/{trace_id}` | 32-hex trace id, window ≤ 24h, ≤ 500 spans |
| `GET /api/observability/alerts` | `limit` ≤ 500 |

Each MCP tool carries the same bounds in its input schema, so an agent cannot
ask for an unbounded scan:

| MCP tool | Signal | Bounds |
| --- | --- | --- |
| `pmc_observability_logs` | log search | window ≤ 24h, `limit` ≤ 200 |
| `pmc_observability_traces` | traces by window | window ≤ 24h, `limit` ≤ 50 |
| `pmc_observability_trace` | one trace by id | 32-hex id, ≤ 500 spans |
| `pmc_observability_node_metrics` | node metrics (fixed set) | window ≤ 24h |
| `pmc_observability_service_metrics` | container metrics (fixed set) | window ≤ 24h |
| `pmc_observability_alerts` | alert list | `limit` ≤ 500 |

The daemon memoises full-window reads for about ten seconds, so a burst of
identical reads (console page fragments, an agent polling through MCP) pays
Parseable once. Readers that arrive while an identical read is still in flight
join that read instead of issuing their own, and an answer that is not
`available` is never memoised, so a backend outage heals as soon as the backend
does. Live and incremental reads bypass the cache. Measured on an
Apple M4 dev host against a mock backend with a 5 ms response: a repeat read
drops from ~5.6 ms (one backend round trip) to ~207 ns (cache hit). The live
cluster's Parseable was not reachable from that host, so this is the round-trip
floor rather than a production figure.

## Per-node resources (console overview)

The overview answers one question: *what is eating this node, right now?* It is a
separate panel from node health on purpose — node health comes from the Swarm API,
and this comes from the control-plane database, so each survives the other's
outage. After the 2026-11-08 incident the console showed nothing while
`nxt-sw-4-m` sat at load 10 with the daemon at 902 MB RSS; the post-mortem took
hours because the numbers lived only in a place nobody could read.

```text
daemon on every node ── sample /proc (loadavg, meminfo, per-pid stat/status)
        │                 every 15s: load1/5/15, mem total/used/available,
        │                 cores, docker reachable, top-5 by RSS and by CPU
        │ OTLP/HTTP → this node's swarm address :4318 (ingress)
        ▼
collector (leader) ── filter node-stats → file exporter
        │                                   /var/stack/nodestat/nodestat.jsonl
        │                                   rotation: 2 MiB, one backup
        ▼
leader's daemon ── tail by byte offset + inode (256 KiB rebuild on rotation)
        │           newest sample per node, malformed lines skipped and counted
        ▼
node_stats table ── ONE row per node, upserted in place
        ▼
console /web/overview ── hostname, role, load, memory, top processes, sample age
```

- **The daemon samples, not the collector.** The collector is a single leader-pinned task,
  so a receiver inside it can only describe the leader — which is why `hostmetrics` is gone
  rather than kept: each node's daemon samples its own `/proc` and publishes
  `system.cpu.utilization{state=idle}`, `system.memory.utilization{state=used}` and
  `system.cpu.load_average.1m` for itself, straight to the collector over the same OTLP port
  applications use. One producer per node, on every node, and the collector never
  re-collects what a node already measured about itself. The daemon never talks to
  Parseable.
  **The leader exports too** — the file exporter is a sink, not a loop, so there
  is nothing for the leader to avoid by staying silent. A swarm that records the
  leader's own address as `0.0.0.0` is dialled on loopback, which is where the
  co-located collector's published 4318 answers.
- **The panel does not depend on Parseable.** The collector writes the series to a
  host-bound JSONL file as well as to the backend, and the leader's daemon reads
  the file. A telemetry outage can cost history; it cannot blank the panel.
- **One row per node, forever.** `node_stats` is `hostname`-keyed and upserted.
  The table is O(nodes); it never grows a per-sample history. Rows for nodes that
  left the swarm are deleted; a node that merely stops reporting keeps its last
  row and ages.
- **Four tint rules**, computed in the controller and tested at their boundaries:
  load average above 1.5x the node's cores, available memory under 15%, a sample
  older than 3 minutes (greyed *and* labelled stale), and an unreachable Docker
  engine on that node.
- **The directory is provisioned by the platform, not by Docker.** The collector
  bind-mounts `/var/stack/nodestat` from the host, and Swarm rejects a task whose
  bind source is missing (`bind source path does not exist:
  /var/stack/nodestat`) — the task never runs, the service sits at 0/1 and no
  container log exists to explain it. `cluster up` and `cluster update` therefore
  create every host directory the platform stacks bind-mount before the stack is
  deployed, idempotently, and `cluster update` reports a path it cannot create
  (read-only filesystem, permissions) as an apply warning instead of aborting a
  reconcile. The path itself is one constant (`cluster.NodeStatHostDir`) that the
  bind mount, the collector's file exporter and the mkdir all read, so they
  cannot drift. The collector's other bind sources need no help: its docker
  socket, `/var/lib/docker/containers`, `/etc/localtime`, `/proc` and `/sys` are
  Docker's or the kernel's, and Parseable's data/staging dirs are named volumes
  the deploy pipeline creates under the volume root.
- **The reader never truncates the file.** The exporter holds it open; truncating
  produces a sparse NUL-filled middle. Rotation is the only cleanup, and the
  reader detects it (inode change or shrink) and rebuilds from the last 256 KiB.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Page shows no telemetry | `docker service ps observability_parseable` and `docker service ps observability_otel-collector` |
| No node metrics | collector task is running on that node and has host metrics access |
| Collector `Rejected`: `bind source path does not exist: /var/stack/nodestat` | `ls -ld /var/stack/nodestat` on the leader; run `cluster update` (or `cluster up` on a fresh node) — the platform creates it, and a warning names the path when it cannot |
| A node has no row in Node resources | `journalctl -u pmcluster \| grep -i self-telemetry` — the exporter is re-probed every minute, so a node recovers on its own when the collector comes back; a persistent `nothing answers at …:4318` is the collector, not the daemon. The leader's wildcard swarm address is expected to resolve to loopback |
| `/var/stack/nodestat` grows without bound | the exporter is running with `max_backups: 0` (a rolled file is renamed and never deleted — 722 MiB / 372 files was measured on a live leader). `cluster update` re-renders the collector config to `max_megabytes: 2, max_backups: 1` |
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
- Node CPU, memory and load come from each node's own daemon (see "Per-node resources");
  there is no `hostmetrics` in the collector any more, so nothing is scraped from the
  leader's host. Disk, network and per-state CPU are not part of this version.
- The top five processes per node are capped at five per axis by design.
- Historical rows written before a collector identity change may not carry the
  current `service.name`.

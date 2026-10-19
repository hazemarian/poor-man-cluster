# Native pmcluster observability and investigation

Status: native implementation built locally, 2026-10-17; verification in progress. This document
supersedes the separate ObsAI service deployment in `nextrum-obsai-design.md`.
The evidence-compaction ideas in that document remain useful; its Python
prototype is parked and is not a production dependency.

## Outcome

Operators use the existing pmcluster console to see cluster health, inspect a
service's telemetry, and ask an evidence-backed question about an incident.
The feature uses the existing hostname, login, navigation, release artifacts,
and Swarm platform stacks. Parseable remains the telemetry store and an optional
deep-dive interface.

## Deployment and process boundaries

```text
Browser → existing Traefik/auth → pmcluster-edge
                                  ├─ existing gin/HTMX console
                                  ├─ observability evidence engine
                                  ├─ Parseable over monitoring-net
                                  ├─ daemon API for cluster state
                                  └─ configured model endpoint over HTTPS

Host pmcluster daemon → Docker + SQLite
OTel collectors → Parseable
External watchdog → independent cluster availability checks
```

The daemon runs as a host systemd process. It cannot use Swarm service DNS to
reach `observability_parseable`; the existing provision script documents this
constraint. Putting a Parseable client in the daemon without a connectivity
change would be an incomplete design.

Recommendation: run telemetry queries, evidence compaction, and on-demand model
calls inside the existing edge binary. Add `monitoring-net` to its stack
networks and use `http://observability_parseable:8000` internally. The edge
already talks to the daemon through its typed `pmapi` client. Parseable is also
currently on `traefik-net`; use the monitoring network explicitly for this
feature rather than depending on that incidental shared network.

No new container, published port, hostname, Python runtime, or Docker socket is
needed. The daemon continues to own cluster state, durable settings, secrets,
and any later durable accounting. The edge owns presentation and bounded
interactive investigation. This is a deliberate refinement of the earlier
proposal to put all query logic in the daemon.

Release impact: update the existing edge image and platform network/secret
configuration; update the daemon binary only for new state/accounting APIs.
Collector repairs are a separate change to the existing observability stack.
Reverting the UI feature flag leaves existing platform operations available.
Actual deployment commands and rollout checks belong to the implementation
change, after this design is accepted.

## Page and investigation flow

Add **Observability** to the existing sidebar, using the current typography,
surfaces, tables, filters, status vocabulary, EN/AR dictionaries, and RTL layout.
Do not create a separate application shell.

```text
Observability                     [service: All] [range: 1h] [Refresh]
Last updated · collection coverage · any stale/unavailable sources

Nodes ready | Services ready | Pending tasks | Backup age | TLS expiry

Node resources                         Logs by service
CPU · memory · load                    Errors · warnings · volume

Recent changes                         Telemetry collection
Deploy revisions · task failures        Logs · metrics · traces freshness

Ask the cluster
[What changed before this service started failing?                 ]
Selected service + time window · provider/data-sharing notice · [Investigate]

Result: summary · ranked hypotheses · evidence links · suggested actions
```

Start with tables and compact trends. Add vendored charting only when series
and missing-data semantics are correct. Source timestamps, gaps, and unavailable
signals must be visible. Current hostmetrics provide CPU/memory/load; do not
promise disk or swap charts until their receivers and values are verified.
OOM counts require actual event evidence; a restart or uptime reset is not proof
of an OOM kill.

The service detail page gains an **Investigate** link that carries its service
and time range into this page. An investigation first collects a bounded
evidence brief; the model receives that brief and the question. The result
shows observed facts separately from hypotheses and links back to the evidence.
Actions such as restart or rollback remain the existing deliberate operator
flows; the model cannot execute them.

No model configured: the dashboard and evidence inspection still work. No logs:
show collection coverage and explain the gap. Parseable unavailable: preserve
daemon-derived health and label telemetry unavailable. A successful empty query
is distinct from an unavailable source. A failed individual chart does not
replace the whole page with an error.

## Code responsibilities

| Area | Responsibility |
| --- | --- |
| `internal/observability` (new) | Typed evidence models, query bounds, aggregation, redaction, source interfaces; no UI or transport imports |
| `internal/observability/parseable` adapter (new) | HTTP client, fixed query builders, explicit time bounds, response/row/byte/time limits, strict response decoding; imports parent contracts, never the reverse |
| `internal/investigation` (later) | Provider interface, cited verdict validation, concurrency/cost limits; depends on evidence contracts |
| `internal/ui/controllers/observability.go` (new) | Request filters, access checks, template data and investigation job presentation |
| `internal/ui/views/templates/frag_observability.html` (new) | Native HTMX page/fragments and escaped result rendering |
| `internal/ui/pmapi` | Existing daemon state methods plus narrowly required additions |
| `internal/ui/ui.go`, `cmd/edge/main.go` | Explicit dependency injection, route registration, worker startup/shutdown |
| Daemon domains/store/server | Durable configuration/accounting only where required; normal bearer-role and stack-scope enforcement |
| `internal/cluster/embeds/edge-stack.yml` | Monitoring network and managed secret mounts |

Document the edge composition boundary in `internal/ARCHITECTURE.md` when
implemented. Domain code must not import CLI, UI, server, or remote adapters.
Controllers should not contain SQL, provider request assembly, or redaction.

## Request contracts

Proposed console routes, all under the existing authenticated `/web` group:

| Route | Behavior |
| --- | --- |
| `GET /web/observability` | Page fragment with service/range filters |
| `GET /web/observability/summary` | Small independently refreshed health fragment |
| `GET /web/observability/evidence` | Bounded evidence for validated service/range selection |
| `POST /web/observability/investigations` | Reserve budget, enqueue one investigation, return job fragment promptly |
| `GET /web/observability/investigations/:id` | Authorized polling for pending/running/completed/failed state |
| `POST /web/observability/investigations/:id/cancel` | Cancel queued/running work and reconcile accounting |

Resolve service selections against authorized daemon inventory before querying;
reject unknown or out-of-scope identifiers. Do not accept request-supplied SQL,
upstream URLs, provider endpoints, or daemon paths. No arbitrary tool execution
in the first version. The
browser receives rendered evidence/results, never Parseable credentials or
provider keys. Source text and model output are untrusted and HTML-escaped.

Keep model work outside HTTP request lifetimes: the existing edge server has a
60-second write timeout. Use a bounded worker and short HTMX polling rather
than holding a connection for a long model response. Proposed initial limits:
one active model call, a queue of four, a 90-second call deadline, 24-hour
maximum evidence window, bounded rows plus response bytes, and a hard output
token cap. These are configuration defaults to validate, not capacity claims.
Polls and dashboard refreshes never trigger model calls. Apply route-level
request and queue-admission rate limits: UI routes do not automatically inherit
the edge reverse proxy's shield/rate limits. Explicit cancellation, job deadline,
and process shutdown cancel downstream work; losing an individual polling
connection does not cancel a submitted job.

Every source result carries `status` (available/unavailable/stale),
`observed_at`, `window_start/end`, coverage, truncation reasons, and counts when
known. Never infer missing values as zero. Evidence IDs include enough scope
and time information to identify the cited record/template unambiguously.
Cache keys include source settings, scope, time window, evidence hash, model,
prompt/schema version, and redaction policy version where applicable.

## Authorization, credentials, and accounting

Dashboard and redacted evidence reads require viewer access; starting or
cancelling paid investigations requires operator console access; provider configuration
and direct daemon accounting mutations require admin access. Enforce both the console role and the daemon's effective
scope. The UI's shared daemon bearer token must not silently broaden a user or
stack's authorized telemetry visibility. Do not cache across different scopes.

Important existing limitation: `EDGE_LOGIN_DISABLED=true` represents every
Traefik-authenticated request as a synthetic admin. It does not provide
individual SSO roles or identity. In that mode this feature is initially a
trusted-operator, cluster-wide console with a global budget, not a multi-tenant
product. Per-person permissions, ownership, and billing must not be claimed.
Do not add stack-scoped telemetry access until identity-to-scope enforcement is
implemented and tested across every evidence and job route.

Use same-origin checks for paid POST requests even in cookieless SSO mode; the
current CSRF middleware permits a missing Origin when no session cookie exists.
Require the new investigation routes to reject missing/mismatching browser
origin metadata. Future API automation must use a separately authenticated API
contract rather than weakening these console checks.

Provider keys and Parseable auth remain server-side managed secrets mounted as
files, following the existing edge secret-loading pattern. The edge currently
receives Parseable Basic auth through an environment value for the old sign-in
bridge; new native functionality must not copy it into browser storage. Retire
that bridge as a separately assessed compatibility change after native coverage
is sufficient. Never log credentials, raw evidence, or provider request bodies.

Paid AI is disabled by default. Before enabling it, require configured provider,
model, explicit data-sharing setting, per-call token limit, and durable global
budget accounting. Reserve capacity atomically before enqueueing; reconcile
actual usage, cancellation, timeout, and uncertain provider responses. Treat
unknown usage conservatively. The edge's SSO-mode SQLite store is in memory,
so it cannot be the durable budget ledger. Persist reservations/accounting
through a narrow daemon-owned API/store migration; fail closed if unavailable.
Budget unit is tokens initially; show currency estimates only with explicit
configured pricing. Job results may be short-lived in edge memory; a restart
marks interrupted work unavailable and must not reset spend reservations.

## Build sequence and acceptance

1. **Native read-only page.** Wire daemon-derived health, filters and explicit
   unavailable states into the existing console. No model calls, new port, or
   external credentials. Validate role gates, EN/AR/RTL, keyboard flow and mobile
   layout using the existing UI test conventions.
2. **Telemetry evidence.** Add monitoring-net membership and the Parseable
   adapter. Verify exact pinned backend query contracts, labeled stdout plus
   OTLP coverage, bounded queries, secret redaction, escaping, time gaps and
   malformed-response handling. Use fixtures first, then a development cluster.
3. **On-demand investigation.** Add persistent budget reservations, provider
   configuration/secrets, bounded jobs and cited verdicts. Verify authorization,
   same-origin checks under both auth modes, budget races, cancellation,
   restart accounting, provider failures and invalid model output before
   enabling paid calls.
4. **Later integrations.** Persistent novelty baselines, deployed-code resolver,
   MCP and automatic triage can reuse the evidence contracts. The external
   watchdog remains independent of this console and is still needed for full
   cluster outages.

The user subsequently authorized implementation. Native page/evidence and
on-demand investigation code now exist in the pmcluster checkout; the earlier
Python prototype remains parked. See the [implementation guide](../../pmcluster/docs/native-observability.md)
for actual supported behavior and limitations. Production deployment remains a
separate action and has not occurred.

## Source anchors inspected

- `pmcluster/internal/cluster/embeds/observability-provision.sh`: host-daemon
  networking constraint and internal Parseable API use.
- `pmcluster/internal/cluster/embeds/edge-stack.yml`: edge networks, credential
  environment, existing managed secret mounts and upstream daemon address.
- `pmcluster/internal/ui/ui.go`: viewer/operator/admin route groups and CSRF.
- `pmcluster/internal/ui/middleware/session.go`: synthetic admin in gated mode,
  session permissions and cookieless Origin handling.
- `pmcluster/internal/ui/pmapi/client.go`: typed shared daemon bearer client.
- `pmcluster/cmd/edge/main.go`: edge composition, in-memory SSO state and server
  timeouts.
- `pmcluster/internal/ARCHITECTURE.md`: domain dependency and adapter rules.

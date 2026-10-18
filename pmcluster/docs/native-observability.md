# Native observability and investigations

The existing console now has `/web/observability`: daemon-derived node/service
health, replica deficits, backup age, certificate expiry, and bounded redacted
Parseable log evidence. Service task pages link into an investigation with the
service selected. The same page works without an AI provider.

The feature runs in the existing Go edge process. The host daemon supplies
cluster state and durable token accounting. There is no separate ObsAI service,
hostname, published port, Docker socket, or Python runtime. Parseable queries
use the monitoring overlay and a mounted managed credential. Existing Parseable
deep-dive links and its sign-in bridge remain unchanged for compatibility; the
native page does not put credentials into browser storage.

## Evidence and authorization

The daemon's `/api/me` authorization check and cluster inventory must succeed
before any telemetry query. Stack-scoped daemon tokens are denied by that check.
Service filters resolve against that inventory and accept no SQL or endpoint
from the browser. Source failures remain unavailable; successful empty evidence
remains empty. Queries are bounded by time window, rows, response bytes,
concurrency and deadline. Logs are redacted and HTML-escaped before rendering.

Ready/desired replica counts describe active regular services. One-shot outcomes
stay unknown until actual task outcome evidence is available. A missing replica
is not labelled as a pending task. Historical OOM counts, trace analysis and
persistent novelty baselines are not implemented in this version. The page does
not invent those values.

Viewer sessions may read the page; operators may submit/cancel their own model
jobs; administrators configure the provider. Jobs bind to both the console user
and the daemon connection identity. Accounting mutations require the daemon's
admin bearer, normally the privileged edge token. A direct daemon operator
cannot refund a running job's reservation.

In deployed `EDGE_LOGIN_DISABLED` mode, everyone admitted by the existing
Traefik gate is a synthetic admin. This is a trusted operator console, not
per-user SSO authorization. No tenant isolation or per-person billing is implied.
Investigation mutations require same-origin metadata even without a console
cookie. `PMCLUSTER_PUBLIC_ORIGIN` is set by the edge stack; standalone reverse
proxy deployments must configure it to their actual public HTTPS origin.

## Console layout

The page has four tabs; each loads only its own telemetry, and every tab runs
the same daemon authorization probe and inventory resolution before any
Parseable call.

- **Overview.** Daemon health tiles, one card per node with CPU, memory and
  one-minute load sparklines, and services grouped by stack. Unhealthy stacks
  sort first and open by default; stacks are paginated and searchable.
- **Logs.** Newest first, 50 rows per page, with service, window, level and text
  filters. Paging uses a bounded offset (at most 2000 rows deep); narrow the
  window or add a filter to reach older records. Container output has no level
  of its own: the collector maps stderr to `ERROR` and stdout to `INFO`, so the
  level filter reflects the stream unless the application sets a level through
  OTLP. Search matches the stored text, but rows are redacted before they are
  shown, so a search can reveal that a redacted value exists. Anyone who can use
  the page already has read access to the telemetry backend.
- **Metrics.** Per-node CPU, memory and load charts and per-container average
  and peak CPU and memory (the 15 highest peaks).
- **Traces.** Trace summaries newest first (25 per page, at most 1000 deep) with
  an optional exact service-name filter and an errors-only toggle. Opening a
  trace shows a span waterfall: spans nested by parent, bars positioned against
  the whole trace, error spans in red. A trace is capped at 500 spans, and a
  trace whose root lies outside the selected window still renders (its spans
  show as roots). Span attributes are not shown. Only the trace id, which must be
  32 lowercase hex characters, is taken from the request.
- **Live tail.** The Logs tab has a "Live tail" toggle that re-reads the newest
  50 records every 5 seconds. It reads the same Parseable stream as the paged
  view (container output is already collected there), so it adds no Docker
  socket access to the edge. Paging is hidden while it is on.

Charts are server-rendered SVG. A gap in a series means nothing was collected
for that interval; it is never drawn as zero. Node CPU and memory percentages
come from the collector's `system.cpu.utilization` and
`system.memory.utilization` series, which the collector config enables because
Parseable does not map the integer usage series. Nodes without collector data
show "No metrics reported" rather than empty charts. Every query is a fixed
statement built by the adapter; the browser supplies only allowlisted filters
(severity, tab), a length-capped search string and a bounded offset.

## OpenTelemetry ingestion

The collector accepts OTLP on gRPC 4317 and HTTP 4318 and writes logs, metrics
and traces to the `pmcluster-logs`, `pmcluster-metrics` and `pmcluster-traces`
Parseable streams. Applications on the cluster's monitoring network send to
`http://otel-collector:4318` (set `OTEL_EXPORTER_OTLP_ENDPOINT`). Docker
container output is collected automatically from the json-file logs without any
instrumentation. Set `severityNumber` on OTLP log records to get a level;
Parseable shows `UNSPECIFIED` when only `severityText` is sent. The console does
renders traces in the Traces tab.

OTLP port 4318 on the Swarm collector is published on all interfaces and has no
authentication: anything that can reach the host can push telemetry. Firewall it
to your application networks. The standalone stack publishes nothing by default;
add `compose.otlp.yaml` (`docker compose -f compose.yaml -f compose.otlp.yaml up -d`)
to expose 127.0.0.1:4317/4318 on a host that does not run the Swarm collector.

## Enabling a compatible provider

Nothing is enabled automatically. Ship a build of both daemon and edge first:
the daemon adds migration `0026` and the policy/accounting API, and the edge
adds the UI, worker, monitoring network and secret mounts. These are local
source changes until built/published/deployed through the normal release flow.
No Swarm rollout has been performed. For existing Docker/Compose hosts, the
explicit `pmcluster observe` mode provides read-only container inventory and
telemetry without initializing Swarm or managing workloads. That mode is deployed
alongside existing apps on the three project hosts; AI remains disabled.

1. Import an actual provider-issued key using the existing cluster secret
   tooling. Omit the value argument to use its protected interactive prompt:

   ```sh
   pmcluster secret create investigation_provider_key --scope cluster
   ```

2. Apply the updated platform configuration through the normal deployment
   workflow. The edge mounts the imported secret only when it exists, using
   its content-addressed Swarm source and stable
   `/run/secrets/investigation_provider_key` target. Rotation uses the existing
   secret edit/update workflow. No bootstrap-generated password substitutes
   for a vendor-issued key.
3. Open **Observability → Investigation settings** as an administrator. Set
   the provider base URL (including `/v1` when required), exact model ID, daily
   token allowance and per-call input/output allowances. Select
   `max_completion_tokens` or `max_tokens` according to the provider's contract.
   Explicitly consent to sharing redacted evidence and enable investigations.
4. Select a service and time range, inspect its evidence, then submit a question.
   Empty or unavailable evidence cannot trigger an investigation.

Pick the wire protocol in the settings form (`protocol` in the policy): 
`openai` (the default, also for OpenRouter, Groq, Mistral, DeepSeek, Together,
Ollama and vLLM), `anthropic` (Messages API, base `https://api.anthropic.com`)
or `gemini` (native `generateContent`, base
`https://generativelanguage.googleapis.com/v1beta`). Keys travel in the
provider's own header (`Authorization: Bearer`, `x-api-key`, `x-goog-api-key`),
never in a URL. The Anthropic and Gemini paths take `max_tokens` /
`maxOutputTokens` from the policy's output allowance; the "output-limit field"
selector applies to the OpenAI-compatible protocol only. Anthropic has no JSON
mode, so the reply is validated strictly (one surrounding code fence is
tolerated). Azure OpenAI and Bedrock use different authentication and are not
supported yet.

Every protocol uses one nonstreaming request with JSON output, validates every returned citation against supplied evidence,
rejects incomplete/invalid output, and neither follows redirects nor retries a
potentially billable call. Compatible providers must support this contract;
unsupported providers fail explicitly. The API format follows the
[Chat Completions reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
and [JSON output guidance](https://developers.openai.com/api/docs/guides/structured-outputs).
For standalone development, a server-side `INVESTIGATION_API_KEY` environment
value is supported; production uses the mounted secret. Never put keys in the
policy form or a committed configuration file.

## What the model is sent

Raw log lines are never sent. Records are folded into message templates:
numbers, durations, ids, UUIDs, IP addresses and timestamps become placeholders
(`<NUM>`, `<DUR>`, `<ID>`, `<IP>`, `<TS>`), and records that then match share one
evidence item with a count, a first/last timestamp and, when values were
replaced or the record spans several lines, one real example (at most 500
characters). A line repeated five hundred times costs one item. Measured on 400
real container records this reduced about 189 KB to about 13 KB.

The list holds at most 40 items, filled by severity: up to 24 error, 10 warning
and 6 other items, with any unused share passing down. Repeated errors and
warnings rank by count; for info and debug the unusual lines come first, because
a heartbeat repeated thousands of times says little. The input also carries
`stats` (records seen, templates found, and how many were left out), so the
model knows when the list is a sample. If the per-call input allowance is
smaller than the list, items are dropped from the end and counted in `stats`.

Container output has no level of its own and stderr is labelled `ERROR`. The
sender therefore reads an explicit level in the first line (`level=info`,
a tab-separated `info`, a glog `I1018` prefix, `[INFO]`) and uses it in place of
the stream label. A level the application itself set is never overridden.

An investigation reads the newest 200 error records and the newest 200 records
of any level in the window, merged without duplicates, rather than the oldest
page.

## Jobs, budgets and failure behavior

The edge runs one worker and queues at most four jobs, with a 90-second total
job deadline and bounded result count. Dashboard refreshes and result polls
never trigger provider calls. HTTP polling disconnects do not cancel submitted
work; explicit cancellation and shutdown cancel downstream calls.

The daemon atomically reserves the complete configured input plus output
allowance before enqueueing. Input is compacted and constrained conservatively
using UTF-8 byte length plus prompt overhead. These are application token
allowances, not a currency guarantee or a provider billing limit.

Cancel-before-dispatch releases the reservation. Once dispatched, unknown usage,
transport failure or cancellation conservatively accounts the entire allowance.
Authoritative usage reconciles it; usage above the reservation is recorded
without silently clamping it, and reduces remaining capacity. Reservations and
settlements survive restarts. Budgets use the UTC day on which admission was
reserved, not the day on which a delayed result arrives. Outstanding unknown
reservations are not automatically refunded after restart. The UI distinguishes
held tokens and accounted tokens, including conservative unknown usage.

Results are in memory and are lost on edge restart. At most 64 are retained;
old terminal results are cleaned up on later submissions. Durable spend is
unaffected by losing those result bodies. Automated triage, code checkout,
MCP, external notifications and long-term result history remain later work.
Model output only recommends actions; it cannot restart, deploy or execute.

## Verification

Tests use local HTTP fixtures and temporary databases, never paid calls or
production secrets. Coverage includes source authorization, query/time/byte
bounds, malformed versus empty responses, redaction, escaped UI output,
operator accounting denial, atomic concurrent admission, restart persistence,
dispatch replay, cancellation, provider redirect rejection and citation checking.
The native UI-to-accounting-to-provider-to-result flow is exercised end to end
with a fake provider. Desktop, mobile and Arabic/RTL browser checks use synthetic
data. Real Swarm networking and the deployed Parseable schema still require a
development-cluster smoke check before production rollout.

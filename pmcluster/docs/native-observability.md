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
is not labelled as a pending task. Host resource charts, historical OOM counts,
trace analysis and persistent novelty baselines are not implemented in this
version. The page does not invent those values.

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

The provider adapter uses one nonstreaming Chat Completions request with JSON
object output, validates every returned citation against supplied evidence,
rejects incomplete/invalid output, and neither follows redirects nor retries a
potentially billable call. Compatible providers must support this contract;
unsupported providers fail explicitly. The API format follows the
[Chat Completions reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
and [JSON output guidance](https://developers.openai.com/api/docs/guides/structured-outputs).
For standalone development, a server-side `INVESTIGATION_API_KEY` environment
value is supported; production uses the mounted secret. Never put keys in the
policy form or a committed configuration file.

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

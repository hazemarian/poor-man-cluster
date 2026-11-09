# Documentation

The README covers install and first use. Everything deeper lives here.
The design rationale — why the platform looks the way it does — lives in
the [whitepaper](whitepaper.md).

## Where to go for…

| You want to… | Read |
|---|---|
| Write a deploy manifest (the DSL) | [`dsl.md`](dsl.md) |
| Hook CI up (HMAC webhooks) | [`webhook.md`](webhook.md) |
| Run self-hosted GitHub Actions runners on the cluster | [`runners.md`](runners.md) |
| Replace `GH_PAT` in CI with a GitHub App (clone, build, Actions token broker) | [`github-app.md`](github-app.md) |
| Store secrets / configs and reference them from env | [`secrets-and-configs.md`](secrets-and-configs.md) |
| Register or rotate the private-registry (ghcr.io) pull credential | [`registry-credentials.md`](registry-credentials.md) |
| Put a real cert on a customer domain / rotate the main cert | [`tls.md`](tls.md) |
| Change a cluster setting | [`settings.md`](settings.md) |
| Click through the UI (console pages, badges) | [`console.md`](console.md) |
| Run stateful services and databases safely | [`storage-and-databases.md`](storage-and-databases.md) |
| Back up and restore: the volume, the run, retention, restore, what it guarantees | [`backup.md`](backup.md) |
| Answer "is this platform healthy right now?" | [`health.md`](health.md) |
| Wire up nodes, DNS, a load balancer, the firewall | [`network-topology.md`](network-topology.md) |
| Read logs, metrics and traces, or find out where the daemon sends its own telemetry | [the observability chapter](../pmcluster/docs/observability.md) |
| Understand the reconcile loop | [`control-loop-design.md`](control-loop-design.md) |
| Read the security model | [`whitepaper.md`](whitepaper.md) (§13) |
| See what was tested on real hardware (TC1–TC10) | [`test-reports/`](test-reports/README.md) |
| Understand *why* something is the way it is | [`whitepaper.md`](whitepaper.md) |
| See the decisions and what was rejected | [`pmcluster/docs/adr/`](../pmcluster/docs/adr/README.md) |

## Reading orders

**Operator (run a cluster):** `dsl.md` → `secrets-and-configs.md` →
`storage-and-databases.md` → `backup.md` → `health.md` → `network-topology.md` →
`webhook.md` → `github-app.md`.

**New contributor / auditor:** [`whitepaper.md`](whitepaper.md) →
`control-loop-design.md` → `agent.md` (the codebase map, repo root).

**Historian / debugger:** `whitepaper.md` → `test-reports/` →
`CHANGELOG.md` (repo root).

## File map

| File | What it is |
|---|---|
| `dsl.md` | The manifest DSL — every top-level and service field, substitution, injection rules, a complete example |
| `webhook.md` | Webhook sources, request format, signature, GitHub Actions + generic CI examples, troubleshooting |
| `github-app.md` | The GitHub App: installation tokens for clone, build and CI (`POST /api/github/token`), why pulls use the stored PAT instead, the permission ceiling, the workflow snippet, rotation, failure modes, what is not built |
| `runners.md` | Self-hosted GitHub Actions runners: org-level registration into a `selected`-visibility group, one local container per selected worker, the filtered docker socket and its allow list, enable/disable as a full transition, the console panel (node picker, kill, restart), the doctor check |
| `secrets-and-configs.md` | DB-backed secrets/configs: encryption, versioning, scopes, content-addressed swarm mirrors, `env` reference kinds, API |
| `registry-credentials.md` | The private-registry pull credential: where it lives (encrypted store, not a manifest), `registry add/list/remove` semantics, ghcr.io token spec, `--with-registry-auth` propagation, the numbered rotation runbook, failure modes, cadence |
| `tls.md` | ACME vs operator-supplied, in-place main-cert rotation, per-host customer-domain certs |
| `settings.md` | The cluster-settings catalogue: every key the daemon accepts, its default, who sets it, and how a change reaches the swarm |
| `console.md` | The operator console: every page, auth model, status badges |
| `storage-and-databases.md` | Storage nodes and auto-placement, the Path 1 decision, storage matrix, DB strategies, moving/failing over a stack — backups live in `backup.md` |
| `health.md` | `pmcluster doctor`: every health check, what it measures and what it cannot see — the control-plane kit drill, the offsite re-read, version skew across managers |
| `backup.md` | **The** backup chapter: the snapshot-capable volume and its repair, the hourly run (no freeze, and why), what an artifact is, what "verified" means per engine, the three destinations, retention, discovery, restore, the failure modes seen in production, what is not built, and the open questions |
| `network-topology.md` | Traffic planes, topology options, firewall reference, DNS routing |
| `control-loop-design.md` | The reconcile loop as built — passes, drift signals, outage pause, Raft snapshot |
| `test-reports/` | TC1–TC10 field reports from live campaigns |
| `whitepaper.md` | The technical whitepaper — problem, hardware envelope, architecture, design rationale, limitations |

Deeper design docs that live with the code:
[`pmcluster/internal/ARCHITECTURE.md`](../pmcluster/internal/ARCHITECTURE.md),
[`pmcluster/docs/`](../pmcluster/docs/) — the observability chapter (the stack,
the OTLP endpoints, the self-telemetry contract), the service-ops and
console-i18n designs, and `openapi.yaml` — and the
[decision records](../pmcluster/docs/adr/README.md), the choices that constrain
the code with what was rejected and what each one costs.

# Documentation

The README covers install and first use. Everything deeper lives here.
The design rationale — why the platform looks the way it does — lives in
the [whitepaper](whitepaper.md).

## Where to go for…

| You want to… | Read |
|---|---|
| Write a deploy manifest (the DSL) | [`dsl.md`](dsl.md) |
| Hook CI up (HMAC webhooks) | [`webhook.md`](webhook.md) |
| Store secrets / configs and reference them from env | [`secrets-and-configs.md`](secrets-and-configs.md) |
| Put a real cert on a customer domain / rotate the main cert | [`tls.md`](tls.md) |
| Change a cluster setting | [`settings.md`](settings.md) |
| Click through the UI (console pages, badges) | [`console.md`](console.md) |
| Run stateful services and databases safely | [`storage-and-databases.md`](storage-and-databases.md) |
| Wire up nodes, DNS, a load balancer, the firewall | [`network-topology.md`](network-topology.md) |
| Understand the reconcile loop | [`control-loop-design.md`](control-loop-design.md) |
| Use the built-in logs, metrics and traces | [`observability.md`](observability.md) |
| Read the security model | [`whitepaper.md`](whitepaper.md) (§13) |
| See what was tested on real hardware (TC1–TC10) | [`test-reports/`](test-reports/README.md) |
| Understand *why* something is the way it is | [`whitepaper.md`](whitepaper.md) |

## Reading orders

**Operator (run a Swarm cluster):** `dsl.md` → `secrets-and-configs.md` →
`storage-and-databases.md` → `network-topology.md` → `webhook.md`.

**Operator (observability):** [`observability.md`](observability.md).

**New contributor / auditor:** [`whitepaper.md`](whitepaper.md) →
`control-loop-design.md` → `agent.md` (the codebase map, repo root).

**Historian / debugger:** `whitepaper.md` → `test-reports/` →
`CHANGELOG.md` (repo root).

## File map

| File | What it is |
|---|---|
| `dsl.md` | The manifest DSL — every top-level and service field, substitution, injection rules, a complete example |
| `webhook.md` | Webhook sources, request format, signature, GitHub Actions + generic CI examples, troubleshooting |
| `secrets-and-configs.md` | DB-backed secrets/configs: encryption, versioning, scopes, content-addressed swarm mirrors, `env` reference kinds, API |
| `tls.md` | ACME vs operator-supplied, in-place main-cert rotation, per-host customer-domain certs |
| `settings.md` | The 29-key cluster-settings catalogue: defaults, who sets what, how a change reaches the swarm |
| `console.md` | The operator console: every page, auth model, status badges |
| `storage-and-databases.md` | Storage nodes and auto-placement, the Path 1 decision, storage matrix, DB strategies, backup architecture, restore/move/failover |
| `network-topology.md` | Traffic planes, topology options, firewall reference, DNS routing |
| `control-loop-design.md` | The reconcile loop as built — passes, drift signals, outage pause, Raft snapshot |
| `observability.md` | Built-in telemetry pipeline, console, retention, security and limits |
| `test-reports/` | TC1–TC10 field reports from live campaigns |
| `whitepaper.md` | The technical whitepaper — problem, hardware envelope, architecture, design rationale, limitations |

Deeper design docs that live with the code:
[`pmcluster/internal/ARCHITECTURE.md`](../pmcluster/internal/ARCHITECTURE.md),
[`pmcluster/docs/`](../pmcluster/docs/) (openapi.yaml, service-ops, restore,
console-i18n designs).

# pmcluster

Single-binary control plane for the [poor-man-stack](../README.md) Docker Swarm cluster.
It owns cluster bootstrap, application deploys (via a small DSL that translates to
Compose), HMAC-verified webhooks for CI, registry credentials, bootstrap-password
generation, per-host TLS certificates, API tokens, on-demand offen backups, and
structured JSON audit logs. It replaces the old `bin/setup.sh` and Portainer's
GitOps role.

The daemon listens on `127.0.0.1:9090` (host-only). Public access goes through
[`pmcluster-edge`](../README.md) — a Swarm service that publishes
`pmcluster.<domain>` with an operator console and a hardened reverse proxy
(rate limiting, connection shield, IP blocklisting).

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-stack/main/install.sh | bash
```

Privately install with `PREFIX=…` or pin a version with `VERSION=v0.2.27`.
On Linux, `install.sh` also drops a systemd unit at `contrib/systemd/pmcluster.service`.

## Quick start

```bash
docker swarm init                # 1. cluster
pmcluster init                   # 2. bootstrap config (admin name, etc.)
pmcluster cluster up             # 3. infra + edge + observability + backup stacks
pmcluster serve                  # 4. run the daemon (supervise via systemd / brew services)
```

After bootstrap, application deployments arrive via webhook, REST API, or CLI:
`pmcluster deploy ./app.yaml`.

## Build

```bash
cd pmcluster
make build         # → ./bin/pmcluster
./bin/pmcluster --help
./bin/pmcluster version
```

`make help` lists the other targets (`test`, `lint`, `fmt`, `vet`, `tidy`, `clean`, `install`).

## Command surface

| Command | Purpose |
|---------|---------|
| `init` | Bootstrap config (flags: `--admin-name`, `--force` destructive) |
| `serve` | Run the HTTP daemon (REST API + webhook receiver) |
| `cluster up/status/down/update` | Bring the stack up idempotently, show status, tear down, or content-aware re-provision |
| `deploy <manifest.yaml>` | DSL-based deploy (flags: `--app`, `--repo`, `--version`) |
| `stack list/show` | List or inspect deployed stacks |
| `rollback <stack> <rev>` | Roll back to a previous revision |
| `backup create/list` | On-demand backups |
| `webhook add/list/remove` | Manage HMAC webhook sources |
| `user create/list` | Manage operator users (v2 tokens `pmc_<token_id>_<secret>`) |
| `credentials list/show/rotate` | Bootstrap + edge credentials (AES-GCM encrypted) |
| `registry add/list/remove` | Private registry credentials (docker login) |
| `tls hosts add/list/remove` | Per-host TLS certificates for customer domains |
| `node list/join-token` | Node and join-token management |
| `logs` | Cluster/service logs (flags: `--follow`, `--since`, `--tail`, `--all-files`) |
| `version` | Binary version |

## Layout

```
cmd/pmcluster/         entry point
cmd/edge/              pmcluster-edge binary (console + smart proxy)
internal/
  cli/                 Cobra command tree (init, serve, cluster, deploy, tls, …)
  config/              viper-backed config loading
  store/               SQLite (modernc.org/sqlite) + embedded migrations
  auth/                bearer tokens, argon2id hashing
  server/              chi HTTP server wiring
  api/                 REST handlers
  webhook/             webhook receivers (HMAC verification)
  deploy/              deploy orchestration (three entry points → one engine)
  docker/              Docker SDK wrapper
  cluster/             cluster lifecycle: preflight, secrets, networks, configs, stacks
    embeds/            bundled compose YAMLs (infra, edge, observability, backup, Traefik)
  credentials/         AES-GCM-encrypted credential storage
  registry/            registry credential storage
  manifest/            DSL parser + translator → Docker Swarm Compose
  edgeproxy/           rate limit, shield, blocklist, real-IP, proxy (edge)
  ui/                  operator console (gin + HTMX, controllers + templates)
  backup/              backup orchestration
  openobserve/         OpenObserve provisioning (users, tokens, datasources)
  telemetry/           OTLP metrics wiring
  logger/              structured JSON audit logs
  buildinfo/           version/commit/date with VCS fallback
pkg/dsl/               public DSL types
migrations/            *.sql, embedded via //go:embed
e2e/                   end-to-end tests (cluster up, deploy, webhook, edge, otel)
hack/                  dev utilities (e.g. OpenObserve creds)
docs/                  openapi.yaml (REST API spec), restore-design.md
```

## Testing

```bash
make test           # unit tests
make e2e            # docker-in-docker end-to-end suite
```

`e2e/` covers cluster-up against a real Swarm, DSL deploys, webhook delivery,
the edge proxy/console, and OpenObserve provisioning. The same suite runs in
CI on every PR (`PMCLUSTER_E2E_SWARM=1`).

## REST API

The OpenAPI spec lives at [`docs/openapi.yaml`](docs/openapi.yaml) (also mirrored
in the repo root `docs/`). All `/api/*` endpoints require a Bearer token
(`pmc_<token_id>_<secret>`); `/webhook/{source}` is authenticated by HMAC
signature + timestamp. Public access goes through `pmcluster.<domain>`, or
directly against the daemon at `http://127.0.0.1:9090` on the manager.

## Deploy DSL

The DSL schema is documented in [`docs/dsl.md`](../docs/dsl.md) (repo root).
A minimal manifest:

```yaml
app: myapp
env: production
domain: example.com
version: v1.0.0
services:
  web:
    image: ghcr.io/me/myapp:${version}
    replicas: 2
    expose:
      port: 3000
      host: myapp.${domain}
    env:
      NODE_ENV: production
```

`pmcluster` validates, interpolates, and translates this into a full Compose
stack (Traefik labels, networks, secrets, restart/update policies included).

## Related docs

- Top-level [README](../README.md) — architecture, getting started, TLS, backups
- [DSL reference](../docs/dsl.md)
- [Webhook integration guide](../docs/webhook.md)
- [REST API spec](docs/openapi.yaml)
- [Restore design](docs/restore-design.md)
- [RFC v2 (issue #1)](https://github.com/hazemarian/poor-man-stack/issues/1)
---
name: poor-man-cluster-deploy
description: Use when deploying to, operating or troubleshooting a pmcluster cluster. Deploy workflow, remote/API deploys, upgrades, incidents.
version: 3.0.0
author: Hazem Arian
license: MIT
platforms: [linux, macos]
metadata:
  hermes:
    tags: [pmcluster, deploy, swarm, operations, webhooks, tls, troubleshooting]
---

# Deploying and operating a pmcluster cluster

The operator playbook: get a stack onto the cluster, keep it there, and diagnose it when it is not.
Reference chapters in `docs/` are the authority on syntax and settings; this skill is the workflow.

## Where the truth lives

| question | authority |
|---|---|
| Manifest syntax, every field | [`docs/dsl.md`](../../docs/dsl.md) |
| Settings (`cluster set`, app defaults) | [`docs/settings.md`](../../docs/settings.md) |
| Configs and secrets | [`docs/secrets-and-configs.md`](../../docs/secrets-and-configs.md) |
| Health checks and what `doctor` can/cannot see | [`docs/health.md`](../../docs/health.md) |
| TLS and site certificates | [`docs/tls.md`](../../docs/tls.md) |
| Webhooks and HMAC sources | [`docs/webhook.md`](../../docs/webhook.md) |
| Storage, volumes, databases | [`docs/storage-and-databases.md`](../../docs/storage-and-databases.md) |
| Console, i18n, auth | [`docs/console.md`](../../docs/console.md) |
| Architecture, why it is built this way | [`agent.md`](../../agent.md), [`docs/whitepaper.md`](../../docs/whitepaper.md) |

The DSL section that used to live in this skill is now `references/dsl-examples.md` (examples only) —
`docs/dsl.md` is canonical, and it is the one that gets updated.

## The deploy, end to end

```bash
pmcluster deploy <manifest.yaml>     # parse → interpolate → validate → translate → record → apply
pmcluster stack show <name>          # current revision, recent revisions, last error
pmcluster stack list                 # here should be one line per stack, in sync or not
```

A deploy is **per `depends_on` level**: services are topologically sorted, each level is deployed with
`docker stack deploy` and waited for, then the next starts. One drift-prune pass runs at the end.

What that means when something goes wrong:

- **A failed deploy keeps its revision.** Retries of an unchanged stack reuse the revision and collapse
  identical errors into one entry with a count — a wall of "deploy failed" lines is usually one failure.
- **The wait per level is `health_timeout`** (default 2m). A timeout names the services that are not
  healthy and quotes the failed task's exit code:
  `waiting for web to become healthy — web: 0/1 running, task failed (exit 1)`.
- **Failures back off** 2m → 30m, cleared by a success or a new revision. A stack that failed twice in a
  row will not retry on the next tick, and that is intended.

## Incidents, in the order that resolves them

1. `pmcluster doctor` — quorum, mirror, secrets, drills, offsite, disk, console telemetry, shell sessions.
2. `pmcluster stack show <name>` — the current revision and its recorded error.
3. `pmcluster service logs <stack>_<svc> --tail 100` — the app's own words.
4. `docker service ps <stack>_<svc>` — task states and exit codes, if the above disagreed.

A worker cannot read cluster state; run steps 1–2 on a manager. `pmcluster service` and `logs` work from
any node.

## Reference files

| file | contents |
|---|---|
| `references/getting-started.md` | Prerequisites, install, first cluster, first app. |
| `references/console-and-sso.md` | Operator console, Traefik dashboard, SSO wiring. |
| `references/auth.md` | API tokens, credentials, audit log. |
| `references/remote-and-api.md` | Remote CLI mode, REST deploys, CI/CD webhook setup. |
| `references/deploy-and-rollback.md` | The deploy workflow and rollback semantics. |
| `references/tls-and-registries.md` | Site certificates, private registry credentials. |
| `references/cluster-operations.md` | Pre-deploy backups, cluster management, multi-node upgrades. |
| `references/troubleshooting.md` | Symptoms and fixes, MCP surface verification, a chi routing trap. |
| `references/dsl-examples.md` | Manifest examples (canonical syntax: `docs/dsl.md`). |

## Observability: Parseable, not OpenObserve

The `observability` stack deploys **Parseable** (`quay.io/parseablehq/parseable`) plus the OTel collector,
served at `observ.<domain>` behind auto-auth, so an operator arriving from the console is not asked to log
in again. Login is the managed admin identity: `admin@<domain>` with the
`observability_admin_password` secret, read as `P_USERNAME`/`P_PASSWORD`.

OpenObserve was the previous backend and is gone from the stack; `docs/observability.md` and
`pmcluster/docs/adr/0002-telemetry-backend.md` record why. This skill said OpenObserve for a year after the
switch — if you find another doc that still does, it is stale in the same way.

## This file used to be 833 lines

Split in 2026-11: it carried a full copy of the manifest reference, an OpenObserve chapter that had been
false for a year, and every cluster-management command in one body. Sections moved verbatim into
`references/`, the two stale ones were replaced with pointers to the docs that own them.

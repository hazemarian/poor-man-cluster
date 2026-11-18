<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Prerequisites

Before any deploy operation, verify the environment:

```bash
# Is Docker running and Swarm active?
docker info --format '{{.Swarm.LocalNodeState}}'   # must be "active"

# Is pmcluster installed and initialized?
pmcluster version
pmcluster cluster status
```

If pmcluster is not installed:

```bash
# Basic install:
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | bash
pmcluster init

# With private registry credentials (e.g. GHCR):
curl -fsSL https://raw.githubusercontent.com/hazemarian/poor-man-cluster/main/install.sh | \
  PMCLUSTER_REGISTRY="ghcr.io=my-username=ghp_abc123" bash
pmcluster init
```

Install env vars: `VERSION` (pin release), `PREFIX` (install path), `PMCLUSTER_USER` (systemd user), `PMCLUSTER_REGISTRY` (comma-separated `host=user=token` entries).

If the cluster is not up, run the **interactive setup wizard** or bring it up with flags:

```bash
# Interactive wizard (recommended) — prompts for domain, TLS, admin user, SSO, node hostname, etc.:
pmcluster setup

# Non-interactive: flags for every prompt:
pmcluster setup --domain=example.com --acme-email=you@host \
  --traefik-admin-user=admin --sso-enabled --sso-client-id=... --sso-client-secret=...
```

The wizard persists settings, then runs `cluster up` (fresh install) or `cluster update` (existing cluster).

Alternatively, use `cluster up` directly with flags:

```bash
pmcluster cluster up --domain=<your-domain> --openobserve-email=admin@<your-domain> --cert=<cert.pem> --key=<key.pem>
# OR with Let's Encrypt (HTTP-01; DNS must point here and port 80 reachable):
pmcluster cluster up --domain=<your-domain> --openobserve-email=admin@<your-domain> --acme-email=<you@host>
```

> **`cluster up` is init-only.** It refuses to run on an already-initialised cluster — use `cluster update` instead. When no data flags are supplied, it falls back to the interactive setup wizard.

`cluster up` deploys four stacks: `infra`, `edge`, `observability`, `backup` (plus `sso` when enabled). It also mints the `edge_admin` console password and the `edge` daemon API token.

The daemon runs as a systemd service. `cluster up`, `cluster update`, and `pmcluster join` install `/etc/systemd/system/pmcluster.service` and (re)start it — no manual unit management. `serve` is leader-aware: it serves on the Swarm leader and stands by (15s poll) on non-leader managers; on promotion it keeps the store it has been mirroring and becomes the writer — nothing is restored or copied (ADR 0005), and a stale mirror is served with its age logged.

```bash
pmcluster serve   # foreground fallback (non-Linux hosts / manual runs)
```

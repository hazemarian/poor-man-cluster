# The operator console

The console is a web UI (gin + HTMX) served by **`pmcluster-edge`** on the
`pmcluster.<domain>` origin. It is up as soon as DNS resolves after
`cluster up` — no separate install.

```text
https://pmcluster.<your-domain>          # the console (/web/* routes)
```

For a new cluster, `pmcluster setup` chooses the console's signed cookie
session, so the `/web/login` and `/web/logout` routes work on a normal non-SSO
cluster. A cluster without the `edge_login_disabled` setting keeps the legacy
Traefik Basic Auth gate; set `edge_login_disabled=false` to switch to native
sessions. `EDGE_LOGIN_DISABLED=true` selects the Basic Auth gate and stateless
synthetic-admin mode; a browser-managed
Basic Auth credential cannot be cleared by a server-side logout button. SSO
keeps the edge session disabled and uses the `/oauth2/sign_out` link. In
standalone mode the first admin account is created on first run, with RBAC
roles admin > operator > viewer. See [the whitepaper](whitepaper.md) for the
auth model.

## The pages

- **Stacks** — deployed stacks and revision history, deploy new manifests,
  roll back, move a stateful stack to another node, acknowledge a storage
  failover; platform stacks (infra/edge/observability/backup/sso) are hidden
  — they are not user workloads.
- **Services** — live replica health, task/crash history, log tailing,
  restart, exec, and a browser terminal per service; customer app services
  only.
- **Platform** — a read-only page listing the platform-managed services
  (stacks, replica health, node placement), separate from customer apps.
- **Overview** — cluster info plus the swarm nodes table with a per-node
  **storage pill** and promote/demote buttons (the storage-node lifecycle).
- **Webhooks** — create/list/remove CI webhook sources (secrets shown once) +
  delivery history.
- **API keys** — issue and revoke bearer tokens for other users/CI systems
  (operator-tier).
- **TLS** — rotate the cluster's own (main) certificate and manage per-host
  certificates for customer domains, with expiry warnings
  ([`tls.md`](tls.md)).
- **Backups** — trigger snapshots, browse archive contents, restore, and view
  the audit log (archives other nodes uploaded to the store included).
- **Users** — manage console accounts with RBAC roles (admin > operator >
  viewer); admins only. Hidden from nav when behind SSO/admin-auth.
- **Settings** — cluster settings ([`settings.md`](settings.md)),
  cluster-scope configs/secrets with **Apply to swarm**, and the read-only
  rendered platform configs (operator/admin only — they embed root
  credentials).
- **Preferences** — theme plus the brand palette picker (six accent palettes,
  persisted before first paint).
- **External** — nav links to `https://observ.<domain>` (Parseable) and
  `https://traefik.<domain>/dashboard/` (Traefik dashboard).

## Status badges

Every stack has a public, no-auth status badge endpoint at
`https://pmcluster.<domain>/api/public/badge/<stack>` (flat SVG: healthy /
in progress / degraded / error / unknown / **failover**) that reads the
control loop's `stack_status` DB snapshot; an unacknowledged storage failover
dominates the badge until `stack ack` or a move-back. Paste it into any
GitHub README — e.g.
`![stack services](https://pmcluster.<domain>/api/public/badge/<stack>/services)`
renders one badge with a segment per service. The console's stack page shows
the copyable markdown (single + combined), and `pmcluster stack badge
<stack>` prints it.

## What the console does NOT cover

Service-level operations beyond the whitelisted set are CLI territory —
`pmcluster service list|ps|tasks|logs|restart|exec` and the native
`pmcluster observability status|logs|metrics|traces|trace` reads (locally or
off-node via `--api-url`). There is no Portainer; the console and CLI cover its
role. Raw `docker` passthrough is deliberately absent.

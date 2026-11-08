# The operator console

The console is a web UI (gin + HTMX) served by **`pmcluster-edge`** on the
`pmcluster.<domain>` origin. It is up as soon as DNS resolves after
`cluster up` — no separate install.

```text
https://pmcluster.<your-domain>          # the console (/web/* routes)
```

Authentication: the Traefik `admin-auth` (htpasswd) or `sso-auth`
(forwardAuth) gate owns access — when gated, the console's own login page is
disabled (`EDGE_LOGIN_DISABLED=true`) and it runs as a synthetic admin
session. In standalone mode (no gate) the console authenticates its own users
(admin > operator > viewer RBAC) and the first admin account is created on
first run. See [the whitepaper](whitepaper.md) for the auth
model.

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
- **External** — nav links to `https://observ.<domain>` (OpenObserve) and
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

## Loading states

Every console request shows that it is in flight. The cues are CSS-only,
driven by the htmx `htmx-request` class and the theme/palette tokens — no
JavaScript, no per-request state to keep in sync:

- **Page progress bar.** A 2px `--accent` bar under the topbar animates for as
  long as any htmx request is running (`body:has(.htmx-request) .inflight`). It
  is fixed and transparent at rest, so it can never shift the layout.
- **Panel reload.** A panel that replaces itself (the logs panel, the traces
  panel, the observability summary) keeps its current content and dims it under
  a spinner overlay (`.load-overlay`, activated by `hx-indicator`). Reading the
  old numbers while the new ones load beats a blank panel — and the operator can
  tell the difference between "reloading" and "broken".
- **Skeleton on first paint.** The shell carries no data, so the first fragment
  lands after a round trip. Until then `#view` renders a page skeleton (title,
  stat strip, panel bodies) at roughly the height of the content that is
  coming, instead of an empty column.
- **Busy buttons.** Every `hx-post` disables its submit control for the duration
  of the request (`hx-disabled-elt`), so a slow action cannot be fired twice,
  and takes a busy look (`cursor: progress`, dimmed fill, spinning leading
  icon) rather than a dead one.

The bar and the overlay wait 180–250ms before appearing: a swap that lands in
50ms should not flash a placeholder at the operator. The rules live in the
`loading states` section of `internal/ui/views/static/components.css`; colour
comes from `--accent`, `--bg-card` and `--text-secondary`, so all twelve
theme × palette combinations are covered without a rule per palette.

```text
htmx-request  →  .inflight            (page-wide bar)
              →  .load-overlay        (the panel named by hx-indicator)
              →  [disabled] on submit (hx-disabled-elt)
```

## What the console does NOT cover

Service-level operations beyond the whitelisted set are CLI territory —
`pmcluster service list|ps|tasks|logs|restart|exec` (locally or off-node
via `--api-url`). There is no Portainer; the console + `pmcluster service`
cover its role. Raw `docker` passthrough is deliberately absent.

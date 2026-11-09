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
- **Inventory** — every stored config and secret in two searchable tabs:
  **Stacks** (service-scope rows, by stack) and **Platform** (cluster-scope
  rows). Both tabs can move a row to another scope (retag), edit or delete it,
  and create a new one in the tab's own scope.
- **Settings** — cluster settings ([`settings.md`](settings.md)) and the
  **Apply to swarm** button. The cluster-scope configs and secrets are listed
  on Inventory → Platform now; Settings keeps the create/edit forms and links
  there. The read-only rendered platform configs open from the Platform tab
  (operator/admin only — they embed root credentials).
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

## Loading states

A page is a shell plus fragments, and every fragment reads the daemon. Two
consequences are visible in the console: the reads a page needs are issued at the
same time, and the page shows that it is waiting.

### One wave per page

Every independent read goes out in one wave rather than one after another, so a
page costs its slowest read instead of the sum of its panels. Two dependencies
are real and stay explicit: the evidence read waits for the inventory it resolves
service names against (and still fails closed on that error), and a node card is
projected after the inventory resolves. The distinct-service-name read runs only
on the observability logs tab, the one place its answer is rendered.

Measured against the live API through a local console, cold, before and after
that change:

| page / fragment | before | after |
| --- | --- | --- |
| `/web/overview` | 1.09–1.29 s | 0.42–0.60 s |
| `/web/observability` (cold) | 4.72 s | 1.83–2.43 s |
| — `observability/evidence` | 1.50 s | 0.45–0.59 s |
| — `observability/traces` | 1.29 s | 0.64–0.74 s |
| `/web/stacks` | 0.50–0.65 s | 0.13–0.16 s |
| `observability/summary` | 1.45–1.60 s | 1.45–1.60 s (unchanged) |

The observability page's 4.72 s was three reads run in series — 1.88 s + 1.50 s +
1.29 s = 4.67 s of backend work inside a page that rendered in 4.72 s, which is
why it read as "the page is slow" rather than "one panel is slow".

`summary` is the exception, and it is now the page's floor: it did not improve,
and the observability page cannot render faster than it until that query itself
gets cheaper. Ranges rather than single figures because a live cluster's link
moves; compare builds inside one window, never one page load against another
hours later. (`pmcluster/docs/observability.md` carries the complementary
measurement — the same pages against a stub that charges every call a fixed
250 ms, which isolates how many waves each page used to pay for.)

### The cues

Every cue is CSS-only, driven by the htmx `htmx-request` class and the
theme/palette tokens — no JavaScript, no per-request state to keep in sync:

- **Page progress bar.** A 2px `--accent` bar under the topbar animates for as
  long as any htmx request is running (`body:has(.htmx-request) .inflight`).
- **Panel reload.** A panel that replaces itself keeps its current content and
  dims it under a spinner overlay (`.load-overlay`, activated by `hx-indicator`).
  Reading the old numbers while the new ones load beats a blank panel — and the
  operator can tell "reloading" from "broken". Three panels carry one today: the
  observability summary's refresh, the evidence form and the traces form.
- **Skeleton on first paint.** The shell carries no data, so the first fragment
  lands after a round trip. Until then `#view` renders a page skeleton (title,
  stat strip, panel bodies) instead of an empty column.
- **Busy buttons.** A submitting control is disabled for the duration of the
  request (`hx-disabled-elt`; 54 `hx-post` triggers and 71 declarations of it, so
  some forms mark both the form and its buttons) and takes a busy look
  (`cursor: progress`, dimmed fill, spinning leading icon) rather than a dead
  one.

**None of it occupies layout space.** The bar is `position: fixed`, the overlay
is `position: absolute` inside its panel, the spinner has a fixed 16px box, and
the skeleton reserves `min-height: 62vh` so the page body does not jump when the
fragment lands. An indicator appearing must never move the control out from under
the operator's cursor. The bar and the overlay also wait 180–250 ms before
showing: a swap that lands in 50 ms should not flash a placeholder.

The rules live in the `loading states` section of
`internal/ui/views/static/components.css`; colour comes from `--accent`,
`--bg-card` and `--text-secondary`, so all twelve theme × palette combinations
are covered without a rule per palette.

```text
htmx-request  →  .inflight            (page-wide bar)
              →  .load-overlay        (the panel named by hx-indicator)
              →  [disabled] on submit (hx-disabled-elt)
```

### What was measured, and what was not

The table above is wall-clock rendering of each page, before and after — the
thing the change was for. It says nothing about the indicators.

The indicators were verified only in part, and this document says so rather than
implying a browser pass that did not happen. The hooks exist in the templates and
the stylesheet — three `hx-indicator` targets each with a matching
`.load-overlay`, `hx-disabled-elt` on every template that posts, the skeleton in
the shell — and the CSS relies on `:has()`, so it needs a browser that supports
it (current Safari, Chrome, Firefox; not an old embedded webview). What was not
done is a browser-by-browser walk asserting that the bar appears for each kind of
request, that the skeleton's reserved height matches the content that replaces
it, or that an overlay clears correctly when a swap comes back as an error.
Treat them as shipped and partly verified, not as measured.

## Static assets and caching

The console's stylesheets, scripts, favicon and fonts are embedded in the
binary and served under `/web/static/`. Every URL is content-addressed: the
templates never write a bare path, they call `{{static "components.css"}}`,
which expands to `/web/static/components.css?v=<hash of the served bytes>`.

That version is the whole cache story. A matching `?v=` is answered with
`Cache-Control: public, max-age=31536000, immutable` — a year, no
revalidation, safe because a changed file is a different URL. A bare URL, or
one carrying a version this build no longer has, is answered with
`Cache-Control: no-cache` and the current bytes: a stale hash is a cache
buster, never a 404 and never an hour of the previous deploy's CSS. Every
response also carries the content's `ETag`, so an ordinary revalidation is a
304.

The hash is taken over the bytes actually served, not the file on disk.
`fonts.css` embeds `@font-face` URLs for the woff2 files; those URLs are
rewritten with the font's own version when the assets are loaded, and the
stylesheet's version is computed over the rewritten body. A font change
therefore busts the font URL, the stylesheet URL and every page that links it.

`internal/ui/views/assets.go` owns the table (built once at startup, so the
daemon and the edge render identical URLs from the same embedded FS);
`StaticHandler` in the same file owns the policy. The `static` helper is
registered in `render.go` and is the only supported way to reference an asset —
a test fails if any template hardcodes `/web/static/`.

## What the console does NOT cover

Service-level operations beyond the whitelisted set are CLI territory —
`pmcluster service list|ps|tasks|logs|restart|exec` (locally or off-node
via `--api-url`). There is no Portainer; the console + `pmcluster service`
cover its role. Raw `docker` passthrough is deliberately absent.

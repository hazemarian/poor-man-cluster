<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Operator Console

`https://pmcluster.<domain>` is a gin + HTMX operator console for stacks (deploy/rollback/delete), webhooks, API keys, per-host TLS, backups, and settings. The stacks list has a **Sync** button per stack that re-deploys from the stored manifest (skipping no-ops when rendered content is unchanged) and a **Delete** button (with confirmation).

- **Login:** user `admin`, password from `pmcluster credentials show edge_admin`.
- It talks to the daemon with a dedicated `edge` API token; you don't manage that token manually.
- **Behind SSO:** When SSO is enabled, the console's own password login is disabled (`EDGE_LOGIN_DISABLED=true`). Users authenticate through the Traefik SSO gate instead.

Prefer the console for interactive work; prefer the CLI/API for automation.


## Traefik Dashboard

The Traefik dashboard is at `https://traefik.<domain>/dashboard/`. The router rule is `Host(...) && (PathPrefix(/api) || PathPrefix(/dashboard))`. Without SSO it is gated by `admin-auth` (htpasswd basicAuth); with SSO enabled it is gated by `sso-auth` (forwardAuth through oauth2-proxy).


## SSO (Single Sign-On)

When enabled during `pmcluster setup`, the `sso` stack deploys [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/) at `sso.<domain>`. GitHub OAuth with optional org restriction.

- **Callback URL:** `https://sso.<domain>/oauth2/callback`
- **Traefik middleware switch:** admin-auth (htpasswd basicAuth) → sso-auth (forwardAuth through oauth2-proxy). The `traefik_dashboard` credential is no longer used for gating.
- **oauth2-proxy env vars (critical — plural forms required):**
  - `OAUTH2_PROXY_COOKIE_DOMAINS` (plural — singular `COOKIE_DOMAIN` is silently ignored in v7)
  - `OAUTH2_PROXY_WHITELIST_DOMAINS` (plural — singular is silently ignored in v7)
  - `OAUTH2_PROXY_REVERSE_PROXY: "true"` (requires `trusted-proxy-ip` / `TRUSTED_PROXY_CIDRS` for hardening)
- **Console behind SSO:** `EDGE_LOGIN_DISABLED=true` — the edge console's own login page is bypassed; Traefik's SSO gate handles identity. Console is at `https://pmcluster.<domain>/web/`.

### Enabling SSO on an existing cluster

```bash
pmcluster setup --sso-enabled --sso-client-id=<id> --sso-client-secret=<secret> [--sso-github-org=<org>]
# then:
pmcluster cluster update
```

The `sso_cookie_secret` credential is minted automatically by credential bootstrap. oauth2-proxy needs a base64-encoded 32+ byte cookie secret.

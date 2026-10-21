# Cluster settings catalogue

Editable settings are a fixed **29-key allowlist**. Values persist in the
store; the swarm side picks them up on the next `cluster update` (or the
console's **Apply to swarm**). Unknown keys are rejected atomically.

```bash
pmcluster cluster settings             # list all (KEY/VALUE; secret-typed keys masked)
pmcluster cluster settings get <key>
pmcluster cluster settings set key=value ...
```

REST: `GET/PUT /api/cluster/settings` (secret-ish values masked for non-admin
bearers). Console: the **Settings** page.

| Key | Default | Who sets it | What it controls |
|---|---|---|---|
| `volume_root` | `/var/stack/data` | setup wizard (`--volume-root`) / operator | the host root every container volume is forced under |
| `storage_nodes` | leader's hostname (auto-seeded) | operator, `join --storage-node`, `node promote/demote` | comma-separated storage node hostnames; stateful stacks round-robin across them; the current leader is always re-added, a former leader is removed, and labeled-but-unlisted nodes are adopted |
| `storage_failover` | `false` | setup prompt (only when offsite S3 is configured) / operator | automatic storage failover in the control loop (requires `backup_s3_*`; refuses to enable without it) |
| `backup_cron` | `0 * * * *` (hourly) | operator | the offen agents' schedule (5-field cron) |
| `backup_retention_days` | `15` | operator | prune window for backup rows + archives (local and remote) |
| `backup_all_nodes` | `false` | operator | run the volume agent on **every** node instead of storage nodes only |
| `backup_store_on` | `""` (= worker/auto) | setup wizard / operator | where the in-cluster SeaweedFS store runs: `leader`, or a non-storage non-platform worker (auto) |
| `backup_s3_endpoint` / `_bucket` / `_access_key` / `_secret_key` / `_region` | unset (`_region` defaults `auto`) | setup wizard / operator | the offsite S3/R2 destination — when set, agents double-write every archive offsite |
| `reconcile_interval` | `60` (seconds) | operator | the control loop's safety tick; `0` disables the loop |
| `edge_login_disabled` | `true` in swarm deployments | setup wizard / edge stack | hides the console's own login (the Traefik admin-auth/SSO gate is the authenticator); `false` keeps password login (standalone) |
| `sso_enabled` / `sso_provider` / `sso_client_id` / `sso_client_secret` / `sso_github_org` / `sso_github_repos` / `sso_cookie_expire` | off / `github` / unset / unset / unset / unset / `1h` | setup wizard (`--sso-*`) / operator | GitHub SSO via oauth2-proxy: org (and optional repo) restriction, tightened 1h default session |
| `domain` | — | `cluster up` / setup wizard | the cluster's base domain |
| `observability_admin_email` | — | `cluster up` (`--observability-admin-email`) | the observability admin login |
| `traefik_admin_user` | `admin` | setup wizard / `cluster up` | the basic-auth username for the Traefik gate |
| `observability_logs_retention_days` / `observability_metrics_retention_days` / `observability_traces_retention_days` | `7` each | operator | per-stream retention in the observability backend (unbounded retention is how a cluster silently grows to hundreds of GB) |
| `log_level` | `info` | operator | daemon verbosity (`debug`/`info`/`warn`/`error`); applied live on save |

## Not in the allowlist (managed elsewhere)

| Key | Managed by | Why |
|---|---|---|
| `tls_mode` / `tls_cert_path` / `tls_key_path` / `tls_acme_email` | the setup wizard / `cluster up --acme-email\|--cert\|--key`; switching modes needs `--force-tls-mode` | TLS lifecycle is a bring-up concern, not a runtime setting |
| `storage_leader` | written by `cluster update` | remembers which hostname was promoted because it held leadership |
| `stack_pin_<stack>` | written only by `stack move` | outranks the `storage_nodes` round-robin |
| `swarm_id` | written by `cluster update` | wipe detection |

## How a setting change reaches the swarm

1. `cluster settings set key=value` (CLI) or the console's Settings page —
   the value is persisted in the store **only**.
2. The next `cluster update` (or the console's **Apply to swarm** button,
   `POST /api/update`) re-renders the affected platform configs and
   re-deploys what changed.

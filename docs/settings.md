# Cluster settings catalogue

Editable settings are a fixed allowlist (the code's list in
`internal/settings/local.go` is authoritative, not this page). Values persist in the
store; the swarm side picks them up on the next `cluster update` (or the
console's **Apply to swarm**). Unknown keys are rejected atomically, and so are
invalid values — nothing is written when any key or value in a request is
refused.

```bash
pmcluster cluster settings             # list all (KEY/VALUE; secret-typed keys masked)
pmcluster cluster settings --show-secrets   # ...and print them in cleartext
pmcluster cluster settings get <key>
pmcluster cluster settings set key=value ...
```

Secret-typed means any key whose name contains `secret`, `password`, `key`, `token`
or `credential` (`internal/settings/mask.go`, one policy for the CLI, the API and
the console). The CLI masks them by default **even though it runs as root on the
box**: root can read the store anyway, but stdout does not stay on the box — it
goes into shell history, scrollback and any transcript it is pasted into, which is
how the offsite keys were exposed once already. `--show-secrets` is the explicit
way to see a value, and it says so when it masks.

REST: `GET/PUT /api/cluster/settings` (secret-ish values masked for non-admin
bearers). Console: the **Settings** page.

| Key | Default | Who sets it | What it controls |
|---|---|---|---|
| `volume_root` | `/var/stack/data` | setup wizard (`--volume-root`) / operator | the host root every container volume is forced under |
| `storage_nodes` | leader's hostname (auto-seeded) | operator, `join --storage-node`, `node promote/demote` | comma-separated storage node hostnames; stateful stacks round-robin across them; the current leader is always re-added, a former leader is removed, and labeled-but-unlisted managers are adopted. **Entries must be managers**: a worker is pruned on the next `cluster update` (it cannot read settings, credentials or the stack list, so its backup agent would archive and ship nowhere) |
| `storage_failover` | `false` | setup prompt (only when offsite S3 is configured) / operator | automatic storage failover in the control loop (requires `backup_s3_*`; refuses to enable without it) |
| `backup_enabled` | `true` | operator | master switch. The timer keeps firing; the agent asks the control plane and exits without freezing when this is off |
| `backup_cron` | `0 * * * *` (hourly) | operator | when a run is due (5-field cron; `*`, numbers and comma lists only). Evaluated by the daemon, not by the unit file |
| `backup_retention_days` | `3` | operator | prune window for backup rows + archives (local, store and offsite): 48 h dense, then one a day to this age |
| `backup_keep_local` | `dense` | operator | how much history a node keeps on its **own** disk: `dense` keeps the whole retention window (48 h dense + the sparse tail), `minimal` keeps one artifact per stack — the newest, deleted-down only *after* every configured destination confirmed the object. The copies that matter are in the store and the bucket; this is the one that costs disk on the machine running the apps |
| `backup_store_on` | `""` (= worker/auto) | setup wizard / operator | where the in-cluster SeaweedFS store runs: `leader`, or a non-storage non-platform worker (auto) |
| `backup_s3_endpoint` / `_bucket` / `_access_key` / `_secret_key` / `_region` | unset (`_region` defaults `auto`) | setup wizard / operator | the offsite S3/R2 destination — when set, agents double-write every archive offsite |
| `reconcile_interval` | `60` (seconds) | operator | the control loop's safety tick; `0` disables the loop |
| `edge_login_disabled` | `true` in swarm deployments | setup wizard / edge stack | hides the console's own login (the Traefik admin-auth/SSO gate is the authenticator); `false` keeps password login (standalone) |
| `sso_enabled` / `sso_provider` / `sso_client_id` / `sso_client_secret` / `sso_github_org` / `sso_github_repos` / `sso_cookie_expire` | off / `github` / unset / unset / unset / unset / `1h` | setup wizard (`--sso-*`) / operator | GitHub SSO via oauth2-proxy: org (and optional repo) restriction, tightened 1h default session |
| `domain` | — | `cluster up` / setup wizard | the cluster's base domain |
| `observability_admin_email` | — | `cluster up` (`--observability-admin-email`) | the observability admin login |
| `traefik_admin_user` | `admin` | setup wizard / `cluster up` | the basic-auth username for the Traefik gate |
| `observability_logs_retention_days` / `observability_metrics_retention_days` / `observability_traces_retention_days` | `7` each | operator | per-stream retention in the observability backend (unbounded retention is how a cluster silently grows to hundreds of GB) |
| `log_level` | `info` | operator | **the platform's own** verbosity: `debug`, `info`, `warn` or `error` (`warning` is an alias of `warn`). Applied live when saved, read at daemon startup, and rendered into the platform stack as `PMCLUSTER_LOG_LEVEL` — the env var the daemon and the edge both read. `debug` adds the full rendered compose per deploy level; `info` logs every deploy step with the service names |
| `app_log_level` | `warn` | operator | **the default for deployed apps**, not the platform: every service of every app stack gets `LOG_LEVEL` = its manifest's `log_level`, else this, else `warn`. Same four values and the same `warning` alias. It never overrides a `LOG_LEVEL` a service declares in its own `env:`; it reaches running stacks on their next deploy or sync (see [dsl.md](dsl.md#what-the-translator-injects)) |

## Logging: two settings, one env var

Two settings control logging, and they are deliberately separate — one knob
would make "quiet the daemon" and "quiet my apps" the same action.

- **`log_level` — the platform.** The daemon reads it at startup, re-levels
  immediately when the console or CLI saves it, and `cluster update` renders it
  into the platform stack as `PMCLUSTER_LOG_LEVEL`. Both pmcluster binaries (the
  daemon and the edge) read that env var through the same helper, so a container
  and a host process configured together log at the same level. Default `info`;
  `debug` is what makes the deploy pipeline print the rendered compose of each
  level.
- **`app_log_level` — deployed apps.** It is the default value of the
  `LOG_LEVEL` env var pmcluster injects into every service of every app stack.
  Precedence: the manifest's `log_level`, then this setting, then `warn`. A
  service that declares `LOG_LEVEL` in its own `env:` keeps it. Default `warn`,
  because most apps log at INFO for every request.

Accepted values are `debug`, `info`, `warn` and `error` for both;
`warning` is accepted as an alias of `warn` and stored as `warn`. An unknown
value is refused when the setting is saved, and an unknown
`PMCLUSTER_LOG_LEVEL` in a process environment does not stop the process: it
logs one warning line naming the rejected value and runs at `info`.

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

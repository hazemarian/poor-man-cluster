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

## The console Settings page

The page (`/web/settings`, also reachable at `/web/settings/cluster`) is six
groups. Each one carries a heading, one line of purpose, and one line naming what
it needs — a prerequisite read before the save that fails on it, not after:

| Group | What is in it | What it needs |
|---|---|---|
| **Connection** | this console's API URL and token state | nothing on the cluster: it is stored in the console's own store |
| **GitHub App** | app id, installation id, key fingerprint, the set/rotate form, probe targets, last probe and its age, granted permissions | the App installed on the organisation (Contents and Packages: Read) |
| **Identity and sign-in** | domain, SSO provider/client/org/repos/session, the edge login switch, the Traefik admin user | a provider the daemon knows plus a client id and secret |
| **Self-hosted CI runners** | the enable switch **first**, then org, runner group, memory, socket proxy and path, allow-leader, and a read-only summary of what is registered | a configured GitHub App, `github_runner_org`, and at least one worker selected on the Runners page |
| **Backups** | schedule and retention | a backup store; the bucket and its credentials are daemon-environment settings, not page fields |
| **Advanced** | volume root, log level, admin e-mail, and the rendered platform configs (collapsed) | the admin role |

Identity, CI runners, Backups and Advanced are **one form** with **one sticky
save bar** that says whether anything is dirty and what a save covers. There is
no autosave. Saving stores the values; **Apply to swarm** is what re-renders the
platform stacks from them.

### Where node selection lives

`github_runner_nodes` is not a field on this page: it is a picker, one row per
node with its role, current runner state and free memory. The picker lives on
**`/web/runners`**, one click from the CI runners heading (**Pick worker nodes**)
and again under its summary. The settings form carries the current value in a
hidden field, so saving it cannot clear a choice made on the picker page. The CI
runners group shows the resulting selection and one state pill per runner; it
never renders the picker itself.

### When a save is refused

The daemon's message is the banner **body** on the page — never a title with the
reason behind a "Show details" disclosure — and the group whose fields the
message names is marked, along with the field when it names one:

| Message names | Marked |
|---|---|
| `github_runner_org` | CI runners group + the Organisation field |
| `github_runner_nodes` | CI runners group + its worker-selection summary (with the link to the picker) |
| `backup_retention_days` | Backups group + the retention field |
| anything about permissions, `403`, or "self-hosted runners" | GitHub App group, plus the permission the App needs and the note that a permission change needs a re-minted installation token |

Any other message keeps the raw text in the banner body with no target invented
for it: a wrong guess about which field was meant is worse than none.

### The permission runner registration needs

Registering a runner is an **organisation** operation, so the App needs
**Organization → Self-hosted runners: Read & write**. That is not
**Administration**, which only covers repository-level endpoints, and it is not a
repository permission. GitHub mints an installation token from the App's current
permissions, so changing the permission on GitHub does nothing until the token is
re-minted: rotate the key in the GitHub App group (or re-install the App), then
run **Test connection**. The CI runners group states all of that in place, next to
the switch it constrains.

| Key | Default | Who sets it | What it controls |
|---|---|---|---|
| `volume_root` | `/var/stack/data` | setup wizard (`--volume-root`) / operator | the host root every container volume is forced under |
| `storage_nodes` | leader's hostname (seeded once at bring-up) | operator, `join --storage-node`, `node promote/demote` | comma-separated storage node hostnames; stateful stacks round-robin across them, labeled-but-unlisted managers are adopted, and **the current leader is always re-added** (the edge and its local backup agent run there). Losing leadership removes nothing: a former leader stays a storage node, and membership is otherwise the operator's decision. **Entries must be managers**: a worker is pruned on the next `cluster update` (it cannot read settings, credentials or the stack list, so its backup agent would archive and ship nowhere) | **The node provisions itself**: the console toggle only writes this setting, and the node named here creates its data volume at daemon start when the volume root is empty (a node whose volume root already holds data is refused and says so).
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
| `observability_logs_retention_days` / `observability_metrics_retention_days` / `observability_traces_retention_days` | `30` / `14` / `10` (per stream) | operator, or setup (`--oo-logs-retention-days`, `--oo-metrics-retention-days`, `--oo-traces-retention-days`) | per-stream retention in the observability backend (unbounded retention is how a cluster silently grows to hundreds of GB). Each stream has its own default because a day of logs costs far more than a day of traces |
| `log_level` | `info` | operator | **the platform's own** verbosity: `debug`, `info`, `warn` or `error` (`warning` is an alias of `warn`). Applied live when saved, read at daemon startup, and rendered into the platform stack as `PMCLUSTER_LOG_LEVEL` — the env var the daemon and the edge both read. `debug` adds the full rendered compose per deploy level; `info` logs every deploy step with the service names |
| `app_log_level` | `warn` | operator | **the default for deployed apps**, not the platform: every service of every app stack gets `LOG_LEVEL` = its manifest's `log_level`, else this, else `warn`. Same four values and the same `warning` alias. It never overrides a `LOG_LEVEL` a service declares in its own `env:`; it reaches running stacks on their next deploy or sync (see [dsl.md](dsl.md#what-the-translator-injects)) |
| `github_runners_enabled` | `false` | operator | whether the platform manages [self-hosted GitHub Actions runners](runners.md). **Refused without a configured GitHub App**, without `github_runner_org`, or with no node selected; disabling is never gated and removes every runner container and registration |
| `github_runner_org` | unset | operator | the GitHub organisation the runner registers into (org-level registration, so one runner serves every private repo the group selects). Required when enabled |
| `github_runner_group` | `pmcluster` | operator | org runner group the runner joins. Created with visibility `selected` when absent; an existing `all` group is **refused**, because an all-visible runner takes jobs from any repository in the org |
| `github_runner_nodes` | unset | operator + Runners panel | comma-separated WORKER hostnames that may run a runner — the capacity model (N nodes = N runners). Managers are refused: a build on the control plane's 1-vCPU manager marks the node Down. The console's node picker writes this list |
| `node_docker_restart` | `false` | operator | let the node's root watchdog (`pmcluster-watchdog.timer`) restart `docker.service` when its socket stops answering: 3 failed /_ping probes, then `reset-failed` + `restart`, with a 30-minute floor. **Restarting docker stops every container on that node**, including a running CI job — which is why it is off by default and never enabled by an upgrade |
| `github_runner_memory` | `2560m` | operator | the DEFAULT memory limit per runner container (a Docker quantity). `2560m` is the value that completed a `govulncheck` run; `1500m` was OOM-killed mid-job. A node with an entry in `github_runner_node_memory` uses that instead |
| `github_runner_node_memory` | unset | operator + Runners panel | per-node caps, `host=cap,host=cap` (e.g. `nxt-sw-3-w=1200m`). Overrides the default for the named workers only. **Why it exists:** one cap for a fleet of different machines fits the node it was tuned on and is permission to OOM the others — `2560m` is 66 % of a 3852 MB worker and 139 % of a 1839 MB one. A malformed entry is dropped at read time and refused at write time; removing an override falls back to the default |
| `github_runner_allow_managers` | `false` | operator, the console's runner group | allow a runner on **any manager**, including the leader. Off by default because a job competes with the API and the reconcile loop for CPU and memory; on, the picker lists every manager with a warning pill. The old key `github_runner_allow_leader` is read as a fallback, so a cluster that set it keeps the permission. |
| `github_runner_assignment_interval` | `60` | operator: `cluster settings set` or `pmcluster setup --github-runner-assignment-interval`. **No console field** — the poll cadence has a working default and is sent to each node inside its assignment, so it is not part of the enable decision the page guides | seconds between a node's polls of the leader for its [runner assignment](runners.md#how-a-worker-learns-what-to-run-the-assignment-channel). The settings surface **refuses** a value outside 10–3600 (`5` and `7200` are errors, not clamps); the loader still clamps a stored value and falls back to `60` when it cannot parse one, so a bad row cannot stop the poll or hammer the leader. It travels in every answer because a worker cannot read cluster settings |

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
| `stack_pin_<stack>` | written only by `stack move` | outranks the `storage_nodes` round-robin |
| `swarm_id` | written by `cluster update` | wipe detection |

## How a setting change reaches the swarm

1. `cluster settings set key=value` (CLI) or the console's Settings page —
   the value is persisted in the store **only**.
2. The next `cluster update` (or the console's **Apply to swarm** button,
   `POST /api/update`) re-renders the affected platform configs and
   re-deploys what changed.

Two destinations, two mechanisms, and they are not the same wait:

- **Another manager's own store** is current within one poll (60 s). A manager that
  is not the leader pulls `GET /internal/api/cluster/snapshot` over the swarm-mTLS
  channel and applies settings, secrets and API tokens in one transaction. So
  `pmcluster doctor` and `pmcluster cluster settings` on a follower read the cluster's
  real settings instead of a copy of unknown age: both print `mirrored: from <leader>,
  version N, applied … ago (age_seconds=N)` above the values, and `doctor` raises the line
  to a warning once the copy is older than three polls. The mirror records when it applied
  (`sync_state`) and when each row arrived (`mirrored_at`). A leader that dies leaves the
  mirror in place: a stale answer, never a blank one. Writing still needs the leader.
- **Running containers** only change when the platform re-renders and re-deploys —
  step 2 above. Mirroring the store does not touch a container.

## Promoting a node to storage from the console

The console's storage toggle (`POST /web/nodes/<hostname>/storage`) sets the intent: the
hostname goes into `storage_nodes` and the node is labelled. It does not provision
anything itself, and it cannot — provisioning is local work (loop file, LVM, mkfs,
mounts), so only the node can do it.

The node converges the intent on its own — at daemon start and then every two minutes,
so a toggle an operator just flipped takes effect without waiting for a restart:

```
1  is this hostname in storage_nodes?          no  → nothing happens
2  is /var/stack/data provisioned and ours?     yes → nothing to do, silently
3  does /var/stack/data hold any entries?       yes → REFUSE, and say so in the journal
4  otherwise                                        → provision (loop mode), then say so
```

Same rule as the memory drop-in and the docker dependency: a setting is intent, and the
node the intent names makes it true.

The consequence worth internalising: a promotion on a node with no free space, or with
data already in the volume root, leaves a node that is *listed* as storage and cannot
host a stateful stack. The journal says which of the two happened; the console's
checkbox cannot.

If it refused because the volume root holds data, that node needs **adoption**, not
provisioning. The console has its own control for it — *Adopt existing data* next to the
storage toggle — which says in its confirm text what the other button does not: the node
**drains** (its stacks stop), the data is copied onto a new snapshot volume and verified,
the mount is switched, and the node is activated again. Afterwards the previous tree is
kept at `<volume-root>.pre-migration-<timestamp>` until you delete it.

Adoption is deliberately not automatic and not implied by promoting a node:

```
storage_nodes        "this node may hold stateful stacks"        safe, reversible
storage_adopt_nodes  "copy this node's data onto a volume, now"  stops the node, moves data
```

The marker is one-shot: the node clears it when the work is done, or immediately when the
volume root is empty or the volume is already provisioned, so a stale request cannot sit
in the settings pretending a migration is pending. The CLI equivalent is the documented
procedure — `docker node update --availability drain`, `pmcluster storage migrate`,
`--availability active` — and the console simply performs it for you.

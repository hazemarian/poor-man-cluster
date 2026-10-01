# Changelog

Release history for **poor-man-cluster**. The RFC and the reference docs describe the
*current* state of the project; this file is the only place that tracks what changed
and when.

## v0.2.125 (2026-10-01)

- **Deploy outcomes joined to each deployment execution.** The stack-level error history (`stacks.last_error` JSON array, keyed by revision) is now JOINED into the revision views — no extra storage. `GET /api/stacks/{name}` revisions and `GET /api/stacks/{name}/revisions/{rev}` each carry an `error` field (the matching history entry for that revision; `""` = that execution applied cleanly). The console revision timeline shows a "Deploy failed" pill on failed executions and the revision detail (modal) page shows the failure banner — so a stack whose current revision is clean but a previous one failed shows both states side by side. Plumbing: `stacks.Revision.Error` (domain + remote + pmapi), `revRow.Error`/`revisionData.Error` (controllers), templates (`frag_stack.html` timeline pill, `frag_revision.html` banner). Test: `TestRevisionErrorJoin` (list + detail endpoints join per-revision outcomes; no per-revision storage).

## v0.2.124 (2026-10-01)

- **Stack deploy error history (JSON)**: `stacks.last_error` now holds a JSON array of deploy/apply outcomes, newest first, capped at 20 (`store.StackErrorEntry{revision, error, created_at}`, `RecordStackError`/`ListStackErrors`, `ParseStackErrors`). Every deploy/apply outcome — success and failure, sync and fire-and-forget — is prepended; a success entry has an empty `error` so the "Deploy failed" banner clears while the failure history is retained. The console stack detail page gains a **Deploy error history** panel (per-revision entries with rev id + timestamp); the stacks list pill still shows the newest failure. `/api/stacks` `last_error` is now an array. No new table (the v0.2.123 column is reused as the JSON store).

## v0.2.123 (2026-10-01)

- **Failed background deploys surface in the console**: new `stacks.last_error` column (migration 0021) records the most recent deploy/apply error and is cleared on the next successful apply. Set on both sync and fire-and-forget (webhook/API) failure paths, exposed through the daemon API → remote CLI → console as a red detail-page banner and a "Deploy failed" pill on the stacks list.

## v0.2.122 (2026-10-01)
- `stacks.Deployer` gained `DeployAsync`; `remote.Deploy` + `RetryDeployer` implement it. Receiver retry-on-transient-error (Q3, v0.2.113) is superseded — the apply is backgrounded once; `retryDeploy`'s unit tests remain.
- Tests: `DeployAsync` returns early while the apply lands in the background, validation errors never start an apply, receiver 202 + delivery semantics, scope-guard HTTP tables expect 202 on deploys, metric labels reflect acceptance-time recording. `recordingDeployer` fakes are now mutex-guarded.

## v0.2.121 (2026-10-01)

- **Deploy pipeline logs at `info`** — the whole deployment process is now visible at the default log level with the service names but without the aggressive per-level compose YAML: `deploy — starting pipeline`, `deploy — manifest parsed`, `deploy stack — deploying depends_on level`, `deploy stack — waiting for level to become healthy`, `deploy stack — level healthy`, `deploy stack — all levels healthy: one drift-prune pass`, `deploy — completed` (stack/revision/services), plus `rollback — completed` and the `sync — no drift` early-return. The full per-level `compose_yaml` stays `debug`-only.
- **Runtime log-level setting** — new `log_level` cluster setting (debug/info/warn/error). Loggers are built at the minimum level and gated by the zerolog process-global level, so changing the setting applies **live** to the running daemon (console, daily log file, and the OpenObserve OTLP writer). The console's **Settings → Cluster settings** page gained a `Log level` select; the daemon also applies the persisted value at startup. Invalid values are rejected at save time.
- Tests: info-vs-debug separation (YAML never leaks into info), settings log_level apply hook, UI select render + save.

## v0.2.120 (2026-10-01)

- **Ordered-deploy visibility**: the deploy pipeline now emits structured zerolog diagnostics at `debug` level — one record per `depends_on` level with the level index, the service names, and the **full per-level subset compose YAML** (`compose_yaml`), plus single-level full-deploy and final drift-prune records. They reach the CLI console (`PMCLUSTER_LOG_LEVEL=debug pmcluster deploy …`), the daemon's daily log file, and OpenObserve through the existing OTel writer whenever `log_level=debug`; `info` and above stay clean. The earlier plain-text `▶ deploy …` markers are gone. Zero-value logger stays a no-op (tests unaffected).

## v0.2.119 (2026-10-01)

- **`run_once` wait edge case fixed**: a one-shot job that failed an attempt then succeeded on a retry left both `[failed, complete]` task records — the ordered-deploy wait loop previously aborted the deploy on any failed record. The wait now ignores records superseded by newer attempts (any active task keeps waiting), and with no active task the **newest terminal task** decides (ties by start time): `complete` → ready, `failed`/`rejected` → deploy error, `shutdown`/`removed` → "did not complete". Six new wait-path tests.

## v0.2.123 (2026-10-01)

- **Failed background deploys surface in the console**: fire-and-forget deploys (v0.2.122) apply in the background, so the apply error was only visible in the logs. The stack row now records the last deploy/apply error (`stacks.last_error`, migration 0021) — set when a deploy or its background apply fails and cleared on the next successful apply. Exposed as `last_error` in the stacks API, remote CLI and console: the stack detail page shows a "Deploy failed" banner with the error, and the stacks list shows a red pill per failed stack. Covers every trigger (webhook, API, CLI, sync, rollback).

## v0.2.122 (2026-10-01)

- **Fire-and-forget deploys**: `DeployAsync` validates (parse → conflict check → interpolate → validate → translate → record revision) synchronously, then applies the swarm deploy in the background on a detached context. The webhook receiver and `POST /api/stacks` return **202** `{status:accepted, stack, revision}` immediately; validation errors still return 400/502 synchronously; the delivery row records `accepted` at acceptance time. The receiver-side retryer is superseded. Deploy order (`depends_on` levels), wait-for-healthy and drift-prune are unchanged.

## v0.2.121 (2026-10-01)

- **Info-level deploy logs**: the whole deployment pipeline logs at `info` with the service names (start, per-level deploy, waiting, level healthy, prune, completed); the per-level compose YAML stays `debug`-only. Structured zerolog records reach the CLI console, the daemon log file and OpenObserve.
- **Runtime log level**: new `log_level` cluster setting (console Settings → Cluster settings select, or `pmcluster cluster settings set log_level=debug`) applies live to the running daemon via the zerolog process-global — no restart. Loggers are built at the minimum level and gated by the global, so re-level works in both directions.

## v0.2.118 (2026-10-01)

- **`depends_on` now orders the deploy — no more rendered wait wrapper.** Docker Swarm parses and ignores compose `depends_on` (no dependency graph); the previous fix wrapped a dependent service's command/entrypoint in a POSIX `sh` loop (`getent`/`nc -z` probes). That wrapper was a runtime-ordering workaround encoded into a rendered artifact — it broke baked-entrypoint images, distroless images without `sh`, had no timeout, and made port/DNS assumptions. It is **removed**.
- **Control-plane ordered deploy**: the pipeline topologically sorts the stack's services into `depends_on` levels (`manifest.ServiceLevels`, Kahn's algorithm — cycles and unknown dependencies fail loudly before anything is created), renders each level as its own compose (`IR.Subset`), deploys level-by-level via `docker stack deploy` (`DeployStackNoPrune` — partial deploys never drift-prune), waits each level healthy before the next (long-running services until replicas ≥ desired; `run_once` jobs until the task completes — failures stop the deploy with the task error), then runs **one** drift-prune pass (`PruneStack`) with the full stack compose. Single-level stacks keep today's full `DeployStack` path unchanged.
- **Rollback re-translates the source**: `rollback` now re-translates the target revision's `source_yaml` with the current translator, records a fresh revision (new `rendered_hash`, `rollback_of` marker), and deploys through the ordered pipeline. Stored `rendered_yaml` is never executed (audit/display/hash only). A source that no longer validates under the current DSL fails loudly instead of silently deploying a stale translation. Deploy and sync already followed this invariant.
- New `StackDeployer` interface methods `DeployStackNoPrune` + `PruneStack` (production + all test fakes).
- Tests: `manifest.ServiceLevels`/`IR.Subset` (chain, fan-out, multi-level, cycle, unknown-dep, empty, subset recompute), ordered-deploy order + prune-once + cycle/unknown-dep rejection, rollback re-translate semantics.

## v0.2.115 (2026-09-30)

- Console: the **API Keys** nav link is now visible for admins when `EDGE_LOGIN_DISABLED=true` (it was previously bundled with the Users CRUD link, which correctly stays hidden — the API Keys page is admin-only but not login-UI-gated). Regression test added.

## v0.2.114 (2026-09-30)

- Usage graph now shows **linked configs** — the DSL resolves `config(name)` into env content at translation time, so rendered compose had no top-level `configs:` block and the usage page showed only secrets. The graph now unions `refs.FindAll(source_yaml)` (the authority for `config()`/`secrets()` refs) with rendered `configs:`/`secrets:` parsing.

## v0.2.113 (2026-09-30)

Quick wins from the improvement backlog (Q1–Q5; Q6 `deploy --compose` deferred):

1. **CLI UpdateStatus column** — `pmcluster service list` / `ps` gained a `STATE` column (`PAUSED`, `PAUSED: <error>`, `UPDATING`, `-`) from the service update status.
2. **Registry credentials on join** — `pmcluster join --copy-registry-creds <host>` ssh-fetches and merges the manager's `~/.docker/config.json`, and `--verify-registry-pull <image>` proves pulls work; both best-effort (the missing-auth silent-stale-image trap now has an explicit path).
3. **Webhook delivery retry** — deploy failures retry 2× every 30s (detached 4-minute budget, so the retry outlives the request), and delivery rows record `retries`; one request still yields exactly one delivery row.
4. **Per-stack API token scoping** — `pmcluster user create --stack <stack>` mints tokens that can only touch that stack's `/api/stacks/<name>` + `/api/services/<stack>` routes; everything else is `403 {"error":"token scoped to stack <x>"}`. Unscoped tokens unchanged. Migration 0020.
5. **OpenObserve alerting metrics** — the daemon now emits OTLP metrics: `pmcluster.webhook.requests.total{source,status}`, `pmcluster.services.paused{scope}`, `pmcluster.services.stale_images{scope}` (image > 30 days old), `pmcluster.reconcile.total{stack,status}` — for OO-side alerting.

## v0.2.112 (2026-09-30)

- Port-aware `depends_on` wait — the generated wait probe now connects to the dependency's service port (`nc -z`), not just its DNS name. Port resolution: `expose.port` wins, otherwise a well-known image default (postgres 5432, mysql/mariadb 3306, redis 6379, mongo 27017, nginx/httpd 80, rabbitmq 5672, elasticsearch 9200, memcached 11211), otherwise DNS-only.
- Wait scripts are `$`-escaped (`$$`) so `docker stack deploy` interpolation leaves them intact (the unescaped form broke the compose).

## v0.2.111 (2026-09-30)

- `depends_on` entrypoint wrap — services without an explicit `command:` get their ENTRYPOINT wrapped with the wait loop instead; the image's baked `CMD` flows through `$@`. Images whose runnable lives inside a baked entrypoint (e.g. Postgres) should declare an explicit `command:`.

## v0.2.110 (2026-09-30)

Five post-mortem hardening points:

1. **`depends_on` → real Swarm wait** — `docker stack deploy` ignores `depends_on`, so dependent services with an explicit command get a generated POSIX wait wrapper (`until getent hosts <dep> ...`), and `run_once` services with dependencies get `restart_policy: on-failure` (`max_attempts: 3`) instead of dying permanently.
2. **Stateful-aware defaults** — services that mount volumes automatically get `update: order: stop-first` (kills the Postgres `postmaster.pid` shutdown race) and auto-pin to the platform node (`platform_node` setting) when `placement` is empty. No manifest change needed.
3. **Registry auth + image freshness** — `pmcluster join` warns when no Docker registry credentials exist (missing `~/.docker/config.json` → stale cached images); the console shows `Image N days old` pills for stale local images.
4. **Paused updates surfaced** — Swarm's `UpdateStatus.State=paused` and the failure message are exposed through the services API and shown in the console (paused pill + hint), so updates that stall are no longer silent.
5. **Config/secret lifecycle** — `pmcluster secret edit` mirrors the new value to the Swarm secret (immutable secrets are removed and recreated); the inventory page shows which stacks reference each config/secret ("Referenced by").

## v0.2.109.1 (2026-09-30)

- Fix(ui): the stacks list no longer shows completed run-once jobs (e.g. a finished migration) as Degraded — completed jobs are excluded from the health totals.

## v0.2.109 (2026-09-30)

- Test coverage: CLI config/secret CRUD run-path tests (cli coverage 11.6% → 19.7%); pmapi client tests against a fake daemon (0% → 75.9%).

## v0.2.84 (2026-09-25)

- `pmcluster join --role worker|manager --token <token> --manager <host>:2377` — verify the
  joined role after `docker swarm join` and print quorum guidance.
- Two-manager production reference deployment (ubuntu Leader + pmcluster-worker Reachable).

## v0.2.83.1 (2026-09-24)

- Fix the systemd unit generated by the CLI: `ExecStart` must include the `serve`
  subcommand (a bare binary unit printed help and crash-looped).

## v0.2.83 (2026-09-24)

- **`pmcluster join`** — join the Swarm, initialise local control-plane state (no bootstrap
  user), install + start the daemon. The daemon is now started by the CLI:
  `cluster up`, `cluster update`, and `join` manage `/etc/systemd/system/pmcluster.service`;
  `install.sh` only installs the binary.
- `serve` waits for Swarm leadership before serving (standby on non-leader nodes);
  standalone/local mode serves immediately.

## v0.2.82 (2026-09-25)

- **Leader-aware daemon**: `serve` serves only on the Swarm leader (15s standby poll on
  non-leader managers). On promotion, the control-plane DB is restored from the newest
  `pmcluster-ctlplane-*.tar.gz` only when the local DB is missing or older — safe with
  shared storage (LINSTOR).

## v0.2.81 (2026-09-25)

- Console OpenObserve external link points at `observ.<domain>/sso-bridge` (one-click
  auto-login into OpenObserve).

## v0.2.80 (2026-09-25)

- **OpenObserve auto-login bridge** (`/sso-bridge` on the observ origin): a page served by
  the edge console that seeds OO's SPA session (`localStorage userInfo`, `pgdata` bypass)
  so OpenObserve opens logged-in behind the SSO/basic-auth gate.

## v0.2.79 (2026-09-25)

- Traefik `openobserve-auto-auth` now also injects the `auth_tokens` session cookie
  (30-day) — a deterministic base64url envelope built from the `openobserve_admin`
  credential — so OO's SPA does not redirect to its native login page.

## v0.2.78 (2026-09-25)

- Whole-disk (cluster-wide) backup runs count as **verified coverage** for every stack in
  the console's backup coverage table.

## v0.2.77.1 (2026-09-25)

- Restore: allow the archive root directory entry through the path-escape guard.

## v0.2.77 (2026-09-25)

- **Restore implemented**: `pmcluster backup restore <id>` extracts a SUCCEEDED backup.
  Stack-scoped runs restore under `<dest_root>/<stack>`; whole-disk scheduled runs restore
  to the volume root with the `/backup/data` prefix stripped; control-plane archives are
  refused into the volume root. Retention pruning (`backup_retention_days`, default 15)
  removes old rows **and** archive files.

## v0.2.76.1 (2026-09-25)

- **Backup discovery**: scheduled offen archives on disk (`/var/stack/backup`) are recorded
  into the backup list (migration 0018, unique partial index on `filename`) so the console
  and CLI show the full backup picture, including nightly runs with real archive paths.

## v0.2.76 (2026-09-25)

- Image display: digest trimmed from service tables; full ref shown on hover.

## v0.2.75 (2026-09-25)

- **Standalone stack detail page** (`/web/stacks/<name>`) with per-stack services: replica
  health, mode, updated, and Tasks/Logs/Restart actions.

## v0.2.74 (2026-09-25)

- Revision/backup IDs are displayed without thousands separators (`N0` template func);
  stack detail scrolls into view after navigation.

## v0.2.73 (2026-09-25)

- **Console v2** (PR #2): full EN/AR localization, calm theme, self-hosted IBM Plex fonts,
  RTL support, per-request language resolution. htmx stays on the CDN.

## v0.2.72 (2026-09-25)

- **Rebrand**: repository renamed to `hazemarian/poor-man-cluster`, MIT license, edge image
  now public at `ghcr.io/hazemarian/pmcluster-edge` (anonymous pulls work).

## v0.2.71 (2026-09-25)

- CLI/API audit completion: `backup browse|restore`, `webhook deliveries`,
  `cluster settings [get|set]`, `usage`; OpenAPI now documents 39 paths.

## v0.2.70 (2026-09-25)

- Fixed the console list pages: polling wrappers use a stable target + `innerHTML` swap so
  clicks are no longer swallowed and timers don't stack.

## v0.2.69 (2026-09-25)

- Fixed logs polling self-embedding (dedicated `servicelogs` fragment for 3s polls).

## v0.2.68 (2026-09-25)

- Settings + usage refactored into domain packages (`internal/settings`, `internal/usage`).

## v0.2.67 (2026-09-25)

- Console features: webhook delivery history, backup browse/restore, API token last-used,
  cluster settings edit surface, deploy pipeline view, secret/config usage graph, cert
  expiry badges.

## v0.2.66 (2026-09-25)

- Console SPA polish: active nav, loading indicator, toasts, live polling, list filters,
  revision timeline, log streaming, cluster status card.

## v0.2.65 (2026-09-25)

- **Deploy provenance**: `repo_url` + `source_file` required in webhook payloads and
  recorded per deploy (migration 0015).

## v0.2.64 (2026-09-25)

- `cluster up` creates the storage root directories (`/var/stack/data`, `/var/stack/backup`).

## v0.2.63 (2026-09-25)

- **Volume root**: every container volume — named or bind — is forced under one configurable
  host root (`volume_root`, default `/var/stack/data`) at `<root>/<app>/<name>`; the
  app-level `volumes:` block was removed from the DSL.

## v0.2.62 (2026-09-25)

- **Isolation refactor**: the Docker client moved behind a neutral port
  (`internal/runtime`); the DSL translation output moved behind a `Writer` interface
  (`internal/manifest`, ComposeWriter). Also fixed the `/web` PathPrefix trap that sent
  `/webhook/*` through the SSO gate.

## v0.2.61 (2026-09-25)

- SSO session expiry configurable (`--sso-cookie-expire`, default 1h).

## v0.2.60 (2026-09-25)

- SSO redirect finally fixed: `OAUTH2_PROXY_WHITELIST_DOMAINS` (plural).

## v0.2.59 (2026-09-25)

- `OAUTH2_PROXY_REVERSE_PROXY` so the post-login redirect returns to the original host.

## v0.2.58 (2026-09-25)

- Post-login landing returns to the original host; Traefik dashboard router matches the
  live gate.

## v0.2.57 (2026-09-25)

- `OAUTH2_PROXY_COOKIE_DOMAINS` (plural) fixes cross-host CSRF on the OAuth callback.

## v0.2.56 (2026-09-25)

- `/oauth2/*` resolves on every gated host; `certresolver=letsencrypt` labels are
  ACME-conditional (BYO-cert clusters).

## v0.2.55 (2026-09-25)

- SSO enabled on the live cluster; missing `sso_cookie_secret` self-heals on
  `cluster update`.

## v0.2.54 (2026-09-25)

- Console external links (OpenObserve, Traefik); oauth2-proxy on the `sso.<domain>`
  subdomain.

## v0.2.53 (2026-09-25)

- **SSO feature flag** (oauth2-proxy sidecar), interactive **`pmcluster setup`** wizard,
  OpenObserve root-admin ingestion (no provisioning API).

## v0.2.52 (2026-09-25)

- Users CRUD + RBAC (admin/operator/viewer) + `EDGE_LOGIN_DISABLED`.

## v0.2.51 (2026-09-25)

- **Auth gateway**: the console moves under `/web`, Traefik admin-auth (or SSO) gates
  `/web/*` + `observ.<domain>`; OpenObserve auto-auth header injection.

## v0.2.50 (2026-09-25)

- `rendered_hash` on stack revisions — `sync` skips no-op deploys.

## v0.2.49 (2026-09-25)

- `--version` flag.

## v0.2.48 (2026-09-25)

- `internal/refs` package — the shared `config()/secrets()` reference language.

## v0.2.47 (2026-09-25)

- Unified `config()/secrets()` references in the platform stack templates.

## v0.2.46.1 (2026-09-25)

- Wire the services domain into the daemon (`/api/services` routes).

## v0.2.46 (2026-09-25)

- **Config reconcile redesign**: DB-only configs, `rendered_hash` (migration 0013),
  init-only `cluster up`, hash-based `cluster update`, UI Sync button.

## v0.2.45 (2026-09-25)

- DB↔store config sync + drift-prune of services dropped from a compose.

## v0.2.44 (2026-09-25)

- Fix empty `volumes:` mapping when ACME is off.

## v0.2.43 (2026-09-25)

- **Service ops replace Portainer** (list/ps/tasks/logs/restart/exec).

## v0.2.42 (2026-09-25)

- Remote CLI mode (API URL/token via env or flags).
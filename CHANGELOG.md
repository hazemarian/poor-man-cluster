# Changelog

Release history for **poor-man-cluster**. `docs/whitepaper.md` is the design paper; the reference
docs under `docs/` describe the platform as built.

## Unreleased — the console's Settings page states its prerequisites

**Three settings failures in one evening were all the same failure: the page made
an operator discover a prerequisite by hitting it.** A runner switch turned on
with no org (the save was refused), a node picker nobody could find (it lives on
`/web/runners`, reachable only through a panel button), and a GitHub permission
that looked granted but covered repository endpoints instead of the organisation
(the App needed *Organization → Self-hosted runners: Read & write*). The refusal
on top of that answered "The cluster settings could not be saved" with the reason
folded into a "Show details" disclosure.

- **Six groups instead of a wall of panels**: Connection, GitHub App, Identity and
  sign-in, CI runners, Backups, Advanced. Each has a heading, one line of purpose,
  and one line naming what it needs, above its fields.
- **CI runners puts the enable switch first**, then the org, runner group, memory,
  socket proxy/path and allow-leader — and links the node picker in one click from
  the heading, with a read-only summary (enabled, selected workers, one pill per
  runner) so "where do I choose the nodes" has an answer on the page.
- **The permission is named in place**: Organization → Self-hosted runners:
  Read & write, not Administration, and a permission change needs a re-minted
  installation token.
- **Errors land where they happened**: the daemon's message is the banner body,
  the group it names is marked, and the field it names carries the sentence. Known
  messages map to `github_runner_org`, `github_runner_nodes` and
  `backup_retention_days`; anything about permissions marks the GitHub App group;
  anything else keeps its raw text with no target invented.
- **One sticky save bar** for the four daemon-stored groups, reporting dirty/clean
  and what a save covers. No autosave. A successful save re-renders with a
  dismissible confirmation that names the group(s) that changed.
- **The GitHub App status and its form are one group**, and a page with no App (or
  with an unreachable daemon) says so instead of rendering empty fields a save
  could blank.
- `/web/settings/cluster` renders the same page, so old links (including the
  Runners empty state) land on the new layout.

## Unreleased — Runners actually run on workers, fed by an mTLS assignment channel

**The self-hosted runner feature was inert on every node it was allowed to use.** A
worker's daemon blocked in `waitForSwarmLeadership` forever, and everything after that
call — including the runner loop and the socket proxy — only ever ran on the leader,
which is refused as a runner host by design. Measured before the fix on `nxt-sw-6-w`
and `nxt-sw-3-w`: `curl 127.0.0.1:9090/health` returned nothing at all and
`/run/pmcluster/` did not exist.

- **The per-node loops start BEFORE the leadership wait**, the pattern the backup plan
  and the version publisher already document. The filtered docker socket binds on
  every node with a docker socket, and a worker's runner loop starts there too.
- **A worker serves its API on loopback** (`127.0.0.1:9090`), never off-host: it is
  standing by, it has no cluster state, and it opens no store — but `/health` answers
  and `pmcluster doctor` works on every node instead of refusing.
- **The runner decision is resolved on the leader and pulled by the node**
  (`GET /api/github/runners/assignment`), because a worker cannot read cluster
  settings at all: Docker configs are a manager-only read. The leader applies the same
  rules as the reconcile (feature switch, org, node list, manager/leader refusal,
  `selected` group) and sends the resolved spec only.
- **Authentication is the certificate Swarm already issued**, checked by a new
  `internal/auth/swarmtls` middleware on its own listener: chain to the swarm CA,
  `O` == this cluster's swarm ID, `CN` == a live node ID (so `docker node rm` is the
  revocation path), not expired. Identity comes from the certificate only; `?node=` is
  ignored. The middleware is mounted on that one route and nothing else.
- **Transport is the leader's swarm data-path address**, read from `docker info`, with
  `127.0.0.1:9092` as the fallback. Port 9092 is bound to the node's overlay address,
  never published, and added to `harden-host.sh`'s restricted list (private subnets
  only, denied from the WAN). The overlay service name and an ingress publish were
  measured as unusable for a host-process listener and are documented as such.
- **A worker's runner is started from the assignment** with the filtered socket and
  no raw mount; de-registration stays on the leader, which is the only side holding
  the GitHub App. `github_runner_assignment_interval` (default 60s) sets the poll.
- **`pmcluster doctor` on a worker** now answers the node-local subset — daemon
  listening, socket proxy present, assignment age and source, local runner state,
  disk — and prints a skipped line naming each cluster question it will not answer.

## Unreleased — The GitHub App key is set and rotated from the console

**App id, installation id and the private key are edited in the console's GitHub
panel instead of over SSH.** Until now the only way to set the key was
`pmcluster github app set --key-file`, so a rotation meant logging into the
leader. The panel now carries all three values and validates them exactly the way
the CLI does before it writes anything.

- **`POST /api/github/app`** (operator role; a viewer gets 403) takes app id,
  installation id, the `.pem` (optional once one is stored), probe repo and probe
  package. Validation order is parse the PEM (PKCS#1 or PKCS#8, RSA only) → sign
  an app JWT → exchange it for an installation token for the **new** installation
  id → run the probe. Nothing is persisted before the mint succeeds, so a wrong
  key, a dead installation or a failed probe leaves the working registration
  intact; the previous key keeps minting.
- **One write path, not two.** The validation lives in `githubapp.Local.Apply`
  and its last step is `Local.Save` — the function `pmcluster github app set` has
  always used — so encryption, the fingerprint and the minter invalidation cannot
  drift between CLI, setup and console.
- **Partial update.** An empty key keeps the stored key; omitted ids keep the
  stored ids; omitted probe fields keep the stored targets. An installation-only
  change still mints for the new installation before storing it, and a
  registration with no key while none is stored is a 400 naming the missing key.
- **No key material in the answer or the log.** The response carries
  `{app_id, installation_id, key_fingerprint, probe:{ok, detail}}` — a SHA-256
  fingerprint of the key's public half, never the key (not even masked). Parse
  errors name the PEM line and block type (`line 3: unsupported block type
  "ENCRYPTED PRIVATE KEY"`), never the content; the route's one audit line drops
  everything from a PEM header on. Verified that no middleware in the request path
  buffers or prints request bodies.
- **Transport guard.** A private key is refused (400, `code:
  insecure_transport`) unless it arrived over TLS, carried
  `X-Forwarded-Proto: https` (the header the console already uses for the session
  cookie), or came from loopback. The console applies the same rule to the
  browser hop first and forwards the browser's scheme.
- **The panel** shows the fingerprint, the last probe result and its age before
  the form, prefills app id / installation id / probe targets, accepts a pasted
  `.pem` or a file upload (64 KiB max, `enctype="multipart/form-data"` on that
  form only), labels the submit **Save** or **Rotate key**, and states after a
  rotation that the previous key stops working immediately.
- **`pmcluster setup`** gains optional `--github-app-id`, `--github-app-key-file`
  and `--github-installation-id`, validated by the same `Apply`; without them it
  prints that the App can be configured later from the console.
- **Schema:** migration `0029_github_app_console.sql` adds `key_fingerprint` and
  the `last_probe_*` columns; `GET /api/github/app` reports them and
  `docs/openapi.yaml` documents the new route.
- **Tests:** a set persists and returns a stable-per-key/different-per-key
  fingerprint; installation-only change keeps the fingerprint and the ciphertext;
  installation-only with no stored key is 400; a bad installation leaves app id,
  installation id and key intact and the old key still mints; a malformed PEM is
  400 with no key material in the response or a captured log buffer; viewer 403;
  cleartext non-loopback 400 (and `X-Forwarded-Proto: https` accepted); oversized
  body/upload 400; the form prefills; the panel renders the fingerprint and probe
  age; en/ar parity.

## Unreleased — Self-hosted GitHub Actions runners, org level, behind a filtered docker socket

**One ephemeral runner container per selected worker, registered at ORG level into a
`selected`-visibility runner group, building through a socket proxy the daemon owns.**
When GitHub-hosted minutes run out, CI stops. A repo-scoped runner per repository was
the first sketch; the operator's hand-made org runner showed the right shape, and one
registration serving the group's repositories is both cheaper and easier to reason
about. The docker socket is the part that had to change: a runner that can reach
`/var/run/docker.sock` can remove a service, demote a node or read every swarm secret,
so the platform now exposes a filtered socket instead.

- **Registration is org level.** `POST /orgs/{org}/actions/runners/registration-token`
  with the App installation token, and the container joins a **runner group**
  (`--runnergroup`). `github_runner_repos` is gone; `github_runner_org` and
  `github_runner_group` (default `pmcluster`) replace it.
- **The group is the security boundary.** The platform creates it with
  `visibility: selected` and the org's private repositories as members, re-scopes it
  as private repositories are added, and **refuses** an existing group visible to
  `all` — an all-visible runner takes workflow code from any repository in the org,
  including one an attacker can push to. Checked on every pass, not only at
  registration.
- **Runners are per node, not a swarm service.** `github_runner_nodes` lists the
  WORKER hostnames allowed to run one; each node's daemon runs (or restarts) its own
  local container and removes it when the node leaves the list. Capacity is the list:
  N nodes = N runners. **Managers are refused**, naming why: a build on the
  control plane's 1-vCPU manager marks the node `Down` and takes the cluster API with
  it (which is exactly what happened on the live cluster with the telemetry loop).
- **`internal/socketproxy`: the filtered docker socket.** An in-process reverse proxy
  on a unix socket (`github_runner_socket_path`, default `/run/pmcluster/docker.sock`,
  root:root 0660) that forwards an allow list and refuses everything else — 403 with
  a JSON body naming the route, plus one rate-limited WRN line so an operator can grep
  `socket proxy: denied` and see that a workflow tried `service rm`. Allowed:
  containers (list/inspect/create/start/stop/restart/kill/wait/logs/remove), images
  (list/pull/build/inspect/remove), volumes, networks, `/version`, `/info`,
  `/events`, `/_ping`. Denied, each tested: `/services`, `/tasks`, `/nodes`,
  `/swarm`, `/secrets`, `/configs`, `/plugins`, `/system`, `/session`, `/auth`,
  `/exec` and `/containers/{id}/exec*`. Deny-by-default, segment-exact, no generic
  passthrough. `github_runner_socket_proxy=false` restores the raw mount as a
  debug-only fallback (documented as unsafe for untrusted workflows; the doctor warns).
- **Enable/disable is a complete transition.** `github_runners_enabled` is the one
  switch every surface reads. Enabling is refused without an App, without an org, or
  with no node selected. Disabling removes every container the platform owns (by its
  `pmcluster.runner` label), de-registers each name, clears the rows, and is
  idempotent; with GitHub unreachable the containers still come off and the report
  says the de-registration is unconfirmed, keeping the entry for the next pass.
- **The console panel.** `/web/runners`: a **node picker** (one row per node with
  role badge, current runner state and free memory; managers greyed out with the
  reason on hover) writing `github_runner_nodes`, and a runner table (scope, name,
  node, state pill, mode, age, last job) with **Kill** per row and **Restart** per row
  plus **Restart stopped**. A busy runner's Kill is a confirmation that says it will
  fail the running job. A killed runner is held, so no pass re-registers it.
- **Doctor reworked.** ENABLED/DISABLED first; leftovers while disabled are a warning
  naming `github_runners_enabled` (the half-disable failure mode); a runner on a
  manager, a raw-socket container or an unmanaged pmc-named runner (the operator's
  hand-made one, named and never counted healthy) are reported without failing the
  check.
- **Tests:** the org registration-token path (asserted, not commented), group created
  `selected`, `all` refused, enable refused without App/org/node, idempotent reconcile,
  disable removing N containers and making N de-registration calls then a clean no-op,
  disable with GitHub down still removing containers, kill of an unknown runner 404,
  busy kill 409 then acted with confirm, GitHub down during a kill still removing the
  container, the socket proxy's deny table (every family, never reaching the upstream
  transport), the allow table's own entries, the audit rate limit, the rendered
  container's mounts and `DOCKER_HOST`, the panel's rows/disabled state/picker, and
  en/ar parity.
- **Docs:** `docs/runners.md` rewritten (org level, the group, the allow list, the
  per-node model, the manager refusal, the audit line), settings catalogue, console
  page list, `pmcluster/docs/openapi.yaml` (org scope, kill/restart outcome) and this
  entry.

## Unreleased — Pulls use a read-only PAT applied at deploy; the App keeps clone, build and the broker

**Removes the 25-minute App-token refresh, the leadership-gain mint and the interval language that
went with them.** The frozen service spec was never the timer's problem: every
`docker stack deploy --with-registry-auth` embeds the credential current at that moment into each
spec, and a spec is never rewritten. A one-hour installation token therefore dies inside a spec that
outlives it, which no amount of re-logging-in can fix. A long-lived read-only PAT makes the frozen
copy harmless, so the two concerns split: pulls use the PAT, everything else uses the App.

- **Removed:** `refreshGitHubAppLogins` and its `serve.go` call site, `loginOnLeadershipGain`,
  `leaderGainSteps`, the App-token branch of the startup replay (`registryLoginTargets`), and the
  `WRN … renewed every 25 minutes` line. Nothing in `internal/` or `cmd/` starts a login timer; a
  test asserts the symbols are absent.
- **Pull credential = stored read-only PAT.** Applied once at daemon start and **once per deploy**,
  immediately before the first `docker stack deploy --with-registry-auth`
  (`stacks.Service.BeforeApply` → `loginBeforeDeploy`; wired in `serve`, `deploy` and `cluster
  up/update`). A multi-level `depends_on` deploy still logs in once. Serialised by the existing
  mutex, password on stdin, `DOCKER_CONFIG=/root/.docker`.
- **The deploy login is the warning.** `WRN docker logged in to ghcr.io using the stored registry
  credential (added N days ago); read-only PATs expire at 90 days — rotate with pmcluster registry
  add` — one line per deploy.
- **Doctor gains `registry credential`.** Warns (never fails) once the stored ghcr.io credential is
  60 days old and names the rotation command. The age is `registries.created_at`; `registry add` now
  resets it on rotation, so the age is the current credential's, not the first-ever row's.
- **Kept unchanged:** `internal/githubapp`, the encrypted App registration, `POST /api/github/token`
  and the `github app` doctor check that mints directly. New tests cover one-login-per-deploy (via
  the hook and end-to-end through a two-level deploy), the age/rotation warning, serialisation, the
  doctor threshold and the absence of the timer; the App broker and minting tests are untouched.
- **Docs rewritten honestly.** `github-app.md` §5, `registry-credentials.md` §4/§5.3/§5.4/§8/§9,
  `health.md` and the architecture note now say: pulls use the PAT and are frozen into specs, and
  that is fine because it does not expire in practice; the App covers clone/build/broker; the real
  risk is the 90-day expiry, surfaced by doctor from day 60. Residual: a revoked or expired PAT
  breaks new pulls — detect it with doctor, not by waiting for a failed deploy.

## Superseded (never released) — Pull credentials back on the 25-minute login loop

> Superseded by the entry above: the timer, the leadership-gain mint and the App-token pull login
> were removed in the same unreleased cycle. Kept as the record of why the pull-time helper was
> rejected.

**Reverts the pull-time credential helper (`dad7c2a`) and restores the original `docker login` timer
(25 minutes), adds a login on leadership gain, and makes every login a warning that states when the
credential expires.**

The helper authenticated a node's docker client over a second edge listener on :9443 with swarm-node
certificate mTLS, and shipped a `docker-credential-pmcluster` binary plus `credHelpers` wiring in
`config.json`. It solved one case — a manual `docker pull` on a node — and left the problem the operator
actually hit untouched: `--with-registry-auth` embeds the credential current at deploy into the service
spec, and a spec cannot be rewritten after the fact, so a task rescheduled onto a node without the image
still fails to pull an hour later. Given that the frozen-service-spec problem remains either way, the
extra machinery (helper binary, extra port, certificate verification, per-node config merge) bought less
than it cost. The operator chose the simple mechanism and asked to be told when docker logs in.

- **Revert:** `git revert dad7c2a` removes `internal/pullcred`, the :9443 ingress listener and its
  `cmd/edge/cred.go`, `docker-credential-pmcluster`, `internal/cluster/dockercfg.go` (symlink +
  `credHelpers` merge), `POST /api/github/pull-credential`, the swarm-cert rule in `harden-host.sh`,
  and the doctor path that shelled out to the helper. `refreshGitHubAppLogins` and its call site return.
- **Timer unchanged: every 25 minutes.** Two logins per 60-minute token lifetime (t=25 and t=50 against a
  token that dies at t=60), so docker's config never holds a credential the token cache has outlived. The
  original reasoning is restored at the constant's default.
- **Login on leadership gain.** The follower→leader path mints and logs in once, after control-plane
  adoption and **before** the reconcile starts, so the specs written immediately after a failover carry a
  fresh credential instead of whatever the previous leader left on disk. Failure is a warning, not a
  blocker: the reconcile proceeds and the timer retries within 25 minutes.
- **The login is announced at WARN on every path.** `docker logged in to ghcr.io with a GitHub App token;
  it expires at <RFC3339> and is renewed every 25 minutes` appears in the journal and in the deploy
  output. A failed login warns with docker's stderr and does not abort the deploy.
- **One mutex around login execs,** so the timer, a leadership gain and a deploy cannot write
  `config.json` concurrently; `DOCKER_CONFIG=/root/.docker` is set explicitly because under systemd
  `HOME` may not point where docker looks; the token still rides `--password-stdin`, never argv.
- **Kept from `75d4014`:** `internal/githubapp`, the AES-GCM-encrypted app registration, the
  HMAC-authenticated `POST /api/github/token` broker, and the `github app` doctor check. The check mints
  through the daemon's own client against GitHub (no helper, no extra process); a test runs it with an
  empty `PATH` to prove that.
- **Docs state the limitation plainly.** `github-app.md` §5, `registry-credentials.md`, `health.md` and
  the architecture note describe what runs — the timer, the leadership-gain login, the WRN line — and the
  unchanged gap: `--with-registry-auth` freezes the credential into each service spec, that copy dies 60
  minutes later, and nothing renews it. Pre-pull and an in-cluster registry mirror are the acknowledged
  fixes and are not implemented.
- **Guard test:** the inverse of the old "the timer is gone" assertion — no source, doc, OpenAPI or unit
  render may name the removed helper, its port or its config key again.

## Unreleased — GitHub App: installation tokens for pulls and CI

**The cluster can hold one GitHub App instead of a personal access token in every repository. It mints
one-hour installation tokens, uses them as the GHCR pull credential, and serves them to GitHub Actions
from a per-repo HMAC-authenticated endpoint — so a workflow can delete `secrets.GH_PAT`.**

Nine repos carried a personal OAuth token through a global `git config insteadOf` rewrite. When it
expired, `go mod download` broke in all of them at once, silently. A GitHub App installation token is
short-lived (one hour), minted on demand, scoped by the App's own permissions, and revoked by
uninstalling the app — none of which is true of a personal token copied into nine secret stores.

- **`internal/githubapp`** signs a 9-minute RS256 JWT (`iat` now−30s, `exp` now+9m), lists
  installations, mints installation tokens, and reads the granted permission set from `GET /app`.
  Errors wrap four sentinels — bad key, installation removed, token rejected, insufficient permission —
  so the console and `doctor` name the cause instead of surfacing a 403 later.
- **One minted token per installation, refreshed at 50 minutes.** The cache is mutex-guarded across the
  HTTP call: ten concurrent pulls mint one token (asserted with a counting fake server). A failed mint
  is not cached, so the next caller retries rather than inheriting the failure for an hour.
- **Storage:** `github_app` (singleton; app id, installation id, AES-GCM private key, probe targets) and
  `github_broker_secrets` (one AES-GCM HMAC secret per repo). Both are in the control-plane kit, so a
  promoted leader can still pull images and serve CI. The derived token is never written to `data.db`
  or a rendered config; a test reads the DB file and asserts the token does not appear in it.
- **Pulls:** when an app is configured the daemon runs `docker login ghcr.io -u x-access-token
  --password-stdin` with the installation token at startup and every 25 minutes, and skips the stored
  `ghcr.io` row. No app, or a failed app login, falls back to the existing `pmcluster registry`
  credential unchanged; other hosts are untouched. `--with-registry-auth` forwards the login to workers
  exactly as before.
- **CI broker:** `POST /api/github/token`, authenticated by a per-repo HMAC secret (the webhook
  receiver's `sha256=<hex>` over timestamp+body scheme, 5-minute skew) — deliberately **not** the daemon
  bearer token, which would put control-plane credentials in every repo. Rate-limited per repo; every
  auth failure is one 401. Response is `{token, expires_at}`. Installation tokens cannot be narrowed,
  so the App's granted permissions are the documented ceiling and are reported in the console.
- **CLI:** `pmcluster github app set|show|clear|test`, `pmcluster github broker add|rotate|list|remove`.
  The key is never displayed; broker secrets are printed exactly once.
- **Console:** a Settings card shows the app id, installation id, probe targets and the granted
  permission map, warns when `contents:read` or `packages:read` is missing, and has a Test button that
  mints and probes. It never displays a token.
- **`pmcluster doctor`:** a `github app` check mints a token and reads a repo / HEADs a package.
  No app is a skip, not a failure; a rejected key, a removed installation and a missing permission are
  distinct failures; a token with no probe target is a warning.
- **Docs:** `docs/github-app.md` (manual app creation, permission table, the exact workflow snippet,
  rotation, failure modes), plus updates to `registry-credentials.md`, `health.md`,
  `secrets-and-configs.md`, and `openapi.yaml` for the three new routes.

Tests cover JWT claims/signature verification, permission ranking, error mapping for 401/403/404, the
one-mint-under-ten-callers guarantee, refresh timing, log redaction (token, JWT and PEM body absent from
captured output), broker auth/429/502 paths, store round-trips, registry fallback selection, the doctor
message matrix, and the DB-holds-no-token assertion. No test reaches github.com: every API call goes to
an `httptest` server.

## Unreleased — content-addressed console assets

**A deploy can no longer leave an open console running the previous stylesheet for an hour, and a changed asset is now cached for a year, not sixty minutes.**

`/web/static/*` was served with `Cache-Control: public, max-age=3600` under a constant filename. After
v0.2.263 shipped the tooltip CSS the operator's already-open console kept the previous `components.css`
for up to an hour: the HTML carried 26–181 `data-tip` attributes and the rules that render them were never
fetched, so a shipped feature looked broken. The same defect hid every CSS/JS change after every deploy.
Every asset URL now carries `?v=<hash of the bytes served>`.

- **Templates link through `{{static "name"}}`, including the favicon.** All four documents (shell, login,
  setup, terminal) were converted; a test fails if any template hardcodes `/web/static/`, so a stylesheet
  added later cannot silently ship unversioned.
- **The hash is of the bytes actually served.** `internal/ui/views/assets.go` hashes each embedded file and
  rewrites `fonts.css` so its `@font-face` URLs carry the font's own version; the stylesheet's version is
  then computed over the rewritten body. A font change busts the font URL, `fonts.css` and every page that
  links it.
- **Policy follows the URL.** A matching `?v=` gets `public, max-age=31536000, immutable`; an unversioned
  or stale version gets `no-cache`. Every response carries the content's `ETag`, so revalidation is a 304.
  A stale hash is not a 404 — the current bytes are served, with revalidation — so the cache-buster is
  never a hard dependency.
- **The daemon and the edge agree.** Both binaries render the same templates from the same embedded FS and
  build the version table once at startup, so a console served by the edge and an API served by the daemon
  emit identical URLs.

Tests: a changed file changes its version and an unchanged one does not; two renders of the shell produce
identical URLs; every `<link rel="stylesheet">`, `<script src>` and icon link in each of the four documents
carries a version, counted rather than spot-checked; a hashed URL is immutable while a bare or stale one is
`no-cache` and still returns 200; a conditional GET answers 304; a font change changes the `fonts.css`
body, its version and the URL it emits; an unknown path still 404s.

## Unreleased

**A service mode change no longer aborts the deploy that contains it, and the platform stops accepting
manifests that can only fail.**

`docker service update` cannot change a service's mode — Swarm answers `Unimplemented: service mode change
is not allowed` — and `docker stack deploy` issues that same update, so one `global` → `replicated` flip
aborted the whole stack: the edge console stayed on the old image fleet-wide while every other service in
the deploy was skipped. Three smaller lies of the same kind: the apply-warning banner printed its printf
verbs, `mode: global` was accepted in customer manifests, and `placement: <typo>` produced a service that
could never schedule and said nothing.

- **Mode changes are applied by remove + recreate.** Before a stack deploy, every rendered service's mode
  is compared with the running service's; where they differ the service is removed (once), logged with the
  service name and the mode pair, and left to the deploy to recreate. Identical modes and non-mode changes
  keep the normal update path. A failed remove is an apply warning, not a fatal — the deploy still runs.
  This closes the follow-up recorded in v0.2.261.
- **The apply-warning banner is formatted at render time.** `overview.apply_warnings_title` and
  `overview.apply_warnings_when` carried `%d` / `%s` that nothing substituted, so the console read
  `The last platform apply could not finish %d step(s)`. They now use the dictionaries' positional
  placeholders (`{0}`, `{1}`), which keeps Arabic word order with the dictionary while the renderer
  supplies the real count, timestamp (`2026-11-08 22:41 UTC`) and actor. `overview.version_skew_title` had
  the same defect and is fixed with it.
- **`mode: global` is refused in customer manifests.** One task per node multiplies a service's resources
  and collides on its published ports, with no legitimate use in an application stack. The mapping is not
  deleted: `infra-stack.yml` renders the Traefik gateway as `mode: global` through the same DSL, so the
  capability stays for `platform: true` stacks and validation refuses it for everything else, naming the
  field and the reason.
- **`placement: <hostname>` is checked against the swarm at deploy.** A hostname pin renders
  `node.hostname == <value>`; a typo created a service that never scheduled, silently. The deploy now
  reads the node list and refuses with the unknown host and the known ones. Role keywords (`manager`,
  `worker`) and an empty placement are skipped; an unreadable node list degrades to a warning instead of
  blocking a legitimate deploy.

Tests: a mode difference removes exactly once and before the deploy; an identical mode removes nothing; a
non-mode change removes nothing; a failed remove warns and the deploy continues. Three warnings render the
digit and no `%d`; zero warnings render no banner; the recorded-at line carries a formatted timestamp and
the actor (English and Arabic). A customer manifest with `mode: global` is refused while a `platform: true`
one still renders it. An unknown placement host errors and does not deploy; a known host deploys; a role
and an empty placement pass; a node-list failure degrades.

## Unreleased — platform bind-mount host directories

**The platform creates the host directories its own stacks bind-mount, so a missing path can no longer reject a task.**

The OTel collector bind-mounts `/var/stack/nodestat` from the host (it appends the node-stats JSONL there and the leader's daemon tails it). On nodes that had never run the node-stats code the directory did not exist, and Swarm refused the task outright — `Rejected  invalid mount config for type "bind": bind source path does not exist: /var/stack/nodestat` — leaving the collector at 0/1 with no container log to explain it. The operator created the directory by hand on four hosts. Provisioning a host path a platform stack mounts is the platform's job.

- **`cluster up` and `cluster update` create them, before the stack is deployed.** `internal/cluster/hostpaths.go` walks the platform's bind sources (`ensureHostDir` per path, `os.MkdirAll` of the parent chain), idempotent and a no-op for an existing directory — an operator's mode is never rewritten. `cluster up` fails on the step (a fresh install has no healthy cluster to preserve and the message names the path before anything is deployed); `cluster update` prints a `⚠` line naming the path and the reason and carries on, so one uncreatable path cannot abort an otherwise healthy reconcile. The warning reaches the apply record through the existing sink.
- **The path is one constant, not three literals.** `cluster.NodeStatHostDir` feeds the observability stack's bind mount and the collector config's file-exporter path (`[[ .NodeStatDir ]]`, defaulted in `LoadComposeFile`/`RenderOTelCollectorConfig`) and the `mkdir`; the daemon's reader (`internal/cli/nodestat.go`) now derives its default from `cluster.NodeStatFilePath()` too. A test asserts the rendered bind source equals the directory the helper is given, and that the exporter path is inside it.
- **No role check.** The directory is created on whichever node runs the command. It is harmless where nothing mounts it, and the leader's daemon is what reconciles, so the node that needs the path is the node that runs the mkdir.
- **The rest of the observability stack's bind sources need no help**, and this was audited: `/var/lib/docker/containers` and `/var/run/docker.sock` are Docker's own, `/etc/localtime`, `/proc` and `/sys` are stock Linux, and Parseable's data/staging dirs are named volumes the deploy pipeline already creates under the volume root. `/var/stack/nodestat` was the only one in this class.

Tests: the helper creates a missing directory and is a silent no-op (mode preserved) on an existing one; it names the path and carries the reason when the parent is unwritable (`permission denied`) and refuses a regular file where a directory is needed; `ensureHostDirs` reports every failure. `Up` calls it exactly once and before the observability stack is deployed, and fails naming the path when it cannot; `Update` calls it once, reports the directory as ready, and turns the same failure into an apply-recorded warning with no error return. The rendered bind source, the helper's list and the exporter path are asserted to be one value.

## v0.2.262 (2026-11-09)

**Storage membership stops following leadership, an empty-valued node label stops existing, and leadership can be moved on purpose.**

Tonight's incident had two halves. Leadership moved from `nxt-sw-4-m` to `nxt-sw-5-m`; `cluster update`
then reported *"the former leader nxt-sw-4-m is no longer a storage node"* and *"the leader nxt-sw-5-m
must be a storage node … added to storage_nodes"*, rewriting the operator's `storage_nodes` set. The
backup agent silently left one node and appeared on another. Separately, `nxt-sw-3-w` carried
`pmcluster.storage: ""` — a key that exists with no value, which any presence test reads as a storage
node.

- **Storage membership no longer follows leadership — but the leader invariant stays.** The reconcile step
  still re-adds the current leader to `storage_nodes` (the edge and its local backup agent run there), and a
  worker is still pruned (it cannot read settings, credentials or the stack list). What is gone is the
  OTHER direction: a node that loses leadership is no longer removed from `storage_nodes` or stripped of its
  label — a manager that stops leading is still a manager, and an election is not consent to move the
  backup agent. The `storage_leader` cursor that drove the removal is gone.
- **No empty-valued labels.** `SetNodeLabel` with an empty value removes the key (it already did; a test
  now pins it, and the docker adapter is exercised through a mock daemon). Reads compare the value —
  `cluster.IsStorageNode` — instead of testing the key's presence, so a stale `pmcluster.storage: ""`
  cannot make a worker look like storage.
- **`pmcluster doctor` reports empty-valued `pmcluster.*` labels** (`node labels`, warn): the key's
  presence is the bug, so the fix is to clear it (`docker node update --label-rm`).
- **`pmcluster cluster leader <node>`** (and `POST /api/cluster/leader/{hostname}`) moves leadership
  deliberately: it demotes the current leader, lets the survivors elect, promotes the old leader back,
  repeats until the target wins (bounded to three elections and 25 s), and reports the leader that
  actually won. It refuses a non-manager target, refuses when the temporary demotion would leave fewer
  than three managers, and refuses without quorum — the same refusals `node promote/demote` already use.
  This is how failover behaviour is tested without taking a node down; nothing automatic calls it.

Tests: the leader is added to `storage_nodes` when missing; a manager that loses leadership is kept
(the regression — byte-identical set, no label cleared); a worker is still pruned; an apply that changes
nothing leaves the set untouched; an empty label write removes the key and a no-op clear writes nothing;
`SetNodeRole` demotes and re-promotes through the Docker node spec; doctor names the empty-valued label;
`cluster leader` refuses a worker, refuses below three managers, refuses without quorum, and reports the
new leader (CLI, API handler and library). `openapi.yaml` documents the route with its own
`LeaderMoveResult` schema.

## v0.2.261 (2026-11-09)

**The OTel collector follows the leader by constraint, not by preference, so the node-stats panel stops reporting an empty fleet.**

The collector writes `/var/stack/nodestat/nodestat.jsonl` to its OWN host disk and the leader's daemon tails that path locally. Its placement was a manager constraint plus a `node.labels.pmcluster.leader` *preference* — and a preference is a tiebreaker, not a pin. Measured on the fleet: `nxt-sw-5-m` carried `pmcluster.leader=true`, the collector task sat on `nxt-sw-4-m` and stayed there across `docker service update --force`, so the JSONL was written on a node nobody reads and `GET /api/node-stats` answered `{"node_stats":[]}` with no error anywhere.

- **The leader label is a CONSTRAINT** (`node.labels.pmcluster.leader==true`, alongside `node.role == manager`) and the preference is gone. The rendered stack is asserted to carry the constraint and no preference on that label.
- **The label is now an invariant, not a best-effort hint.** The daemon stamps it on leadership gain (including a daemon that starts while already leading — the leadership watch emits its initial state, so the first pass asserts it), re-asserts it every 30 s while it leads, clears it on loss, and clears a stale copy from any other node that still carries it (two matches would let the scheduler pick the former leader). Only the leader manages another node's copy.
- **Deploying without the label is refused, loudly.** `cluster update` returns `no node carries pmcluster.leader=true — the leader must claim it before the collector can be pinned` before it deploys the observability stack, instead of leaving a service at 0/1 with no task logs. `cluster up` CLAIMS the label for the current leader first, because the daemon that maintains it is installed and started after that command. `pmcluster doctor` gained a `leader label` check: fail when nothing carries it, warn on a stale/duplicate/mismatched carrier.
- **A placement change now moves the running task.** `docker stack deploy` rewrites the service spec but leaves a healthy task where it is, so the reconcile pass compares the collector's stored placement with the fresh render and force-updates `observability_otel-collector` when constraints or preferences differ (and stays silent when they do not).
- Known follow-up, unchanged by this release: `docker service update` cannot change a service's MODE (`global` → `replicated` is `Unimplemented: service mode change is not allowed`), which aborted a whole `cluster update`; a mode change still needs remove + recreate.

## v0.2.260 (2026-11-09)

**The control-plane kit keeps a small rollback window instead of the full history or none.**

`v0.2.259` stopped the kit from carrying `stack_revisions` at all, and the operator's follow-up was
right: a promoted leader that knows stack X is on revision 42 but has no row 42 cannot reconcile or
roll that deploy back until someone re-deploys, and dropping `config_versions` outright would take
`config rollback` away from the new leader. Both tables are now **windowed**, not dropped.

- **`stack_revisions`: the newest 5 revisions per stack, plus the row `current_revision` points at.**
  The pointer is not always among the newest — a rollback leaves it on an older row while the newer
  ones stay in history — so "newest five" alone would still lose exactly the manifest a rollback needs.
  The copy is a per-stack `ROW_NUMBER()` over `revision DESC`, then the current row added with
  `INSERT OR IGNORE`.
- **`config_versions`: the newest 10 versions per config name** (`ROW_NUMBER()` over version-row id
  `DESC`, partitioned by `config_id`). Row ids are copied unchanged, so a rollback on the promoted
  leader names the same version row it would have on the old one. Nothing is deleted from the leader:
  these are copy policies, and a rollback further back than ten versions is a leader-side operation.
- **Both caps live in one place** — `internal/controlplane/kitdb.go` — with a comment naming the
  operator's numbers; the per-table reasons sit beside them and a schema test still fails when a
  migration adds a table without a decision (`kitTables` / `excludedTables`).
- **`store.Export` is the new primitive** (plans: `CopyAll`, `CopyNewestStackRevisions`,
  `CopyNewestConfigVersions`); `store.ExportDB` remains a verbatim wrapper, so its tests are unchanged.
  The statements are composed from a closed selector set and an identifier-checked table name, so a
  plan picks a policy and cannot inject SQL. A `Keep` below 1 is rejected rather than treated as "all".

Measured on a read-only copy of the live leader's `data.db` (5,357,568 bytes; 18 stacks, 548
revisions, 19 configs, 50 config versions): the kit was **84,739 bytes (16.55 %)** with no revisions
and the whole `config_versions` table; it is **120,033 bytes (23.44 %)** with both windows applied —
90 revision rows (5 × 18) and 48 config-version rows. The increase is the rollback window the
operator asked for, and it sits under the 40 % budget rather than near the 512,000-byte ceiling.

Tests: 500 revisions per stack on two stacks plus 200 config versions on two configs still pack at
**4.2 %** of the limit with exactly 5 revisions per stack and exactly 10 versions per config; the
10,000-revision fixture packs at 3.0 % while the same fixture with every row exported is 22× the
limit; a stack whose `current_revision` is the middle of three carries it with its rendered manifest,
and a rolled-back stack whose `current_revision` (3) is older than the newest five (4-8) still crosses
its current row — the case where "newest N" alone would lose exactly the live manifest — while
`Revision()` on a row outside the window returns `ErrRevisionNotFound` (asserted by `errors.Is`); `config_versions` is capped per config name, the oldest surviving row is the tail of
200, and the leader's own table keeps all 204 rows. Everything else keeps its previous fidelity
assertions (token hashes, credential/webhook/registry ciphertext bytes, settings, config content,
secret payload, site-cert metadata, failover marker, stack list), and adoption still serves the same
control plane through the handle it already holds — now with the current revision readable.

## v0.2.259 (2026-11-09)

**The control-plane kit carries the authoritative tables instead of the whole `data.db`, so Raft replication works again.**

Measured on the leader: `data.db` 5,218,304 bytes, the kit 538,446 bytes gzipped against Docker's
`500 KiB` config cap — so **every publish was refused**, 70 times in six hours
(`control-plane state payload 538446 bytes exceeds the 512000-byte Docker config limit`). No snapshot
had been publishable for a long time, which means every API key, setting, secret and credential created
since the last accepted one existed **only on the leader's disk**. `b60c58c` (adopt on leadership gain)
could not help: there was no fresh snapshot to adopt, and the offsite tarball was the only copy, hours
behind.

3.7 MB of that file was `stack_revisions`. The operator's decision: **revisions do not need to cross the
fleet** — a promoted leader re-renders them from the repo — while users, keys/tokens, credential
ciphertexts, settings, webhook secrets and *which stacks exist* do.

- **The kit's `data.db` is now an export** (`internal/controlplane/kitdb.go`, `store.ExportDB`): a fresh
  SQLite file with the migrations applied and the rows of the authoritative tables copied in one read
  transaction. `VACUUM INTO` was rejected — it copies everything, which is exactly what does not fit.
- **The table list is a decision in code**, with a comment per table: in are `users`,
  `managed_credentials`, `stacks`, `webhook_sources`, `registries`, `cluster_settings`, `configs`,
  `config_versions`, `secrets`, `site_certs`, `stack_failover`; out are `stack_revisions` (deploy history,
  re-rendered from the repo), `backups` (2775 audit rows; archives are re-discovered from disk),
  `webhook_deliveries`, `stack_status` (rebuilt by the first reconcile pass) and `node_stats` (volatile).
  `config_versions` was the genuinely ambiguous one — 258 KB live, 36 KB gzipped — and is **kept**, because
  service-scope config history edited in the console is in no repo and dropping it would take away
  `config rollback` on the promoted leader. Correctness beats bytes: 36 KB is not what breaks the cap.
- **The limit check stays, and the size is now always visible**: the packed payload size is logged at info
  on every publish (`bytes`, `limit-bytes`, `limit-pct`), so growth is observable before it becomes a
  refusal. Above the cap the existing error path is unchanged.
- **A promoted leader degrades cleanly without revisions.** `stacks.Sync` on a stack whose revision is
  absent returns an error wrapping `store.ErrRevisionNotFound` instead of a bare "no revisions"; `reconcile`
  classifies it as `skipped` (new telemetry status, so it is not counted as an error), logs once at info
  per stack and **does not** record a stack error or mark the stack errored; `move` says to re-deploy
  first rather than "not found". Everything else — auth, settings, secrets, configs, the stack list, badges
  from live swarm — is unaffected. An absent revision is normal, not corruption.
- **`data.db-journal` / `-wal` / `-shm` are excluded from the kit.** Packing a sidecar next to a *different*
  `data.db` would give the promoted node a hot journal to replay into the exported file.
- If the export itself fails (a `data.db` that is not a readable database), the kit falls back to packing
  the file verbatim and warns — a kit carrying too much still beats no kit at all, and the size log makes it
  visible immediately.

Tests: a 10,000-revision database (incompressible payloads) produces a kit at **1.5 % of the limit**, while
the same fixture packed *with* revisions is **22× the limit** — the same test fails if revisions are
included, and asserts the excluded tables are empty in the packed file. Fidelity: every authoritative row
survives pack→unpack with row counts plus token hashes, roles, credential/webhook/registry ciphertext
bytes, settings, config content and kept versions, secret payload, site-cert metadata, failover marker and
the stack list. Adoption: a manager adopting a slim kit serves the same users, keys and settings through the
handle it already holds, lists the same stacks, and reports a missing revision as `ErrRevisionNotFound`.
Reconcile: a stack row with no revision is left healthy, gains no error record, and is announced once, not
per pass (verified to fail without the classification). Store: `ExportDB` copies only the named tables, the
result opens through the daemon's store with nothing left to migrate, a non-database source is refused
without leaving a partial file, and unsafe table names are rejected. A schema test fails when a migration
adds a table without classifying it. Observability: the logged payload size equals the published bytes.

## v0.2.258 (2026-11-09)

**The console overview now shows, per node, what is eating that machine — and it shows it while telemetry is broken.**

On 2026-11-08 `nxt-sw-4-m` went `Down` at load 9.96 with the pmcluster daemon at 902 MB RSS on a 1839 MB box.
Nothing in the console said so: node health came from the Swarm API (which reported a healthy-looking node right
up to the heartbeat it lost), and the numbers that would have named the culprit — per-process RSS and CPU — were
never collected anywhere an operator could read them during the incident.

The overview gained a **Node resources** panel: one row per node with load, memory, the top five processes by
memory and by CPU, and the age of the sample.

- **Each node's daemon samples its own `/proc`** (`internal/nodestat`) every 15s — load, MemTotal/Used/Available,
  cores, Docker reachability, top-5 RSS and CPU — and exports it as OTLP metrics. `hostmetrics` can only read its
  own host, so it could never answer this. The daemon still never talks to Parseable; routing stays the
  collector's job.
- **The collector is no longer `mode: global`.** It is one task with a `node.role == manager` constraint and a
  `node.labels.pmcluster.leader` *preference*, so the task moves with leadership but still schedules when no node
  carries the label (a constraint there would strand it at `no suitable node`). The daemon stamps the label on
  gaining leadership and clears it on losing it. `docker_stats` and `filelog` now describe the leader's
  containers only, which is the accepted cost of one reservation instead of four on 2 GB nodes.
- **The node-stats pipeline writes a JSONL file** (`file` exporter, `/var/stack/nodestat/nodestat.jsonl`,
  rotation 2 MiB / no backups) as well as Parseable. The leader's daemon tails it by byte offset + inode, rebuilds
  from the last 256 KiB on rotation, buffers a torn trailing line, skips and counts malformed lines, and reduces
  to the newest sample per node. It never truncates the file — the exporter holds it open, and truncation leaves
  a sparse NUL-filled middle.
- **`node_stats` is one row per node**, `INSERT … ON CONFLICT(hostname) DO UPDATE`. The table is O(nodes)
  forever; a departed node's row is deleted, a silent node's row ages instead.
- Four tint rules on the panel: load > 1.5x cores, available memory < 15%, sample older than 3 minutes (greyed
  *and* labelled stale), Docker unreachable. Both themes, all six palettes, en/ar, RTL via logical properties.
- `GET /api/node-stats` serves the rows from the control-plane DB, and the overview reads it in the same wave as
  its other five reads.

Tests: reader (unchanged file, torn line, rotation, shrink, malformed line, newest-wins, top-5 with ties and
fewer than five, silent node), store (one row across 50 ticks, restart, prune), collector/stack render (pipeline,
rotation values, manager constraint, leader preference, the node's own swarm address), the rendered panel
(numbers, greyed stale row, empty state, tint boundaries), and an e2e that pushes a collector-shaped fixture with
several nodes, several samples, a torn line and a rotation through the real reader, store, API and renderer. One
fixture is the *actual* output of `otel/opentelemetry-collector-contrib:0.157.0` fed the *actual* bytes the Go
SDK exports, so a format drift fails in a unit test instead of as an empty panel.

## v0.2.257 (2026-11-08)

**A manager that GAINS leadership now adopts the newest control-plane kit instead of serving its own old database.**

Measured on the real cluster the same day: `nxt-sw-4-m` saturated (load ~10 on one vCPU), Docker stopped
answering heartbeats, Swarm marked it `Down`, and leadership moved to `nxt-sw-5-m`. 5-m then answered **401**
to an API key created minutes earlier on 4-m, because 5-m was serving its own `data.db`. The newest kit was
already replicated to it — nothing applied it: `Kit.Restore` had exactly one caller, the startup path, which
runs before the daemon waits for leadership. A node that becomes leader while running never re-checked.

Adoption now runs the same predicate at two call sites: startup (before `store.Open`) and once per
follower→leader transition (`Kit.AdoptOnLeadership`, from the leadership goroutine, before the reconcile loop
reads settings and before the node answers control-plane reads). The mid-run case stages the kit in a
`.adopt-*` directory inside the data dir and then closes the store (`store.Store.Swap`), renames the staged
`data.db` and `.encryption_key` into place with `data.db` last — one atomic rename — writes `.state_revision`,
and reopens the same `*store.Store` every consumer already holds. A changed key reloads `credentials.Cipher` in
place, since a cluster DB beside the node's old key leaves every ciphertext undecryptable.

- Once per transition, never per tick: a node that stays leader does not close its store; the tick path is
  unchanged.
- Failure is contained: an unreadable kit or a state config without its key config logs ERROR with the reason
  and keeps serving the current database. Nothing is ever half-extracted.
- A snapshot this node published itself is never adopted back.
- While adopting, the daemon answers 503 + `Retry-After` for everything but `/health`, so no request is served
  from the revision being replaced. The window is one extract plus one rename.
- The stale comment in `controlplane/state.go` claiming the leader loop calls restore "on every tick and every
  promotion" is corrected: no such call existed, and now the two real call sites are named.

Tests assert the behaviour and the call counts: stale revision adopts and the reopened handle serves the
adopted rows; a current revision performs zero extractions; two transitions perform exactly two checks and one
extraction; a missing key config is refused with the DB byte-identical and an ERROR log; an unreadable kit
leaves the DB untouched with no staging left behind; tick churn never swaps the store; a self-published
snapshot is skipped.

## v0.2.256 (2026-11-08)

**Each console page now issues its reads in one wave, and says so while it waits.**

The observability page fetched `summary`, `evidence` and `traces` one after another:
1.88 s + 1.50 s + 1.29 s = 4.67 s of backend work inside a page that rendered in 4.72 s
— the page *was* its own sum. The reads are now issued together, so the page costs
roughly its slowest read. Measured against the live API through a local console:

| page / fragment | before | after |
|---|---|---|
| `/web/overview` | 1.09–1.29 s | **0.42–0.60 s** |
| `/web/observability` | 4.72 s cold | **1.83–2.43 s** |
| — `evidence` | 1.50 s | **0.45–0.59 s** |
| — `traces` | 1.29 s | **0.64–0.74 s** |
| `/web/stacks` | 0.50–0.65 s | **0.13–0.16 s** |

`summary` (1.45–1.60 s) did not move and is now the page's floor: the observability
page cannot render faster than it until that query does.

A visible in-flight state was added so a multi-second panel is not a silent one. Note
that the loading state is the thin part of this release: the shell carries a spinner and
the styling, but the per-fragment indicators were verified only in part before the change
was committed, so treat B as shipped-but-unproven rather than measured.

## v0.2.255 (2026-11-08)

**The read cache could not hit, because it was keyed on the clock.**

`windowKey` used absolute `Start`/`End`, while the console and the API derive
windows from the clock ("the last hour"). Every request therefore produced a
different key, and the cache served only callers that were in flight at the same
instant — which the in-flight join already covered. Measured on the leader before
the fix: three back-to-back requests for 100 alerts took 0.655 s, 0.577 s and
0.737 s, i.e. three upstream reads.

The key is now the window *length* plus the end floored to the cache TTL, so any
request ending inside the same 10 s bucket shares an entry. Worst-case staleness
is the TTL that was already promised. Two tests cover it: windows one second
apart must share an entry (they failed before this change, passing after), and
one-hour and 24-hour windows over the same end must not.

The unit tests did not catch this because they used identical `Window` values.
Production never repeats a window.

## v0.2.254 (2026-11-08)

**Alerts joined the read path, retention regained its defaults, and a burst now costs one read.**

`GET /api/observability/alerts` exposes Parseable's alert list (limit <= 500) behind
the same bounded envelope as logs, metrics and traces. Because the edge MCP
catalogue is derived from `openapi.yaml`, documenting the route *is* what creates
the tool: `pmc_observability_alerts` now sits beside `pmc_observability_logs`,
`_traces`, `_node_metrics` and `_service_metrics`, with `limit` bounded at 500 in
the schema an agent reads.

Retention was never hardcoded — `observability_logs_retention_days` (30),
`observability_metrics_retention_days` (14) and `observability_traces_retention_days`
(10) were already wired into the provisioner. What was wrong is the fallback: an
unset env var made the script a no-op instead of applying the default, so a stack
that lost its environment silently stopped enforcing retention at all. The script
now falls back to the same numbers the settings report.

The read path gained `CachedReader`: 10 s TTL, 64 entries, longest-standing entry
evicted first, and two deliberate exemptions — `Since`-bound reads (the console's
incremental live poll) always reach the source, and an unavailable backend is
never memoised, so an outage recovers on the next request instead of being cached
for ten seconds.

Two bugs surfaced only because the tests counted calls rather than checking that
output looked right:

- The cache did not join concurrent misses, so a burst of identical requests paid
  Parseable once per caller. It now coalesces in flight: one read, N waiters.
- `decodeAlertRows` returned an empty list for a body with no recognised envelope,
  making "no alerts" indistinguishable from "not an alert list". It now errors;
  `{"alerts":null}` remains a legitimate empty list.

107 new test cases, including an e2e that runs the real daemon, real bearer auth
and a read-counting stub to prove two back-to-back requests produce one upstream
read. Reverting either fix fails its test.

## v0.2.253 (2026-11-07)

**Three charts asked Parseable for something its engine refuses.**

The console grouped by SELECT aliases (`GROUP BY bucket, level`), which Parseable
rejects outright because it runs DataFusion:

```
Datafusion Error: Column in SELECT must be in GROUP BY or an aggregate function:
... currently only pmcluster-logs.level, count(Int64(1))
```

The whole query answers 400, so the panel rendered "telemetry source
unavailable" — it looked like a missing data source, not a broken statement. Three
builders were affected: the overview's **log volume by level**, the **node CPU
and memory** charts, and the **trace list**. All three now group by the
expression they select, which the same engine accepts.

Verified against the live cluster, one-hour window: log volume 183 rows in 0.27s,
node metrics 729 rows in 0.23s — both previously 400s.

The tests that pinned the old strings were pinning the bug; they now assert the
expression form, and `groupby_test.go` fails any future builder that groups by an
alias, because that form reads better and someone will reach for it again.

## v0.2.252 (2026-11-07)

**The observability page stops being slow.**

Measured before: a Parseable log query costs 0.28s over a one hour window and
2.88s over 24 hours, a metrics aggregate 0.31s against 3.27s — there are no
rollups, so cost follows the window. The console assembled the page from
sequential round trips, which made a 24 hour logs tab about 6.5 seconds, and
live mode then re-read the whole window every five seconds: longer than its own
interval, so the panel never settled.

- **The two investigation queries run in parallel** — newest errors, then newest
  of any level — instead of queueing. A failure in either still fails the whole
  read, so one call succeeding can never mask the other.
- **The served range defaults to `1h` explicitly**, so the selector shows a real
  choice instead of an unselected list.
- **Live mode is incremental.** `LogQuery` gained a `Since` bound, the Parseable
  adapter emits a strict `p_timestamp` predicate combined with the window
  bounds, and each log row carries its timestamp as `data-ts` so a poll asks
  only for what arrived after the newest row on screen. The response is rows
  only, appended into the list rather than replacing the panel, so scroll
  position, filters and search focus survive a refresh.
- **A hidden tab stops polling entirely**, as does a tab where the search box
  has focus. A backgrounded console now makes no Parseable requests at all.
- **Evidence is cached for ten seconds**, keyed by service, range, severity,
  search and offset. Live and incremental requests bypass the cache — they are
  fresh by definition.
- **The button edge, in dark theme.** A primary button's fill sat at 1.8-2.8:1
  against the card, under the 3:1 non-text guidance: the label was readable, the
  button was not. The boundary now comes from a dedicated `--control-edge` token
  (the palette's light tint in dark, the fill itself in light). Measured after:
  6.77-9.78:1 in dark, 6.3-11.93:1 in light, white-on-fill unchanged.

## v0.2.251 (2026-11-07)

**A way to see what belongs to nothing.**

- **Services that carry no stack were invisible to the whole platform.**
  `docker stack ls` does not list them, `pmcluster stack list` does not know
  them, and the reconcile never touches them, so nothing scaled, updated or
  cleaned them up while their tasks kept using the node. One was sitting on the
  cluster at `0/1`. `pmcluster cluster orphans` reports them — name,
  running/desired, mode, image — and `--remove` deletes them.
- **Only stack-less services are ever candidates.** A service that carries a
  stack label is left alone even when its stack is gone: it belongs to a deploy
  that is still converging, and removing it would race that deploy. Removal is
  sequential and exits non-zero on any failure, so a scripted cleanup cannot pass
  on a partial result, and a completed one-shot reads `0/1 (run-once)` rather
  than the alarming bare `0/1`.

## v0.2.250 (2026-11-07)

**Verbosity you can set, and a console worth looking at.**

- **The daemon logged without a level.** Every debug line reached stderr and
  nothing could be filtered, because the process never set a zerolog level. There
  is now `log_level` (default `info`, values `debug|info|warn|error`, `warning`
  as an alias), read at startup, re-applied live when saved, and rendered into
  the platform stack as `PMCLUSTER_LOG_LEVEL` — the one env var both pmcluster
  binaries read. An unknown value logs one warning naming it and runs at `info`
  rather than refusing to start.
- **Deployed apps log `INFO` for every request, and nothing could change that.**
  Every service of every app stack now receives `LOG_LEVEL`, settled in order
  from the manifest's `log_level`, the cluster's `app_log_level` setting
  (default `warn`), then `warn`. A service that declares `LOG_LEVEL` itself keeps
  it: the platform supplies a default, not a policy. Platform stacks are not
  injected — their components configure their own verbosity. The value is
  written at the IR/compose-writer boundary, never by patching the manifest.
- **Traefik exported a span per proxied request for no consumer.** The infra
  stack now runs with `--tracing.otlp=false`. Access logs were already off.
- **The console ran on cold zinc surfaces with an indigo accent and a dark
  default** — generic SaaS, unrelated to the product. Both themes are retokenised
  to warm paper, deep green as the workhorse accent and gold as the brand
  highlight (never body text: ~2.9:1 on paper), with the dark theme derived as a
  warm counterpart. Light is now the default.
- **The six-palette picker recoloured only the logo mark.** It now drives
  `--accent`/`--accent-strong` through 12 theme × palette blocks, with green as
  the default. Every combination was measured: the weakest is 5.78:1 for accent
  on the body and 6.30:1 for white on a filled control.
- **Reading the console at a glance.** The overview's lead cell carries the
  node's headline state as a bar and tint, and status is a pill with a dot, so a
  state is never colour alone. Tables got one treatment — sticky header,
  right-aligned `tabular-nums` numerics, truncation paired with a title, hover
  and selected rows — and forms got field-local errors (`aria-invalid` +
  `aria-describedby`) on setup and the user form.
- **"needs manager" was bare text in an actions column**, indistinguishable from
  an empty cell. The storage refusal now carries a lock and reads as a disabled
  control.
- **`backup-agent prune-remote`** trims remote objects the local pruner can
  never see — `--target store|s3|all`, `--legacy`, `--dry-run`. Retention only
  ever ran against local archives, so 1076 legacy objects in the IONOS bucket and
  357 in R2 were invisible to policy.

## v0.2.227 (2026-11-02)

**Fixes for v0.2.226, all found by CI rather than by reading the diff.**

- **A node that is not in a swarm is not leader-gated.** The guard conflated "not
  the leader" with "not in a cluster", so on a bare node it refused cluster-state
  commands with *"this node is a node, not the leader"* — on the one node you
  bootstrap from. `docker info` reports no leadership because there is no cluster
  yet, and the daemon already serves in that state as *standalone/local mode*. The
  scope probe now reports `SwarmActive` separately, and only membership in a swarm
  without leadership refuses. (Caught by `TestSetupWizardFallback` — `cluster
  update` with no data must fall back to the wizard — and `TestSmoke`.)
- **`secret create` teaches the current syntax.** It told the operator to write
  `secrets(<name>)`, which no longer exists, and showed nothing about the mount. It
  now prints the `secrets: [<name>]` line, `secret_path(<name>)` as the recommended
  form, and `secret(<name>)` with the warning that the value is visible in
  `docker inspect`.
- **Real-swarm e2e fixtures migrated to `secret_path()`**, and its assertion follows
  the validator's actual wording. Two subtests had been failing for the wrong
  reason since v0.2.225: the deploy that should succeed was rejected as a malformed
  reference, and the one that should fail failed on the old spelling.

Console strings (English and Arabic) that named the removed tokens are corrected
too — the operator-facing copy said `config(name)` / `secrets(name)`.

## v0.2.226 (2026-11-02)

**The CLI stops answering cluster questions on nodes that do not have the answer.**

The leader is the only node whose view of cluster state is current, and the daemon
already worked that way: a non-leader manager runs as a standby that does not
serve. The CLI was the exception — it opened the local store on any node and
answered from it. On `nxt-sw-3-w` that produced:

```
$ pmcluster stack list
(no stacks yet — use `pmcluster deploy <manifest.yaml>`)
```

while ten stacks were running, because opening the store seeds an empty database
and an empty database answers confidently. The same node claimed there were no
secrets, and `credentials list` told the operator to run `cluster up` on a cluster
that was up.

Every top-level command now declares a scope (`internal/cli/leader_guard.go`) and
a guard runs before any command body, so a refusal happens **before** the store is
opened:

| scope | commands | where it runs |
|---|---|---|
| leader | stacks, secrets, configs, credentials, nodes, settings, telemetry, TLS, registry, webhooks, users, backups, usage, logs, services | leader only |
| node | `version`, `serve` | any node — they describe the machine |
| bootstrap | `init`, `setup`, `join`, `cluster up`, `cluster reset` | any node — they create or repair the cluster |

A refusal names the node, its role, and the leader, and offers both ways out:
run it on the leader, or use the remote CLI from your own machine. Remote mode
(`--api-url` / `PMCLUSTER_API_URL`) is unchanged and remains the only path that
makes HTTP calls — the CLI never discovers the leader and never falls back to the
API by itself. A test asserts every registered top-level command has a scope, so a
new command cannot silently inherit cluster-state answers.

Leadership detection uses `docker info` plus a node inspect by ID: `docker node
inspect self` is manager-only, so the SDK call cannot use `"self"`, and a failed
lookup fails loud rather than reporting the leader as a follower.

**Documentation.** `docs/dsl.md`, `docs/secrets-and-configs.md`,
`pmcluster/README.md`, `pmcluster/internal/ARCHITECTURE.md` and `agent.md` now
describe the four-token reference language, the platform-owned mount paths, and
the CLI node-role contract. ADR 0002 (CLI must be API-first) is deleted: its
problem statement was right, its recommended remedy — route every command through
the local daemon API — contradicted the daemon's own standby semantics, and the
behaviour it described now lives in the README and the code.

## v0.2.225 (2026-11-02)

**The legacy `secrets()` spelling is gone too.**

`v0.2.224` renamed it to `secret_path()` but kept the old plural parsing as a
compatibility alias. No manifest in the fleet used it — the platform was the only
place that still understood it — so the alias is removed and the language has
exactly four tokens:

| token | yields |
|---|---|
| `config(name)` / `config_path(name)` | content / location |
| `secret(name)` / `secret_path(name)` | value / location |

`secrets(x)` and `settings(x)` now fail validation as malformed references, with
the four surviving tokens named in the error. That is deliberate: an unrecognised
`x(y)` value would otherwise be written into the container environment verbatim —
`DB_PASSWORD: secrets(db_pass)` would reach the app as that literal string and
"work" until someone wondered why authentication failed.

Verified across every deploy manifest on the workstation (18 files) and the app
repos: no live manifest used the legacy spelling, so nothing needed migrating.

## v0.2.224 (2026-11-02)

**The reference language loses a token and gains a clearer one.**

`settings(name)` is gone. Nothing in the fleet used it, the platform resolver
already refused it (`platform manifests cannot reference cluster settings`), and a
manifest that kept it would now read as the literal string `settings(x)` in the
container environment — so it is rejected at validation as a malformed reference
instead, with the surviving tokens named in the error. The `cluster_settings`
table is untouched: it still holds `domain`, `reconcile_interval`,
`storage_nodes`, `backup_*`, `observability_*`, and feeds the platform render.

`secrets(name)` becomes `secret_path(name)`, symmetric with `config_path(name)`:

| token | yields |
|---|---|
| `config(name)` / `config_path(name)` | content / location |
| `secret(name)` / `secret_path(name)` | value / location |

The old plural spelling still parses and normalises to `secret_path`, so no
manifest breaks, but the trap is gone: singular is a value, `_path` is a location.
The secret path itself stays **computed** (`/run/secrets/<name>`) rather than read
back from Docker — it is the compose mount convention, and querying swarm for it
would add a live API call to rendering without making it truer. The guard that
matters already exists and now covers both spellings: `secret_path(x)` requires
`secrets: [x]` on the service, so a reference cannot hand an app a path to a file
nobody mounted.

## v0.2.223 (2026-11-02)

**Config files get one platform-owned location, a directory the app discovers, and
secrets resolved inside them.**

Until now a config mounted at `/etc/<name>` — a path the resolver chose and no
manifest could predict — so every stack restated it in `env`
(`PLUGINS_CONFIG_PATH: "/etc/test_lms_plugins"`). Two sources for one fact, and
the second one silently points at nothing the day the mount moves.

### The platform owns the path

Every app config now mounts under one directory, `/etc/config/<name>.yaml`
(`.yaml` only when the name carries no extension of its own). The mount location
is a platform decision — overridable per cluster with the `config_mount_root`
setting — and never appears in a manifest.

### A service is told the directory, automatically

A service that declares at least one `configs:` entry is handed
`CONFIG_PATH=/etc/config` by the translator. The app reads one variable and lists
the directory — the Kubernetes ConfigMap shape — instead of being told each
file's path. A service with no configs gets no variable, and a manifest that sets
`CONFIG_PATH` itself always wins.

`env: VAR: config_path(<name>)` is also available when a single explicit path is
needed: it resolves through the same call that produces the mount target, so the
two cannot disagree.

### Config content may reference secrets

A config file is a whole document, so `config(x)`, `secret(x)`, `secrets(x)`,
`settings(x)` and `config_path(x)` are substituted inline anywhere in it:

```yaml
db:
  user: demo
  password: secret(db_password)     # resolved at render time
```

Substitution happens when the config is materialised for a deploy — the row keeps
the reference, so no secret plaintext is ever stored in a config row next to the
ciphertext it came from. The swarm object's name is derived from the **expanded**
content, which means rotating a referenced secret changes the name, changes the
rendered compose, and the reconcile drift check redeploys on its own.

Resolution stays stack-scoped: a config belonging to stack A that references a
secret or config of stack B is refused, with the owning stack named and the value
never echoed. Config → config cycles stop at a depth limit instead of hanging the
deploy.

## v0.2.222 (2026-11-02)

**A service with no running replica now says why.**

`adam-world-dev` deployed cleanly — revision recorded, workflow green, webhook
`202` — and then every swarm task was rejected:

```
failed to resolve reference "ghcr.io/nextrum-sy/adam-world-dev:dev-v20261101-56ac0ae": not found
```

The stack page showed `0 / 1` and nothing else. The reason existed the whole time
(`GET /api/services/{stack}/{service}/tasks` returns it) but only behind the
per-service **Tasks** tab, which is not where anyone looks when the page gives
them nothing to look for.

The console now fetches task state for services that are running fewer replicas
than desired — at most five lookups per page, and none when everything is
converged — and renders the newest rejected/failed task error under the replicas
cell on the services list and the stack page, in both the table and the stacked
card. The full message is the tooltip; a running task always wins, so a service
that has recovered stops showing a stale error.

This is deliberately *not* `update_error`: that field is the swarm's reason for
pausing an update, and it is empty when the deploy itself succeeded and the
runtime then refused the task. The two were conflated, which is why a completely
broken stack looked like a quiet one.

## v0.2.221 (2026-11-01)

**A refused deploy now says why, and a new stack can get its secrets before it exists.**

### The webhook told CI the control plane was down when a secret was missing

`adam-world`'s first deploy failed with

```
secret "adam_world_session_secret" not found — create it with
`pmcluster secret create … --scope service --stack adam-world`
```

and GitHub Actions recorded, after forty seconds:

```
HTTP 502: {"error":"control plane unavailable"}
```

Three failures stacked into that one lie:

- **A deterministic error was retried.** `deploy/retry.go` retried every failure,
  including the ones no retry can change (parse, name conflict, interpolate,
  translate, record) — spending `2 × 30s` of sleeping per attempt cycle. The step
  that failed is now carried as a typed `workflow.StepError`, so the receiver can
  tell the two classes apart instead of matching on message text, and a
  deterministic failure is answered on the **first** attempt.
- **The answer waited for the whole retry window.** The phase runs on a detached
  context precisely so a CI client hanging up cannot abort a deploy — but the
  response was still held until it finished, up to `≈4 min`, against a **30s**
  ingress ceiling. The response now waits at most `webhookResponseBudget` (20s,
  deliberately under the proxy's `REQUEST_TIMEOUT`), then answers `202` with
  `pending: true` and a pointer to the delivery history, which is where the
  outcome was always recorded.
- **The proxy described a timeout as an outage.** `edgeproxy` answered every
  upstream failure with `502 {"error":"control plane unavailable"}`. A dial that
  never connects is `503`; a request the daemon accepted and did not answer inside
  `REQUEST_TIMEOUT` is `504 {"error":"control plane did not answer within 30s"}`
  with a hint naming the delivery history. An operator reading a 502 went looking
  for a dead daemon that was healthy the whole time.

Deterministic failures are now `422` with `{status, error, step, help}` and
**zero** retries; both the OpenAPI document and its drift test were updated with
them, which is how the shared `DeployResult` schema being reused for two different
bodies got caught.

### A first deploy could never create the secrets it needed

The console offered service-scope configs and secrets **only** from a stack page,
and a stack page exists only after that stack has deployed. A manifest references
`config(<name>)` / `secret(<name>)` before its first deploy — which is the deploy
that fails without them. The API and the CLI never had this problem; the UI had a
dead end with no exit.

Settings → configs/secrets now carries **“For a Stack…”**, which asks for a stack
name in a free-text field with suggestions (existing stacks, plus any stack that
already owns a service-scope item) and then opens the real service-scope form for
it. The name is allowed to be one the cluster has never seen, because that is the
case it exists for.

## v0.2.220 (2026-10-31)

**Every MCP read was broken in v0.2.219, and the tests could not see it.**

### The in-process dispatch re-entered the router under the wrong route

`v0.2.219` shipped an MCP surface whose reads all failed: `pmc_list_stacks` returned HTTP 500
(a recovered panic), `pmc_cluster_status` reported every source as `HTTP 405: ` with an empty body,
and only the tools that never touch the router (`pmc_capabilities`, and the local half of the curated
ones) worked.

Cause, in chi v5's `Mux.ServeHTTP`:

```go
rctx, _ := r.Context().Value(RouteCtxKey).(*Context)
if rctx != nil { mx.handler.ServeHTTP(w, r); return }   // no routing at all
```

A tool call executes inside the request that matched `/api/mcp`, so its context already holds chi's
route context — with `RoutePath = "/api/mcp"`. Piping that context into `base.ServeHTTP` for
`GET /api/stacks` did not route the sub-request; chi delegated to its handler with the *old* route
state, so the request was still `/api/mcp`. The MCP handler then answered its own re-entry (400/405),
and the derived-read path crashed it (500, recovered, logged without a stack).

Fixed by dispatching with `dispatchContext(ctx)`: the same context with `chi.RouteCtxKey` set to a
typed nil, which makes chi take its fresh-route path while the parent context keeps carrying
cancellation and deadlines.

### Why no test caught it

`initialize` and `tools/list` never dispatch, and the mcpserver tests call through a stub router that
contains no chi at all. Grepping `tools/call` across the repo returned nothing.

Now `internal/server/mcp_dispatch_test.go` drives a real `tools/call` through `server.New` with a real
chi router: two derived reads (settings, usage) and one curated tool whose degraded answer must name
its unavailable sources as 404 rather than the old 405. Reverting the one-line fix reproduces the
original `HTTP 405: ` exactly.

## v0.2.219 (2026-10-31)

**The MCP surface now states its own contract — and stops inventing commands.**

### A refusal told operators to run a command that does not exist

`v0.2.218` shipped refusals whose `command` was the tool name turned into words: `pmc_create_webhook`
became `pmcluster create webhook`, which no CLI implements. An agent relays a refusal verbatim, so the
failure mode was "the human runs it, it fails, and the agent that was supposed to hand the job back
handed back a dead end."

Refusals now carry:

- `command` — only for operations with an unambiguous CLI invocation (deploy, rollback, webhook add,
  user create, config set, secret show, tls site set, cluster update, service restart, backup);
- `api` — always: the exact REST call from the operation's own method and path, with body shape and
  bearer header, so the answer is correct even where no CLI mapping exists.

The mapping is validated against the real cobra tree (85 command paths parsed out of `internal/cli`),
not a bag of `Use:` tokens — a flat set would happily accept `pmcluster create webhook` because `create`
exists elsewhere. Verified by mutation.

### A tool list that advertised writes the endpoint would refuse

Every write tool now carries this endpoint's permission in its own description, computed per endpoint
from the allowlist: `WRITE: permitted`, `WRITE: NOT permitted … relay the refusal, do not retry`, or
`DANGEROUS: never served through MCP`. Read tools carry no marker. A model reading the list learns its
limits without spending a call to discover them.

### `initialize` carries instructions, and four resources carry the schemas

Connected clients now receive an `instructions` block stating the contract: reads served, the one
permitted write named from the live allowlist, the `manual_action_required` shape and `do_not_retry`,
the fact that deploys are **202-async** and must be polled, and the observability bounds — including
that `system.cpu.utilization` is the **idle** fraction, so busy = 1 − value.

Resources, so an agent reads a document instead of probing with tool calls:

| URI | carries |
|---|---|
| `pmcluster://schema/api` | the OpenAPI document the catalog derives from |
| `pmcluster://schema/logs` | the four projected log columns, filters, page caps, redaction |
| `pmcluster://schema/metrics` | every metric name per view, units, idle/used semantics |
| `pmcluster://policy` | permitted / refused / never-served writes, and the refusal shape |

They are generated, not transcribed: metric names come from the `observability` package constants, the
write lists from the live allowlist, and a test extracts the projection from
`parseable.Client.LogsSQL` and fails if the published log schema drifts from it. Instructions are
asserted on the wire through `InitializeResult`; resources are read through a connected client.

## v0.2.218 (2026-10-31)

**The API description became a checked artifact, and the daemon now serves MCP tools derived from it.**

### The spec did not describe the daemon

`pmcluster/docs/openapi.yaml` failed validation with 20 errors: 19 `$ref`s pointed at components that
were never added (`components.parameters` did not exist at all, so all eleven `/api/observability/*`
operations were unresolvable), and one response used the 3.1 `const` keyword in a document declaring
3.0.3. No generated client could consume it.

Where it disagreed with the handlers:

- `POST /api/stacks` answers **202** and its body carries `status:"accepted"` — fire-and-forget since
  v0.2.122 — while the spec promised `200`. Read literally, the document said a deploy had landed when
  the apply had not started.
- `POST /webhook/{source}` is the same 202.
- `POST /api/backups` returns `{id, status, archive_paths}`, not a `Backup`.
- `GET /api/services` emits 14 fields per service, 11 per node, 8 per stack; the schemas documented
  7, 10 and 5. Configs have three distinct shapes (list / detail / created) behind one schema.
- Four served routes were undocumented and on an allowlist the old check could not see past: the three
  public SVG badges (GET **and** HEAD, registered with `r.Method`) and the interactive `exec/ws`
  websocket.

### The check that was supposed to catch it could not

It compared path *strings* with suffix matching. It asserted nothing about methods, status codes,
response bodies or `$ref` resolution, and its route extraction only understood `Get/Post/Put/Delete/
Patch` with literal paths — so `r.Method(...)` and constant-composed routes were invisible. It passed
through every finding above.

Replaced by three checks in `internal/docscheck/openapi_test.go`:

1. every `$ref` resolves, and no 3.1 keyword appears in a 3.0 document;
2. served routes and documented operations agree **in both directions**, walking the router from
   `server.New` through `r.Route`, `Group`, every `Mount*` and `r.Method`;
3. the 2xx status codes and the JSON keys a handler can write equal what the operation documents —
   the union across every branch of the handler, following same-package response helpers, so a spec
   cannot pass by describing only the happy path or only the degraded one.

The websocket route is composed from `execWSPathPrefix`/`Suffix` read out of `execws.go`, so renaming
it in the code fails the suite instead of drifting. Verified by mutation: flipping 202→200, adding a
response key, dropping a documented field, and deleting a documented route each fail the build.

### `/api/mcp`: the Model Context Protocol surface, as a sub-route

MCP (streamable HTTP) is served by the daemon itself at `POST/GET/DELETE /api/mcp`, behind the same
`auth.Bearer` and `stackScopeGuard`. Each tool call re-enters the router **in-process** with the
caller's own `Authorization` header, so the handlers, the middleware and the token's authority are
shared — no second client, no parallel auth path, no second implementation to keep in sync.

The catalog is derived from the now-checked spec: one tool per `operationId` (65 of them), input
schemas built from path parameters, query parameters and request body with `$ref`s inlined. Four
curated tools carry hand-written schemas because the questions are not one endpoint: `pmc_cluster_status`,
`pmc_cluster_capacity`, `pmc_capabilities`, `pmc_manual_action`.

Reads are all served. Exactly **one** write is permitted by default — updating the content of a
service-scoped config — and the tool reads the config first, refusing cluster-scope targets without
issuing the write. Everything else, including every bootstrap operation that has no API route at all
(cluster up, join, reset, host hardening, TLS materialisation, secret healing), answers a structured
`manual_action_required` payload naming the command and where to run it. The dangerous set (exec,
secret reveal, deletions, TLS, backup restore) is never served, and `AllWrites()` derives its permit
list from the catalog filtered by tier, so those cannot be enabled by configuration.
`PMCLUSTER_MCP_WRITES` = unset/`service-config` (default) | `none` | `all`.

New dependency: `github.com/modelcontextprotocol/go-sdk` v1.8.0 (Apache-2.0).

## v0.2.213 (2026-10-17)

**`hack/upgrade-nodes.sh`: the sudo path was decorative under zsh, and discovery was trusted as if it were reachable.**

### The non-root install path split a variable that zsh does not split

`v0.2.212` escaped `\$SUDO` so it would expand *on the node* rather than locally — but expansion was only
half the problem. The remote login shell is the user's own, and on wafaa that is **zsh**, which does not
word-split an unquoted parameter. `SUDO="sudo -n"; $SUDO install …` therefore tried to exec a command
named literally `sudo -n` and failed with `zsh: command not found: sudo -n`, so the non-root install and
`systemctl restart` had **never** worked; wafaa upgraded only by hand. The privileged steps are now written
out once per branch (`if [ "$(id -u)" -eq 0 ]; then install …; else sudo -n install …; fi`), which cannot
depend on any shell's splitting behaviour. The same bug sat in the single-quoted `pmcluster cluster update`
one-liner at the end: identical `$SUDO` word, identical zsh failure, and because its output is piped
through `grep | tail` it exited 0 and the reconcile was silently skipped — that one is now a branch too.

### Discovery is not a reachability check

`docker node inspect … {{.Status.Addr}}` is the swarm's **internal** address. On the wafaa deployment the
nodes answer `10.7.224.x`, routable between the nodes and nowhere else, so the discovered list could only
ever time out from the machine running the script. The addresses remain correct for **role ordering**
(workers, then follower managers, leader last), so discovery stays — but it now warns, on stderr, that the
addresses are swarm-internal and tells the operator to pass `PMCLUSTER_NODES`. An explicit list wins for
*which* nodes to touch; its **order** is still derived from the swarm when a seed answers, falling back to
the stated order (never aborting) if the swarm cannot be read.

## v0.2.212 (2026-10-17)

**Three defects closed, one of them in my own previous fix, plus the e2e race root-caused.**

### The non-root install path never worked

`v0.2.210` added `sudo -n` for deployments that do not log in as root — and the `$SUDO` in the install and
restart lines sat inside a **double-quoted** ssh argument, so it expanded *locally*, where it is unset, and
`set -u` aborted with `SUDO: unbound variable`. The privilege escalation was decorative. wafaa only upgraded
because those steps were run by hand. Escaped (`\$SUDO`) in the double-quoted block; left bare in the
single-quoted reconcile command, where it *is* evaluated on the node. Both verified with `bash -n`.

### The copy no longer depends on SFTP

`scp` on OpenSSH 9.x rides the SFTP subsystem; a host with it disabled answers `scp: Connection closed`, which
reads like auth or fail2ban and is neither. The binary now goes over as `ssh … "cat > /tmp/pmcluster.new"`,
with `scp -O` as a second chance. The copy stays strictly before the install, so a copy failure leaves the node
untouched and the upgrade aborts rather than half-applying.

### `cluster update` reports config replacements

A replaced rendered config could be reported as "No rendered content changed — nothing to redeploy", because
the summary tracked stack redeploys only. `UpdateResult` gains `ConfigsChanged`, the workflow line now
distinguishes stacks-and/or-configs from configs-only from genuinely-nothing, and the CLI prints
`Configs replaced`. Previously only the OTel and Traefik configs were recorded, so a replaced
`pmcluster_observability_provision` was invisible.

### The intermittent `swarm-e2e` failure is an ordering race, fixed

`EnsureBundledNetworks` creates only the shared overlays; each stack's **private** overlay was left to
`docker stack deploy`, which attaches by name and skips creation when a network of that name is merely
*listed* — so a leftover `backup_net` still being removed asynchronously gets listed, creation is skipped, and
the name is gone by the time the service is created. That is exactly `network backup_net not found`. It is
intermittent because `TestBackupRestoreE2E` runs before `TestClusterUp` and its purge does not wait for the
network to disappear.

The up path now ensures per-stack overlays before deploy, stamped with `com.docker.stack.namespace=<stack>` so
`docker stack deploy` reuses them instead of colliding, and waits out a same-name leftover. Covered by four
tests asserting `network-create:backup_net` precedes `deploy:backup`.

## v0.2.211 (2026-10-16)

**Copy the binary over SSH, not scp — and the guard earned its keep.**

The first wafaa upgrade discovered its node, verified the release checksum, then failed at
`scp: Connection closed`. On OpenSSH 9.x `scp` uses the SFTP subsystem; a host with `Subsystem sftp` disabled
rejects it in a way that reads like an auth failure or fail2ban and is neither. The binary now goes over with
`ssh … "cat > /tmp/pmcluster.new" < "$BIN"`, which needs no subsystem.

What did *not* happen matters as much: the script noticed, reported "nothing changed on this node", and exited
non-zero. wafaa stayed at v0.2.181 with every service at 1/1. A deploy script's value is largely in what it
refuses to do.

## v0.2.210 (2026-10-16)

**The upgrade script works on a deployment that does not log in as root.**

wafaa runs as `wafaa` with passwordless sudo, and a single-node swarm — no second manager to absorb a
mistake. The script assumed `root` for three privileged steps: staging the binary (into `/root`, unwritable by
a normal user), `install` into `/usr/local/bin`, and `systemctl restart`. It now takes `PMCLUSTER_SSH_USER`,
stages in `/tmp`, and routes privileged steps through `sudo -n` **only when it is not already root**, so the
same script serves both without a second code path.

Learned the hard way in this session's wafaa attempts: the username is `wafaa` — `idlib-wafaa` is the
*hostname*, and `root` is simply not accepted, so the right key sent to the wrong user reads exactly like a
missing key. That is what the earlier "key refused" findings actually were.

## v0.2.209 (2026-10-16)

**The node-discovery path was dead on arrival — fixed, and proven.**

`upgrade-nodes.sh` aborted with `SSHOPT[@]: unbound variable` on its first real run: the discovery block I
added referenced the SSH options before they were defined, and `set -u` turned that into a fatal error. It
failed loudly rather than proceeding with a missing key, which is the behaviour the script is for — but it
means the discovery path had never worked.

Options are now defined before anything uses them. Verified two ways: `bash -n` clean, and a dry run against
an unreachable seed that reaches the discovery step and reports "discovery failed via …" instead of dying on
an unset variable.

Worth recording as a pattern rather than an incident: this script's whole purpose is to replace a manual
procedure that was easy to get subtly wrong, and its first two production runs each found something — the
seven-version worker skew, and now this ordering bug. A deploy path earns trust by being exercised, not by
being written.

## v0.2.208 (2026-10-16)

**Nothing in the platform assumes this deployment's nodes any more.**

Two static assumptions removed, both of which would have broken the first time this ran somewhere else:

- **The upgrade script takes no IP list.** It discovered nodes from the swarm — one seed host is enough — and
  derives each node's address from `Status.Addr` rather than DNS or a hand-kept list. Ordering comes from the
  cluster's own roles: workers, then follower managers, leader **last**, so a run never removes the last
  manager or restarts the leader mid-flight. `PMCLUSTER_NODES` still overrides for unusual setups.
- **The per-node dashboard filter no longer matches a naming scheme.** It keyed on `host.name LIKE 'nxt-%'` —
  one deployment's convention. It now matches a **hyphen**, which node hostnames carry and Docker's
  12-hex-character container ids do not, so it keeps excluding pre-fix container rows on any cluster.

### Dashboard variables: not supported by this Parseable

A dashboard filtered by a `service_name` variable was requested. `GET /api/v1/dashboards/{id}` returns exactly
`author, created, dashboardId, dashboardType, isFavorite, modified, tags, tenantId, tiles, title, version` —
**no `variables`, `templating` or `params` field**, and tiles carry no placeholder syntax. So Parseable v2.8.0's
dashboard API has nowhere to put a variable; a `$service` in `chartQuery` would be passed through as literal
text. Options, none of them a drop-in: check whether a newer Parseable adds dashboard variables, confirm
whether the SPA has a client-side feature the API doesn't expose, or generate one dashboard per service from
the same provisioner.

## v0.2.207 (2026-10-16)

**Placement fixed, shapes chosen per resource, and one tile removed for being unrenderable.**

The layout used w=5 at x=0/5/10 on a 12-column grid — 10+5=15, past the edge — so the third tile in every row
overflowed and left a hole. Now w=4 at x=0/4/8: exactly 12, a clean three columns. Removing the network tile
opened a hole at (0,6) — mid-grid, which is what "the placement is wrong" looks like — so the tiles re-flow to
3+3+2 with any gap trailing.

**`chartType` follows the shape.** It names the renderer family; `config.type` names the shape within it.
Pinning `chartType` to one value while varying `config.type` drew axes and legends with nothing plotted, and
pinning it to `"timeseries"` made pie/donut/table come out as "No data available" outright.

- **bar** — comparisons and magnitudes (tier split, log volume by service, memory by service)
- **table** — per-node scalars, where four exact numbers beat four bars

**pie and donut are deliberately unused**, which is a finding rather than a preference: with `chartType`
"pie"/"donut" the tiles render legends carrying the correct values (0.302/0.195 for the CPU split;
63.8/18.2/3.8/3.5 per node) but draw no marks. They need an axes configuration this provisioner does not have,
and a share-of-whole chart that draws nothing is worse than a bar that draws.

**The network tile is removed.** Every container network metric is an int64, which Parseable does not map, so
the tile was blank by construction — the same type split that makes `system.memory.usage` unchartable. It goes
back the moment a double-valued network series exists.

## v0.2.206 (2026-10-16)

**`chartType` was the bug: "timeseries" draws axes and legends but no values.**

Reported plainly — "it does not show any numbers" — and that was the accurate reading. The tiles drew axes,
legends and correct axis SCALES with nothing plotted, which I twice misread as "data is bound". Axis ranges
are a property of the returned data, not proof that anything was drawn, and I should have suspected my own
inference when the first capture of my *first* revision had shown real bars.

That first revision used `chartType: "bar"`. Introducing varied shapes changed `chartType` to `"timeseries"`
(the family the UI's own time-bucketed tile carries) while varying `config.type` — and from that point every
tile rendered frame-only. The `"timeseries"` family renders a time-bucketed x-axis; categorical x axes
(`kind`, `host.name`) need the bar family.

All nine tiles are `chartType: "bar"` + `config.type: "bar"`. Verified in the browser: every tile draws
green bars with live values.

## v0.2.205 (2026-10-16)

**Dashboard tiles all render: the shape, not the query, was wrong.**

Three tiles showed "No data available" after the metric-name fix — CPU and Memory split, Node load. Their
queries were correct: run verbatim they returned `platform=0.786 / deployed=0.183` and all four node loads.
The fault was `config.type`. Every tile declared `chartType: "timeseries"` (the one family proven to work)
with `config.type` set to `pie` / `donut` / `table` — shapes from families that combination does not provide,
so the renderer had no axes and drew nothing. Every tile whose `config.type` was `bar` worked.

All nine tiles now use `bar`. Verified in the browser, not from the API: every tile draws with populated
axes and legends. Variety is worth having, but the correct family strings for pie/donut/table have to be read
off a tile the UI creates — the same way `layout.i` and `tileType` were — and a dashboard that silently
charts nothing is worse than a uniform one.

## v0.2.204 (2026-10-16)

**Upgrading a server is now one command, and it found a seven-version skew on its first run.**

There is no `upgrade` subcommand — `cluster update` reconciles stacks and configs, not the binary on each
node. Updating was an undocumented manual dance (download, checksum, scp, install, restart, and remember to
put the leader last), which is not something to repeat on a second server.

`hack/upgrade-nodes.sh <version>` does it: download, **checksum-verified**, copy to each node, install, restart,
report the version before/after, and fail non-zero if any node doesn't take the upgrade. The previous binary
is kept as `/usr/local/bin/pmcluster.<version>.prev`. Followers are upgraded before the leader so the swarm
never loses its only manager mid-run.

Run against production it immediately exposed that **`nxt-sw-3-w` was still on v0.2.196** — seven versions
behind — because every manual rollout had only touched the three managers, while the worker also runs a
daemon (it ships its own telemetry). All four nodes are now on v0.2.203 and agree.

The README documents the procedure and the two things that make it non-obvious: `cluster update`'s summary
has printed "nothing to redeploy" while replacing configs, and repeated SSH attempts trip fail2ban, after
which the leader refuses connections while followers still answer.

## v0.2.203 (2026-10-16)

**Node tiles built on metrics that actually carry values — and the reason the first ones didn't.**

The tiles I wrote queried `system.cpu.utilization` and `system.memory.utilization`. Neither exists; this
collector emits `system.cpu.time` and `system.memory.usage`. Worse, fixing the name alone would still have
charted nothing, because of a type split found by counting populated values per metric:

| metric | rows | with a value |
|---|---|---|
| `system.cpu.time` | 16 | **16** |
| `system.cpu.load_average.1m` | 2 | **2** |
| `system.memory.usage` | 12 | **0** |
| `system.cpu.logical.count` | 2 | **0** |

Every double-valued metric lands in `data_point_value`; every **integer**-valued one arrives empty. Parseable
does not map integer datapoints, and `data_point_value_double`/`_int` do not exist — there is no column to
reach for. So no SQL could have fixed a memory tile based on `system.memory.usage`.

Rewritten against metrics that demonstrably carry values: node CPU as the non-idle/total ratio of
`system.cpu.time` (both doubles), node memory from `container.memory.percent`, node load from
`system.cpu.load_average.1m`. The memory tile filters `host.name LIKE 'nxt-%'` because rows written before the
resourcedetection fix carry a **container id** in `host.name`, and grouping without it mixes nodes and
containers into one chart.

Verified against live data before provisioning:
`nxt-sw-1-m` 75.1% · `nxt-sw-3-w` 60.5% · `nxt-sw-4-m` 44.0% · `nxt-sw-2-m` 27.9% memory,
`nxt-sw-3-w` 3.84% CPU, load 0.19.

## v0.2.202 (2026-10-16)

**Fixed the lint failure CI caught and my local linter could not.**

`v0.2.201`'s `pmcluster` job failed on `offBy1: Index() can return -1` in the new traces-pipeline assertion —
I sliced `body[strings.Index(body, "traces:"):]` without guarding, which panics if the marker is absent. CI's
golangci-lint is newer than the local one, which reported 0 issues on the same code.

Worth keeping: **a clean local lint is not evidence CI will pass.** The `-1` was real, and the fix is a guard
rather than a suppression.

## v0.2.201 (2026-10-16)

**The traces-pipeline test now asserts the invariant, not the literal list.**

Adding `filter/drop_docker_api_spans` broke `TestRenderOTelCollectorConfig_LogsPipeline`, which pinned the
traces pipeline as exactly `[memory_limiter, batch]`. The property worth protecting is that the limiter runs
**first** and `batch` **last** — a limiter placed after the exporters it exists to protect from is the actual
bug — so the test now checks the order instead of the contents, and still fails if the limiter is not first.

## v0.2.200 (2026-10-16)

**Docker API spans dropped in the collector — the missing emitter found.**

The instrumentation site I could not locate from the code is Traefik: it runs `--tracing.otlp=true` and its
**swarm provider** polls the Docker API, which is the 64,790 `GET /v1.51/tasks` spans under
`service.name=traefik-proxy`. pmcluster supplies the other half — ~89,000 inspections of a single image.
Together they are most of a ~400-span trace.

Filtered with `filter/drop_docker_api_spans` on the **traces pipeline**, matching the span name:

```yaml
- 'IsMatch(name, "^[A-Z]+ /v1\.[0-9]+/")'
```

Chosen over filtering in either producer: it is one place, it is config, it needs no rebuild, and no
operational signal is lost — a Docker API call is never what an operator is looking for, while every *caller*
span (the reconcile pass, the deploy, the HTTP request) remains.

Escaping needed care and is worth recording: YAML single quotes are literal, so the file must carry the CEL
source verbatim — **two** backslashes, which CEL evaluates to `\.`'s single `\.`, i.e. a literal dot. Four
backslashes validate cleanly and then match nothing, which is the inert-config failure this repo has already
been bitten by more than once. Verified by rendering and `otelcol validate` (exit 0); actual match behaviour
is to be confirmed against the live span counts after deployment.

## v0.2.199 (2026-10-16)

**The 400-span trace was not a span leak. It was the same image inspected ~194 times per 30 seconds.**

Reported as "each trace contains 402 spans, the control loop must not be closing them". Checked against the
traces stream instead of the code first, which reframed it:

- The spans are **Docker Engine API calls** — `GET /v1.51/tasks`, `GET /v1.51/images/<ref>/json` — emitted by
  `service.name=pmcluster` (919k spans, 144 names).
- **Nothing was leaking.** A span that never ends is never exported, so every span in that trace had been
  closed properly. Both spans in the control loop do `End()` (reconcile.go:196 and :228).
- The real defect is work, not lifecycle: `ServiceList` inspects **every service's image on every call**
  (client.go:325), and the daemon calls `ServiceList` constantly — control loop and console polling alike.
  Measured: the *same* image inspected **194 times in 30 seconds** (~6.5/s), each an API round trip and an
  exported span.

Fixed by caching the image's creation time across calls with a 10s TTL. Safe because the value is documented
as best-effort — it backs a "your local image looks old" hint — so 10s of staleness cannot change a decision.
Failures are cached too: a missing local image does not appear a moment later.

Not yet deployed; the 400-spans-per-trace figure should fall sharply once it is.

## v0.2.198 (2026-10-16)

**Every widget now splits platform from deployed, and the shapes vary.**

The split is computed in SQL from the one dimension both signals share, not assumed:

```sql
CASE WHEN `service.name` LIKE 'infra%' OR `service.name` LIKE 'observability%'
       OR `service.name` LIKE 'sso%' OR `service.name` LIKE 'backup%'
       OR `service.name` LIKE 'edge%' OR `service.name` LIKE 'pmcluster%'
     THEN 'platform' ELSE 'deployed' END
```

Four split widgets (log lines, CPU, memory, network), three per-node widgets from the new `hostmetrics`
receiver (CPU %, memory %, load), and two per-service widgets for the detail the split necessarily hides.
Shapes are chosen for the data: pie/donut for shares of a whole, bars for magnitudes, a table for per-node
scalars.

Every query was run against the live cluster before shipping, which is how two assumptions died: `stack.name`
**does not exist in the logs stream at all**, and `severity_text` does exist (3 ERROR rows). Field names now
come from `SELECT nope FROM <stream>` returning the schema.

Verified live before deployment: the split returns platform=16163 / deployed=5060 log lines, container CPU
0.489 / 0.378.

## v0.2.197 (2026-10-16)

**`host.name` was a container id, so "per node" was a lie waiting to be charted.**

`resourcedetection` was configured `[system, docker]` with `override: false` — the first detector to set a
value wins, and `system` runs first, so `host.name` became the *container's* hostname: `1af781ba5184`. Any
node CPU/memory tile built on it would have charted containers while claiming to chart nodes.

Reordered to `[docker, system]`. The docker detector reports the hostname of the daemon it talks to — the
node — and the collector already mounts `/var/run/docker.sock`, so it needs no extra access. Verified by
running the detector against the live node: `host.name: "nxt-sw-4-m"`, matching that node's own `hostname`.

Caught before shipping only because I checked what the field actually contained rather than trusting the
name.

## v0.2.196 (2026-10-16)

**The collector keeps one task per node and hands back the memory it never used.**

After the 2026-10-15/16 memory-pressure incident, the OTel collector looked like the obvious thing to
shrink: four tasks, one on each of four nodes, each carrying a 512MiB cap and a 256MiB reservation on
hosts with 1.8GB. Measured, it is not that: across all four nodes its 24-hour peak is **88–102MiB and
0.02–0.10 cores**. Cutting the count to three buys ~89MiB on the node that has the most free memory —
and costs real telemetry, which is why the count stays.

- **The shape is `global` because two of the three jobs are node-local by construction.**
  `docker_stats` reads this node's `/var/run/docker.sock`, and `filelog` tails this node's
  `/var/lib/docker/containers/*-json.log`; no other task can read either. Only the OTLP receivers are
  node-agnostic — every app on the cluster dials the service name over `monitoring-net`
  (`observability_otel-collector:4317`, verified across all eleven OTLP-configured services), so app
  traces/logs/metrics survive the loss of any single task. What does not survive is that node's
  container metrics and its **daemon's own telemetry**: `pmcluster serve` is a host process exporting
  to `http://127.0.0.1:4318` (the host-published port this service provides), so a node with no
  collector task ships nothing. Observed live during the 2026-10-16 stop-gap, when the worker ran no
  collector: `dial tcp 127.0.0.1:4318: connect: connection refused` on every export, invisible outside
  journald. The decision is recorded in `observability-stack.yml` beside the service, so the next
  reader does not have to re-derive it.

- **What actually shrank is the headroom nobody was using**: reservation `256M → 96M`, cap
  `512M → 320M`, CPU reservation `0.3 → 0.05` cores, CPU cap `0.4 → 0.25`. On a 1.8GB node that is
  160MiB of schedulable memory returned per node (640MiB cluster-wide) and a third less worst-case
  cgroup exposure, with nothing dropped from the pipeline. The reservation now matches the measured
  steady state instead of reserving 3x it; the cap sits at ~3x the measured peak.

- **The `memory_limiter` moved below the cap, where it can still work.** It was `320MiB + 80MiB
  spike` against a 512MiB cap; it is now `192MiB + 48MiB` against a 320MiB cap. A limiter that sits
  above the container limit never fires — the cgroup OOM-kill ends the backlog instead, and takes the
  collector down with it. The 270–420MiB growth this guard exists for was an unbounded exporter
  queue, which the bounded `sending_queue` and `retry_on_failure` already prevent; 192MiB is still
  ~2x anything the collector holds in steady state.

- **Parseable is left alone on purpose.** Its cap is 768MiB reservation 128MiB and its measured
  24-hour peak is 266MiB (average 197MiB) — that cap has already been reached once, when the
  collector re-read every historical container log after a restart, and it is the limit that made
  that failure local instead of fatal.

## v0.2.185 (2026-10-16)

**Observability runs on Parseable, and every platform service has a memory cap.**

Two changes that belong together, because the first caused the outage the second prevents.

- **OpenObserve is replaced by Parseable.** One self-contained binary, Parquet on mounted volumes,
  no database engine, no external dependency. The reason is not size — although Parseable idles
  around 70MB against OpenObserve's ~330MB — but credentials. OpenObserve reads `ZO_ROOT_USER_*`
  only on its first boot and seeds the root user into its metadata volume; after that the
  environment says one thing and its user store says another, with nothing to reconcile them. That
  is why its configured password could stop working with no change on either side, and why the
  recovery was always "reset the data volume". Parseable takes `P_USERNAME`/`P_PASSWORD` from the
  environment on **every** boot, so the managed credential always applies and a changed admin takes
  effect on the next restart. No volume reset exists to get out of sync.

- **The same login is reused, not reissued.** The managed `openobserve_admin` credential — and its
  Swarm secret name — are kept deliberately, so migrating rotates nothing: the OTel collector, the
  Parseable admin account and the operator all keep the credential they already had. It reaches
  Parseable through the existing `secret(...)` render-time reference language
  (`secret(observability_admin_password)`, with the old `oo_*` names still accepted so a
  half-migrated snapshot still resolves). The credential key keeps its OpenObserve-era name for the
  same reason; renaming it would have minted a new password.

- **The collector's exporters needed three separate corrections, each found live.** Parseable
  requires `X-P-Stream` and `X-P-Log-Source` on **every** signal — the value for logs is the
  reserved `otel-logs`, not an arbitrary stream name. Encoding must be `json`: Parseable answers a
  protobuf OTLP body with `Protobuf ingestion ...`. And the endpoints are
  `http://observability_parseable:8000/v1/{logs,metrics,traces}`.

- **The collector can no longer take a host down with it.** It now carries a `memory_limiter`
  processor, a bounded `sending_queue` (`queue_size: 128`) and a bounded `retry_on_failure`
  (`max_elapsed_time: 60s`). Without them, an exporter pointed at a backend that is down queues
  every batch it cannot deliver: the collector grew to 270–420MB per node, on hosts with 1.8–3.8GB
  total, until they stopped heartbeating and dropped out of the swarm one after another. The guard
  was simply never shipped in the embedded config, so every failing backend turned into a cluster
  incident.

- **Every platform service now has memory limits and reservations.** `oauth2-proxy`, all three
  backup services, the observability backend and the collector had none; `traefik` and the edge
  console had limits but no consistent policy. A runaway service is now killed inside its own
  cgroup rather than starving the host into losing its Raft leadership — which is the only way to
  make the failure mode local instead of cluster-wide.

- **The console's External link opens Parseable**, on the same `observ.<domain>` origin, through
  the renamed `observability-auto-auth` middleware which injects the admin Basic credential.
  OpenObserve's SPA session machinery is gone with it: the `auth_tokens` cookie minting and the
  `/sso-bridge` page existed only to satisfy OpenObserve's localStorage-based login, and Parseable
  has no equivalent server-side session to mint. The injected header authenticates the API and the
  OTLP ingest paths; Parseable's own UI sign-in uses the same managed credential.

- **Retention is applied by the daemon, not by env.** OpenObserve expressed retention as
  `ZO_*_RETENTION_DAYS` env vars; Parseable takes none from the environment, so the `oo_*_retention_days`
  settings only mean anything if something pushes them. v0.2.185 shipped without that and flagged it as an
  open gap; v0.2.186 closes it — see below.

## v0.2.196 (2026-10-16)

**Node-level CPU and memory, and a dashboard that is not eight bar charts.**

`docker_stats` only ever sees containers, so there was no way to answer "how loaded is this node" — the
question that decides whether a node can take another workload. Added the `hostmetrics` receiver
(`cpu`, `memory`, `load`) to the metrics pipeline; the service mounts the host's `/proc` and `/sys`
**read-only** and points gopsutil at them via `HOST_PROC`/`HOST_SYS`, because a container's own `/proc` is
not the host's. `mode: global` means every node scrapes itself and `resourcedetection` tags each series with
that node's `host.name`, so "per node" is honest rather than a sum of containers. Config validated with
`otelcol validate` before shipping.

Nine tiles, with shapes chosen to suit the data rather than for variety's sake: shares of a whole read
better as pie/donut (which node is loaded, which service dominates traffic), magnitudes compare better as
bars, and a table suits per-node scalars. `chartType` is the UI's own `timeseries` family with `config.type`
selecting the shape — the combination its SQL path produces.

## v0.2.195 (2026-10-16)

**The dashboard renders. Three details were wrong, and each was fatal on its own.**

Established by having the UI create one tile and reading it back, then replicating it — the server stores
tiles as an untyped passthrough, so no amount of API-level checking could have found these:

- **`layout.i` was missing, and it must equal `tile_id`.** It is the grid-item id react-grid-layout keys
  on; without it the dashboard throws "Something went wrong" before drawing anything. This was the primary
  crash.
- **`tileType` is `builder`, not `code`**, and `config` carries the `axes` the renderer reads
  (`x.field`/`y.field`), plus `type`/`colourScheme`/`advanced`.
- **`tile_id` must be a real ULID.** It is the one field Parseable types, so the server parses and
  re-serialises it — and a hash-derived id that merely *looks* like a ULID gets its first character
  rewritten (`2`→`A`, `3`→`B`) while `layout.i`, sitting in the untyped passthrough, keeps the original.
  The two then disagree and the grid throws again. Ids are now generated ULID-shaped, reusing the timestamp
  prefix from a tile the UI itself created, with a stable 16-char suffix from the title hash.

Queries also switched to **backtick** identifiers, as the UI writes them — no JSON escaping of double
quotes, so none of the mangling that silently stored `metric_name = \'x\'` while still answering 200.

Verified on the running instance: six tiles, `layout.i == tile_id` for all, and the dashboard draws —
four tiles with live data, "Errors by service" correctly empty, and "Log volume per minute" empty pending a
fix to its time-bucket query.

## v0.2.194 (2026-10-16)

**The provisioner stops creating dashboard tiles until the render contract is known.**

Tiles created through the API render as *"Something went wrong"* in the SPA, and the API cannot detect it.
Parseable's model is

```rust
pub struct Tile { pub tile_id: Ulid,
                  #[serde(flatten)] pub other_fields: Option<Map<String, Value>> }
```

— everything but `tile_id` is an unvalidated passthrough, so a malformed tile is accepted with **HTTP 200**
and fails only in the browser. Every "success" I reported from the API was compatible with a page that
cannot draw.

Established by bisect against the running instance: a dashboard with **no tiles renders perfectly**, so the
dashboard object is fine and the tiles are the problem. The UI's chart vocabulary is
Line/Bar/Area/Pie/Donut/Query value/Gauge/Table — `timeseries` is not in it — yet moving to `bar` with a
clean string `chartQuery` still fails, so it is not a single missing field.

The provisioner now converges the dashboard and leaves tiling to the UI, which demonstrably works. An empty
canvas is a better outcome than one that crashes.

## v0.2.193 (2026-10-16)

**The dashboard's tiles render, and its SQL is correct.**

The dashboard was being created with tiles the API accepted and the UI could not draw ("Something went
wrong"). Two independent faults, neither of which fails loudly:

- **`chartQuery` was missing.** It is what a tile actually charts. The renderer does
  `typeof chartQuery === "string" ? chartQuery : convert(chartQuery)`, so omitting it passes validation and
  then throws in the browser. A string is stored verbatim — the SQL goes in directly.
- **The SQL was being mangled.** `printf '%s' \"$query\"` word-split the query, stripping every space, and
  the host shell's `sed` doubled backslashes, so the stored SQL read
  `metric_name = \'container.memory.percent\'` — which matches nothing and renders an empty tile while the
  API returns 200. Escaping now runs in `awk` inside the container, whose BusyBox semantics are the ones
  that actually apply.

Also: the memory-bytes tile is gone. `container.memory.usage.total` has rows but a **null** `data_point_value`
on this build (1881 rows for it and for the percent metric alike; only the percent one selects a real
value), so it can never chart. A CPU tile takes its place — `container.cpu.utilization` is real.

Every tile query was run against the live streams before being committed.

## v0.2.192 (2026-10-16)

**The unattributable telemetry is gone at the source — a wipe alone would have refilled it.**

The live cluster was ingesting **4.6M INFO + 48k ERROR** log rows with `service.name` null *and* `node`,
`app` and `stack` null — traefik's own `Labels traefik.docker.* ... deprecated` chatter. Ungroupable,
unalertable, and the cause of the "null services" noise.

- The `filelog` receiver's `exclude:` entries were **inert**. They name services
  (`*infra_traefik*`), but container log paths are built from container **IDs**, so the globs matched
  nothing — which also explains why those "exclusions" never reduced the collector's memory pressure
  earlier, only `start_at: end` did. The block is removed with a note saying why, rather than left looking
  correct.
- Noise is filtered at **record level** in `filter/skip_filelog`, by `service.name` — the only handle that
  exists there: unattributable records (`service.name` null or empty) and enumerated platform chatter
  (traefik, the collector, Parseable, the provisioner, oauth2-proxy, the three backup agents). Names are
  enumerated rather than regex-matched so a typo fails loudly instead of silently filtering nothing.
- Verified with the production collector image before deploy: `otelcol validate --config=…` exits 0.

## v0.2.191 (2026-10-16)

**OpenObserve naming is gone, labels follow the OTel convention, and provisioning now includes a dashboard.**

- **Every OO-era name is renamed, with a migration.** The managed credential becomes
  `observability_admin` / kind `observability` / Swarm secret `observability_admin_password`; the settings
  become `observability_admin_email` and `observability_{logs,metrics,traces}_retention_days`; the CLI flag
  becomes `--observability-admin-email`. The gateway step in `cluster update` **carries the password over
  untouched** — the running Parseable, the collector's exporter, the injection middleware and the sign-in
  bridge all authenticate with that exact value — and it removes the three orphaned
  `zo_root_user_password*` secrets. Idempotent, so it is safe on every update.
- **Telemetry labels are dot-separated**, matching the OTel semantic convention and the resource processor
  that already used `service.name`/`stack.name`. docker_stats emitted `service_name`/`stack_name`/
  `swarm_node_id`, so a metric's attribute and its own resource disagreed on the name of the same thing.
- **The retention one-shot becomes a provisioner** (`observability_provision`, config
  `pmcluster_observability_provision`) that converges per-stream retention **and** creates the platform
  dashboard with five tiles — log volume, errors by service, memory, memory percent and network received.
  Same mechanism as retention, same reason: Parseable exposes dashboards only through its API, and the call
  belongs inside the swarm network rather than the host.
- Dashboard API contract, established against the running build: `POST /api/v1/dashboards` with just
  `{"title": …}` creates one and returns `dashboardId`; `PUT /api/v1/dashboards/{id}/add_tile` takes the
  **tile itself** as the body (not wrapped), needs `tile_id`, and is `PUT` — `POST` answers 405. Queries
  must JSON-escape their double-quoted identifiers or the API answers 400.

## v0.2.190 (2026-10-16)

**The identity headers the platform claimed to forward are actually forwarded now.**

The sign-in bridge returned `forbidden` for a real operator even though they had passed the SSO gate.
Root cause: Traefik's `sso-auth` middleware declared

```yaml
authResponseHeaders: [X-Forwarded-User, X-Forwarded-Email, X-Forwarded-Preferred-Username]
```

but `forwardAuth` copies the *response* headers of the auth endpoint, and oauth2-proxy returns
**`X-Auth-Request-*`** — and only when started with `--set-xauthrequest`. So Traefik had nothing to copy,
the downstream service saw an anonymous request, and the bridge's own gate refused it.

- `OAUTH2_PROXY_SET_XAUTHREQUEST: "true"` so oauth2-proxy emits the identity at all.
- `authResponseHeaders` renamed to the names it actually emits. Nothing else consumed these headers, so
  this has been inert since the middleware was written — the platform had never forwarded an identity
  downstream. The bridge is the first consumer.
- The bridge gate accepts `X-Auth-Request-*` (and still tolerates `X-Forwarded-*`), so a deployment with
  an older proxy is not locked out of its own console.

One auth path, for both SSO and Traefik basic-auth installs: the operator never types a credential — the
router injects it and the bridge seeds the session.

## v0.2.189 (2026-10-16)

**The sign-in bridge verifies the credential instead of assuming its shape.**

Parseable's SPA builds `Authorization: Basic <btoa(user:pass)>`, which leaves a real ambiguity
about what `localStorage["authToken"]` should hold — that whole header, or the bare base64. Guessing
wrong is a silent 401 on every request, so the page now tries the candidates against `/api/v1/about`
and stores the one the server accepts, then redirects. No assumption, nothing to debug by hand.

## v0.2.188 (2026-10-16)

**The observability login works again — restored, and ported from OpenObserve to Parseable.**

Two reported symptoms, one cause. `https://observ.<domain>/web/` showed a not-found view, and an
SSO-authenticated operator still met Parseable's sign-in form.

- **`/web/` is redirected to `/`.** Parseable answers `/web/` with HTTP 200 and then its SPA renders
  its *own* not-found page, so it looks like a 404 even though the request succeeded. OpenObserve-era
  bookmarks and links now land on the root, via a Traefik `redirectRegex` that runs before the auth gate.
- **The sign-in bridge is back architected the same way, for Parseable.** The SSO gate authenticates the
  *operator*, but the backend's web app decides whether it is logged in from state it keeps in the
  **browser** — `localStorage["authToken"]` for Parseable, exactly as OpenObserve needed
  `localStorage["userInfo"]`/`["currentuser"]`. Nothing off-origin can write it, so the console serves a
  page on that origin that seeds it and redirects.
- Routing restored: the `sso-bridge` router (`Host(observ.<domain>) && Path(/sso-bridge)`, **priority
  1000** so it beats the backend's catch-all router, `service: pmcluster`, behind sso-auth/admin-auth),
  the console's `GET /sso-bridge` handler, and the console nav link that goes through it.
- The credential is declarative, not hardcoded: `OBSERVABILITY_BASIC_AUTH` is rendered into the edge
  stack from the same `RenderInput.ObservabilityBasicAuth` the injection middleware uses, so the
  collector, the UI and the operator share **one** login. The bridge re-checks the identity headers the
  gate supplies, so it cannot hand the credential to an anonymous caller.
- A password rotation now redeploys **three** stacks (observability, infra, edge) — the credential
  reaches all three, and a stale copy in any of them means a broken login or ingest.

## v0.2.187 (2026-10-16)

**Retention is applied from inside the swarm, and Parseable publishes no port.**

A correction to v0.2.186, which reached Parseable by publishing 8000 on the routing mesh so the *daemon*
could call it.

- **The published port is gone.** It existed only because the caller was in the wrong place: the daemon is
  a systemd HOST process, not a member of a swarm network, so it cannot resolve `observability_parseable`.
  Publishing was a workaround for that, and it made the backend reachable from outside the cluster.
- **Retention is now a `run_once` service in the observability stack** — `observability_retention`, a
  curl one-shot that applies the per-stream PUTs and exits (`restart_policy: none`). Parseable is
  therefore reachable only where it is needed, from the swarm network. The API knowledge lives in the
  stack definition (config), not in Go, and the daemon keeps no backend-specific code.
- It takes the day counts from the same `oo_*_retention_days` settings, renders them into its env, and
  receives the admin credential through the same `secret(...)` reference everything else uses. It retries
  until the server answers — swarm ignores `depends_on` on this path — and is best-effort: an unreachable
  backend warns, it never fails the deploy.
- It is deliberately NOT in `WaitHealthyStacks`' service list: being `run_once`, it sits at 0/1 once it has
  completed, and the health gate would otherwise wait on it forever.

## v0.2.186 (2026-10-16)

**Retention is real, and the collector stopped re-ingesting history.**

The release that follows the Parseable swap, because the first one measured fine on an idle box and badly
under load.

- **`filelog` no longer starts at `beginning`.** With it, every collector restart re-read the ENTIRE
  historical container-log set and re-sent it — and the collector is global, so that is per node, four
  times. Measured on the live cluster it drove Parseable to **764 MB against its 768 MB cap**, OOM-killed
  (`exit 137`) four times in eight minutes at 64% CPU. Changing this one line took it to **~350 MB and
  ~0.1% CPU**. The receiver also stops scraping four platform services whose stdout has no query value:
  the two backup agents, SeaweedFS and traefik.
- **Per-stream retention is converged over Parseable's API** at `cluster update`, from the existing
  `oo_logs_retention_days` / `oo_metrics_retention_days` / `oo_traces_retention_days` settings. The
  contract was established by probing the running instance rather than from documentation, because the
  documentation does not match v2.8.0: `PUT /api/v1/logstream/{stream}/retention` takes a JSON
  **sequence** of task objects — a bare object is rejected with *"invalid type: map, expected a
  sequence"*, and an object missing `action` with *"missing field `action`"*. An unset (zero) day count
  skips that stream, leaving whatever is already configured in place rather than silently clearing it.
  Best-effort by design: an unreachable backend warns, it never blocks the update.
- **Parseable publishes 8000 on the routing mesh** so the daemon can reach it at all. The daemon is a
  systemd host process, not a swarm-network member, so it cannot resolve `observability_parseable` — the
  same reason SeaweedFS publishes 8333. Parseable enforces HTTP Basic on every path, so the port stays
  authenticated, but it is now reachable from outside the cluster and the VPS firewall is what limits
  that.

## v0.2.184 (2026-10-13)

A fix for the placement column added in v0.2.183.

- **The running node is read from the node's `Description`.** The join keyed node IDs to
  `Spec.Annotations.Name`, which is empty on every node this cluster has produced. A service's
  running node therefore resolved to nothing and the column rendered blank for every service —
  silently, because an empty value is indistinguishable from "no task running". Found by reading
  the live API after the rollout rather than the tests: the wire field was present and correctly
  named, the lookup behind it was not.

## v0.2.183 (2026-10-13)

- **The console shows where each service is running, not only where it was placed.** A service row
  carried the `io.pmcluster.node` label — the node the service was assigned — and rendered an em
  dash for every unconstrained one, which is all of them that hold no volume. The listing now also
  reports the node the running task is on, and the services, stack and platform views show it: the
  assigned node where there is one, the running node where there is not, and both when they
  disagree. A service running on a node other than the one it was assigned serves an empty data
  directory with no outward sign, so the disagreement is worth a column.

A stack's storage node is assigned once. This release makes that true in the code, not just in
intent.

- **A stack is pinned to its storage node at first placement.** The resolver's order is
  `stack pin → storage_nodes round-robin → platform_node`, and only `stack move` ever wrote a
  pin, so nothing recorded where an app had been placed. Every deploy therefore re-ran the
  round-robin against the storage-node list *as it stood that day*, and growing that list
  silently relocated every stack whose index changed — onto a node holding none of the stack's
  data, because the volume is a host path and a service that lands elsewhere comes up beside an
  empty directory. A stack's node is a property of where its data already is, so the resolved
  node is now written as a pin the first time an app holding a volume is placed. Later changes to
  `storage_nodes` affect only stacks that have never been placed, which is the intended contract:
  a stack moves when an operator moves it, or when a failover moves it.
- **`stack_pin_<stack>` is now settable through the settings path.** The key was written by the
  platform but rejected by the settings validator's fixed key list, so a pin could only be
  observed where it was read, never inspected or cleared. The prefix is accepted now, which also
  keeps a moved stack's pin honest — the failover path resolves the source node from the pin, and
  an unpinned stack's pin is only a re-computed guess.

## v0.2.182 (2026-10-12)

A backup is only a backup if its copies exist. This release makes the audit table say so.

- **A stored archive is no longer taken as proof that the offsite copy landed.** The offen agent
  writes the local copy first, then the store copy, then the offsite copy, so an archive appearing
  on disk says nothing about the configured offsite destination. Scheduled runs never pass through
  the daemon and therefore record no outcome, and discovery inferred `succeeded` from the presence
  of the local tarball alone. A deleted offsite bucket produced hours of runs reported as successful
  while every offsite upload failed — a failure visible only in the agent's own log. Discovery now
  verifies the offsite copy of each newly discovered run and records `offsite-missing` when that
  copy is provably absent, or when the bucket cannot be read at all, with a note saying which of the
  two it was. The status is new but the semantics are conservative: `offsite-missing` runs remain
  restorable, because the archive on disk is intact and restore is local-first — only the claim of a
  complete backup is withheld. With no offsite configured nothing changes.
- **`pmcluster cluster update` refuses a backup configuration whose bucket does not exist.** A 404
  `NoSuchBucket` is a configuration error, so the update fails with the bucket and endpoint named
  rather than deploying a stack that will fail its offsite copy on every scheduled run. An endpoint
  that merely cannot be reached warns and continues, so an outage cannot become an accidental
  prerequisite for bringing a cluster up. The two cases are distinguished by a typed error
  (`backups.ErrS3BucketMissing`), not by matching error text, so they cannot silently collapse.

## v0.2.181 (2026-10-12)

A correctness release for backup and restore, plus the gates that keep this repository's
documentation honest. It contains one behaviour change, and it is called out first.

- **`backup restore` REPLACES the volume instead of merging into it.** A restore extracted the archive
  over the data root and left everything else in place, so a restored volume was a union of two points
  in time rather than the state the snapshot captured. For a database that is not cosmetic: a lock file
  or an append-only log written after the snapshot describes a different moment than the data pages
  beside it, and the result can be a directory that never existed as any single state. Entries under the
  restored scope that the snapshot did not hold are now removed.
  The scope is derived from the SNAPSHOT, never from the destination: an app deployed after the backup
  was never in the archive and is untouched, and `--volume` confines both the restore and the removal to
  a single volume. The ordering is deliberate too — the snapshot is read before anything is written and
  every archive is extracted before anything is removed, so a restore that dies on a truncated archive
  leaves the volume as it was rather than half-empty. Whether a restore should merge instead, or offer
  both, remains an open decision; `internal/backups.pruneToSnapshot` is the one place implementing it.
- **A restore can no longer report success while restoring nothing.** A run recorded as succeeded with
  no archive path restored 0 files and exited 0 — `Restored 0 file(s)` — which a script, a CI job
  or an operator mid-incident reads as a restore. It now refuses, and says there is nothing to restore
  from.
- **Restore and browse find the archive the agent actually wrote.** The CLI resolved the archive
  directory from a constant while the agent's bind honoured `PMCLUSTER_BACKUP_DIR`, and an on-demand run
  records the agent's CONTAINER path (`/archive/<name>`), which nothing mapped onto the host directory.
  A local-first restore therefore declared the archive missing on the very node holding it, and a
  browsable run listed as a single "missing" entry.
- **`backup create` waits out a cold agent, and reports its own log.** The agent accepts a backup
  roughly 30-60s after its container starts; before that it exits 1 having written nothing, while the
  trigger made exactly one retry after 10s — so a backup issued straight after `cluster up` failed on a
  cluster that was seconds away from being able to take one. It now retries to a bound, and a failure
  carries the agent's own log tail instead of the old `exit status 1 ()`: `docker exec` captures only
  the streams of the command it runs, which is why the cause was always empty.
- **New real-swarm e2e for backup and restore.** It deploys a stateful stack, seeds its volume, backs
  up, overwrites the data, restores, and asserts both halves of the replacement rule — including that an
  app deployed after the backup keeps its data. It found the first three defects above.
- **New gates for the documentation.** `internal/docscheck` verifies the claims the prose makes —
  cluster setting keys, CLI commands and flags, OpenAPI paths against the registered routes, local
  links, in-repo file references and Mermaid syntax — while `scripts/check-mermaid-render.py` renders
  every diagram and measures it against GitHub's content column. Both exist because each claim they now
  enforce was false here at some point: `settings.md` documented a cluster setting the code does not
  define, and `control-loop-design.md` described an architecture with no implementation.
- **Documentation.** The multi-file RFC is replaced by a single technical whitepaper
  (`docs/whitepaper.md`); the reference docs were reconciled claim-by-claim against the code in two
  passes; and all 27 diagrams now fit GitHub's column width (three of them rendered labels at roughly
  6px).
- **CI.** Documentation-only changes no longer run the code suites — `pmcluster/**` matches markdown
  too, so a README edit was running the Go unit/smoke tier and the real-swarm e2e.

## v0.2.180 (2026-10-09)

A consolidation release with no new surface area: a repository-wide Go doc-comment conformance
pass, a docs refresh, a new engineering RFC under `docs/rfc/`, seven structural cleanups, and a
round of swarm-e2e fixes that close two real product bugs.

- **Go doc-comment conformance.** A 65-file pass aligns exported identifiers with Go convention —
  package doc comments, sentence-case summaries that begin with the identifier name, and full-stop
  termination — with no behaviour change. Two dozen exported store getters were renamed to their
  idiomatic forms (e.g. `GetSettingDefault` → `SettingDefault`, `GetCredential` → `Credential`,
  `GetConfig` → `Config`, `GetSecret` → `Secret`), with the old names kept as aliases where needed.
- **Docs refresh.** `README.md`, `agent.md`, `docs/dsl.md`, `docs/security-and-improvements.md`,
  `docs/webhook.md`, `docs/control-loop-design.md`, `docs/improvements.md`,
  `docs/network-topology.md`, `docs/storage-and-databases.md` and
  `pmcluster/docs/service-ops-design.md` were corrected and brought in line with current behaviour.
- **New RFC (`docs/rfc/`).** Eight new documents (~2,500 lines) capture the problem statement,
  architecture, decision log, bug catalogue, evolution timeline, testing methodology and open
  questions — the engineering record behind the release notes.
- **Structural cleanups.** Seven design-pattern fixes: a shared sentinel-error package
  (`internal/errs`), a `manifest.ParseBuild`/`RenderStamped` build helper, a `settings.Mask` policy
  for secret redaction, a `runtime.StackDeployer` interface extracted into a leaf package, an
  importable fake `runtime.Client` for tests (`internal/testutil/fakeclient`), the getter rename,
  and one documented behaviour change: the local CLI now prints settings secrets in cleartext, on
  the grounds that a local CLI operator is already an admin.
- **Swarm ID now persisted at `cluster up`.** `cluster up` records the live Swarm ID in the store, so
  the first `cluster update` after a fresh `up` no longer reads an empty stored ID, mistakes it for a
  wiped-and-reinitialised Swarm, and force-redeploys every platform stack. The first update after a
  fresh up is now a content-aware no-op. (Product bug.)
- **Stale platform configs are garbage-collected.** A new post-reconcile GC step in `cluster update`
  releases the previous content-addressed platform config objects (Traefik dynamic, OTel, edge) once
  the deploy has re-pointed each stack at the new object, and `EnsureConfig` now GCs on both the
  create and reuse paths — so an edited config no longer leaves orphans that accumulate forever.
  (Product bug.)
- **Swarm e2e made hermetic.** The e2e suite (`cluster_up_test.go`, `setup_test.go`,
  `standalone_test.go`, `control_loop_test.go`) was corrected for content-addressed names and made
  hermetic, and `.github/workflows/swarm-e2e.yml` now runs `go test -timeout 20m -tags=e2e ./e2e/...`
  directly instead of `make e2e` (whose hardcoded 10m timeout was too tight for the full swarm tier).

## v0.2.179 (2026-10-09)

The Poor Man's Cluster visual identity applied across the console and the README: a six-palette
brand-token system, a palette picker persisted before first paint, an inlined combination-mark logo,
and a self-contained README lockup.

- **Brand design tokens (`--brand-*`).** A new `static/brand.css` maps the brand spec into CSS custom
  properties. The palette accents (`--brand-primary`/`--brand-secondary`/`--brand-dark`/
  `--brand-surface`/`--brand-light`) switch on `[data-palette]`, with Orange as the default plus
  Purple, Blue, Red, Green and Teal; the surface/text tokens (`--brand-bg-*`, `--brand-text-*`,
  `--brand-border`) switch on `[data-theme]` (dark/light). These tokens power the logo only — the
  console's own component tokens are untouched, so the existing look is preserved.
- **Palette picker.** The console preferences now expose a six-swatch palette control that writes the
  chosen accent to `localStorage` under `pmc_palette`; a boot script in the document head applies it
  before first paint (alongside the existing theme boot), so the accent never flashes the default
  orange.
- **Combination-mark logo.** A new `frag_logo.html` partial renders an inlined isometric three-node
  mark (the mark's lines and LEDs reference `var(--brand-primary)` so they follow the active palette)
  plus a "Poor Man's Cluster" wordmark and `pmcluster` mono subtext, in the sidebar header and on the
  sign-in and setup pages. A standalone `static/logo-mark.svg` ships for reuse.
- **README lockup.** The README leads with a centered, self-contained lockup
  (`docs/assets/logo-lockup.svg`) — GitHub strips CSS variables, so its colours are hard-coded.
- **Tests.** Five new tests assert the brand stylesheet's palette/theme tokens, the static mark asset,
  the logo partial wiring across shell/login/setup, the palette boot + picker markup, and the lockup
  rendering end-to-end.

## v0.2.178 (2026-10-09)

The console SSO sign-out, Traefik pinned to manager nodes (fixing an intermittent 404 through the
ingress routing mesh), and a batch of thirteen code-hardening fixes from a source review.

- **Console SSO sign-out.** The edge now receives `EDGE_SSO_ENABLED` from the infra stack and the
  console shell exposes it as `SSOEnabled`; when `edge_login_disabled` is true but SSO is enabled the
  shell renders an `/oauth2/sign_out` link (which clears the oauth2-proxy session and bounces back to
  the gate's sign-in page) instead of the `/web/logout` form, since the console session cookie is no
  longer the authenticator under SSO.
- **Traefik pinned to managers (prod 404 fix).** A global Traefik replica landing on a worker node
  cannot use the swarm provider (it lists Swarm services through the local Docker socket, which only
  a manager can do), so that replica had no routers at all — the ingress routing mesh intermittently
  handed requests to it and `https://pmcluster.nextrum-sy.com` returned `404 page not found` on
  ~half of requests. The infra-stack Traefik is now pinned with `placement: manager`; the routing
  mesh still fronts the gateway from every node and forwards to the manager's Traefik.
- **Thirteen hardening fixes.** (1) the reconcile event loop no longer busy-spins on a closed event
  channel (it disables the case and falls back to the safety-net ticker); (2) `runPass`/`RunOnce`
  now propagate the platform-pass error instead of swallowing it; (3) `GetSettingDefault` logs a
  genuine read failure (with the key) at warn while still returning the fallback; (4) `replicas: 0`
  is rendered verbatim via a `*int` (scale to zero) instead of collapsing to the default of 1; (5)
  http healthchecks fall back to the first published port and are omitted (with a warning) rather
  than shipping a port-0 probe; (6) bcrypt passwords over 72 bytes are rejected outright instead of
  being silently truncated; (7) the session cookie is flagged `Secure` when the request arrived over
  HTTPS; (8) a CSRF Origin/Referer guard protects state-changing console routes; (9) `/sso-bridge`
  is gated against anonymous callers when SSO is enabled; (10) htmx is vendored locally instead of
  loaded from a CDN; (11) the "storage node down" warning is throttled (once per state change plus
  an hourly heartbeat); (12) the backup trigger retries once after a bounded delay to absorb a
  mid-restart offen agent; (13) `LimitBody` rejects oversized uploads at the edge with a 413.

## v0.2.177 (2026-10-09)

Platform services are now separated from customer app services across the API, the console and the
CLI. The discriminator is the `io.pmcluster.platform=true` label the compose writer already stamps on
every platform service (edge, ingress, observability, backup, SSO) — surfaced end-to-end as a
`platform` boolean rather than inferred by name or stack.

- **`platform: true` on the services API.** `GET /api/services` now returns a `platform` field for each
  service, populated from the `io.pmcluster.platform` label (pmapi, remote, services, runtime, docker
  client). Verified live: all seven sandbox platform services report `platform: true`.
- **A dedicated console Platform page at `GET /web/platform`.** Viewer-readable (like the rest of the
  read-only pages) and reachable from a new sidebar entry, it lists the platform services (with their
  stacks, replica health and node placement) that are no longer mixed in with customer apps.
- **The Services page keeps platform services in a separated "Platform services" panel.** The app
  services table now only ever shows customer apps; the platform services sit in their own panel that
  links through to `/web/platform`.
- **Stack pages are filtered to app services.** Platform-owned stacks (`infra`, `edge`,
  `observability`, `backup`) no longer appear in the stacks list, so "deploy/remove/manage" surfaces
  only real customer workloads.
- **CLI `service list` gains a `PLATFORM` column and a `--platform` flag.** `pmcluster service list
  --platform` prints only the platform services.
- **Local gate.** Unit tests for the controllers, services, remote and CLI layers were updated for the
  new field/flag, and `go test ./...` passes.

## v0.2.176 (2026-10-09)

Security, manifest and reconcile hardening (seven fixes, verified live on the sandbox).

- **Literal `$` is escaped in every rendered value (BUG-001 / CODE_REVIEW H8).** All user/DSL-derived
  values — env, command, entrypoint, labels, volumes, healthcheck paths, CORS regexes — are now
  written with `$` doubled to `$$` so `docker stack deploy`'s compose interpolation can no longer
  silently corrupt a literal `$` (a secret value `p@ss$word` was arriving in the container as `p@ss`).
  The now-redundant pre-escaping in the platform render path (`ResolveSecretValue`, `loadObjectStore`)
  was removed. Verified live: an env injected from `secret(esc_test)` stores `p@ss$$word` in the
  rendered compose while the container env is the literal `p@ss$word`.
- **The console's insecure default session secret is refused.** With login enabled the edge no longer
  silently starts on `pmcluster-ui-insecure-default-change-me`; it generates and persists a random
  32-byte secret (internal/ui/config.go, ui.go, ui/store/store.go). The `cmd/edge` "insecure default"
  startup warning is gone.
- **Multi-level deploys keep their top-level blocks.** `IR.Subset` now copies `Platform`, the app-level
  `Networks`, `Configs` and `PlainVolumes`, so a per-level subset (any `depends_on` stack) still renders
  the top-level `configs:`/`volumes:`/`networks:` declarations. Previously `config_path()` mounts,
  plain volumes and platform/app networks silently broke on multi-level stacks. Verified live: a
  two-service `depends_on` stack deployed "all levels healthy" and the `config_path()` file was present
  in the container.
- **Token lookup fast-rejects garbage.** `UserByToken` refuses tokens lacking the `pmc_` prefix or
  shorter than 40 chars before the legacy argon2id scan, so an obviously-invalid bearer can no longer
  trigger a full (expensive) user-table scan (CODE_REVIEW H2 follow-up).
- **RBAC for secret-bearing surfaces.** Migration `0025_users_role.sql` adds a `role` tier
  (admin/operator/viewer; existing users default to `admin`). Rendered configs now require the
  `operator` role (the console route moved viewer→operator), cluster settings mask secret values for
  non-admin tokens (keys containing secret/password/key), and freshly minted API keys are `operator`
  (CODE_REVIEW H5).
- **Swarm workers stand by instead of spamming (BUG-033).** A worker daemon (`LocalNodeState==active`,
  no `ControlAvailable`) now logs `this node is a swarm worker — standing by (no control loop)` once
  and stays idle, instead of re-running the doomed manager reconcile loop and spamming
  `platform pass failed: this node is a Swarm worker, not a manager` every pass.
- **Sync failures are throttled (BUG-034).** The reconcile loop logs each distinct sync error once
  (plus a 1/hour heartbeat) and records the stack status as `error` via `RecordStackError`, instead of
  re-logging on every pass.

## v0.2.175 (2026-10-08)

Storage failover & restore correctness + platform placement (pre-production blocker batch).

- **Storage failover restored another node's archive (BUG-031).** The failover picked the globally
  newest `backup-*.tar.gz` in the object store, which could be a different node's whole-disk
  archive; restoring it silently started the app with an EMPTY database. The archive is now
  selected by the SOURCE node (the stack's current pin) and the mover verifies the tarball actually
  contains `<stack>` before touching the volume, failing loudly otherwise.
- **`backup restore` was node-local (BUG-032).** It wrote into the volume root of whatever node the
  CLI ran on, so on a multi-node cluster the stack's own node stayed empty and postgres re-initialised.
  The restore now resolves the stack's owning node and, when it differs, routes the restore there
  through the same store-transit mover (rclone pulls the archive from the store via the published
  ingress and extracts it into the owning node's volume root); when it cannot route it fails loudly
  (`this stack's volume lives on <node> — run the restore there`) and the output always names the
  destination node. `--from-s3` now genuinely pulls the archive from the offsite bucket.
- **Platform placement: `platform_node` now defaults to the leader (BUG-09/BUG-03).** Platform
  services fell back to `node.role == manager`, which is a no-op on an all-manager swarm, so
  OpenObserve could land on a 1-core/1.8 GB node and OOM — load spike, SSH/Raft starvation and
  ~30 minutes with no swarm leader while the hostname-pinned backup store was down with it. The
  leader is now written to `platform_node` (update/up/persistInstallState), pinning OpenObserve,
  the edge and the control-plane backup to the leader; the store node selection avoids the platform
  node; and the edge returns a clean JSON 502 when its upstream is unavailable.
- **`join --storage-node` no longer needs joiner→manager SSH (BUG-019a).** The joiner stamps the
  swarm-visible `pmcluster.storage` label and the leader adopts labeled-but-unlisted nodes; demotion
  goes through `pmcluster node demote` (the earlier stale-label clearing is superseded).
- **`credentials rotate` is atomic (BUG-05/06).** The new versioned swarm secret is created before
  the store row is updated and the old secret is never removed; a new self-heal step rebuilds
  missing rendered secret/config mounts (the cause of silent Traefik 404s after a rotation).
- Extras: the `pmcluster webhook add` signing recipe now matches the receiver
  (HMAC-SHA256 over `<unix-seconds><body>`, `X-Pmcluster-Timestamp`), the `cluster down --purge`
  help documents the `purge-backup-<ts>.tar.gz` archive + `cluster reset --restore`, and the
  control-plane archive is uploaded offsite too.
- Verified live on a 4-manager sandbox: failover completes with the data intact (marker, amber
  badge, `stack ack`, move-back); restore lands on the owning node from both the store and
  `--from-s3`; `credentials rotate` leaves both secrets present and the dashboard serving 200/401;
  the console API through the edge returns 200.

## v0.2.174.3 (2026-10-08)

Move/failover: do not wait on the `docker service create` CLI's pipes (BUG-030 follow-up).

- Found live on the four-manager cluster: the store-transit mover task completed, but the move never
  finished because `docker service create` itself never returned (a stuck CLI process holding its
  inherited stdout/stderr pipes). The task wait therefore never started and the mover service was
  never cleaned up.
- `startMoverService` now redirects the CLI's output to a temp file (no inherited pipes), starts it
  asynchronously, and proceeds as soon as `docker service inspect` confirms the service exists
  (typically <1s); the CLI is reaped in the background and killed by a bounded context. Genuine
  create failures still surface the CLI output (e.g. `unknown flag: --host-add`).
- Tests: `TestStartMoverService_HangingCreateIsTolerated` (a create that hangs while the service
  already exists must return quickly) and `TestStartMoverService_ReportsCreateFailure` (a real
  create failure must report the captured CLI output).

## v0.2.174.2 (2026-10-08)

Storage failover: create the mover service with `--host` (BUG-030 follow-up).

- v0.2.174.1 passed `--host-add host.docker.internal:host-gateway`, but `--host-add` is a
  `docker service update` flag; `docker service create` rejected it with
  `exit status 125: unknown flag: --host-add`, so the failover aborted instead of moving the stack.
- Fix: use `--host host.docker.internal:host-gateway` when creating the mover service.
- Also removed an orphaned mover service left behind by the aborted attempt (it was stuck retrying
  the container-loopback endpoint, which is exactly what the previous release fixed).
- Test updated to assert `--host host.docker.internal:host-gateway`.

## v0.2.174.1 (2026-10-08)

Storage failover: the mover pulls the archive through the Docker host gateway (BUG-030 follow-up).

- Found while re-running the storage failover on the four-manager test cluster: the new store-transit
  path was used, but the mover task on the target node timed out.
- Root cause: the mover runs in a container, where `127.0.0.1` is the container itself. The daemon
  reads/writes the in-cluster store at `http://127.0.0.1:8333` (published on the routing mesh), but a
  container cannot reach that loopback address. Probes on the target node: host `127.0.0.1:8333/healthz`
  → 200; from a container `127.0.0.1:8333` → connection failure; `host.docker.internal:8333` → 200.
- Fix: the mover service is created with `--host-add host.docker.internal:host-gateway` and the store
  endpoint handed to rclone is rewritten from a loopback host (`127.0.0.1` / `localhost`) to
  `host.docker.internal` (non-loopback endpoints untouched). The archive still transits the object
  store, so there are still no cross-node host ports and no firewall rules.
- Also raised the mover task timeout from 120 s to 240 s: the first move on a cold node must pull the
  `rclone/rclone` image on a 1-core host.
- Tests: `TestMoverEndpoint` (loopback → host.docker.internal for `127.0.0.1`/`localhost`, with and
  without a port, other hosts unchanged) and `TestStorePullMoverScriptAndArgs` now asserts the
  `--host-add` mapping.

## v0.2.174 (2026-10-08)

Move + storage failover transit via the object store — no cross-node host ports (BUG-030).

- Found live during the four-manager campaign (TC10-B): the failover reached the transit step and the
  mover task failed (`wget: download timed out`). The old mover had the LEADER serve the archive over a
  temporary HTTP server on a random ephemeral port, and hardened hosts only open 2377/7946/4789 per
  peer — the ephemeral range was allowed only for the original two-node pair, so targets such as
  nxt-sw-4-m were silently dropped. An unattended recovery therefore depended on an undocumented
  firewall rule.
- Fix (no transit at all): the mover service on the TARGET pulls the archive object directly from the
  object store with `rclone` (an env-var remote pointed at the store's published loopback ingress
  `http://127.0.0.1:8333`) and unpacks the `<stack>` subtree into the volume root. The leader no longer
  serves anything, so no cross-node host ports and no firewall rules are involved.
- A manual `stack move` still takes a fresh backup first (it must relocate CURRENT data); the failover
  path pulls the newest uploaded archive instead (the data lives on the node that just failed).
- New `backups.ListS3Objects` (+ `S3ObjectInfo`). The mover refuses to pick a control-plane archive
  (`pmcluster-ctlplane-*`) or a non-tarball, and errors clearly when no store is configured
  ("cannot move between nodes without a shared archive …") or nothing has been uploaded yet.
- The old ephemeral-HTTP mover remains as the no-store fallback; the mover poll loop is now shared.
- Tests: `TestStorePullMoverScriptAndArgs`, `TestRcloneStoreEnv_EndpointScheme`,
  `TestNewestArchiveObject`, `TestNewestArchiveObject_Errors`.

## v0.2.173 (2026-10-08)

Backup store: sign `x-amz-date` in SigV4 (BUG-029) — automatic storage failover could not restore from the in-cluster store.

- Found live during the four-manager campaign (TC10-B): draining a storage node made the reconcile
  correctly pick a healthy alternate storage node and start the failover move, but the S3 fetch
  failed with `403 SignatureDoesNotMatch` against the in-cluster SeaweedFS store.
- Root cause: our SigV4 signer (`internal/backups/s3.go`) signed only `host;x-amz-content-sha256`
  while still sending `x-amz-date`. Strict SigV4 verifiers recompute the signature over exactly the
  headers listed in `SignedHeaders`; SeaweedFS rejects the request, whereas IONOS tolerated the
  omission — which is why the offsite leg worked and only the store path failed. Proven with a
  byte-exact reproduction on the node: the old form returned 403, the standard form
  (`host;x-amz-content-sha256;x-amz-date`) returned HTTP 200 and downloaded the archive.
- Fix: include `x-amz-date` in the canonical headers and in `SignedHeaders`. Both the object fetch
  (`fetchS3Object`) and the ListObjectsV2 path (`listS3ObjectsPage`) go through the same signer.
- This also fixes store-backed `pmcluster backup restore --from-s3`, the store discovery added in
  v0.2.170 (`pmcluster backup list` seeing the archives other nodes uploaded) and the automatic
  storage-failover restore.
- Test: `TestSignV4Headers_SignsAmzDate` — asserts the signed-header set and that the signature
  changes with the timestamp (it was stable within a day before the fix).

## v0.2.172 (2026-10-08)

Offsite backups: the S3 path-style knob offen actually supports (BUG-028).

- Found live on the four-manager test cluster: every offen agent uploaded to the
  in-cluster store (WebDAV) and kept a local copy, but the **offsite S3 leg failed**
  with `The request signature we calculated does not match the signature you
  provided` on IONOS. The rendered env set `AWS_S3_FORCE_PATH_STYLE=true`, which the
  agent's binary does not implement, so offen fell back to virtual-host style
  (`<bucket>.<endpoint>`) and the provider rejected the signature.
- The template now renders `AWS_S3_BUCKET_LOOKUP: "path"` (the knob the agent does
  support) for both offen agents, and the legacy offsite-only branch renders
  `AWS_ENDPOINT=<host>` + `AWS_ENDPOINT_PROTO=<proto>` instead of a scheme inside
  `AWS_ENDPOINT` (which offen rejects).
- Tests: `TestLoadComposeFile_BackupS3Renders`, `TestLoadComposeFile_BackupSeaweedFSDisabled`.

## v0.2.171 (2026-10-08)

Storage follows leadership — the leader is always a storage node.

- On every platform pass, `cluster update` now tracks the leader: a manager that gains
  leadership is added to `storage_nodes` (and labeled), and the **former** leader is
  removed from `storage_nodes` with its `pmcluster.storage` label cleared — so the backup
  agent (and the edge) always run on the current leader, and a failed leader returns to a
  plain node. The promoted leader is remembered in the internal `storage_leader` setting.
- `pmcluster node demote <leader>` remains refused (v0.2.170).
- Tests: `TestUpdate_DemotesFormerLeader` (with v0.2.170's `TestUpdate_AddsLeaderToStorageNodes`).

## v0.2.170 (2026-10-08)

Backups: the leader is always a storage node, the store is indexed, and no one-shot service.

- **The leader is always a storage node (BUG-024 policy).** `cluster update` re-adds the
  leader to `storage_nodes` when the setting omits it and stamps its `pmcluster.storage`
  label; `pmcluster node demote <leader>` is refused. The leader also runs the edge
  (console/API/webhooks), so the offen agent is always local and `pmcluster backup create`
  reaches it with a plain `docker exec`.
- `backup create` on a node with no offen agent now fails with an actionable message
  ("no offen backup agent on this node — it is not a storage node … run this on a storage
  node, or make this node one: pmcluster node promote <hostname>") instead of the generic
  "is the backup stack deployed?".
- **Reverted the one-shot backup service.** A manual trigger no longer creates an
  ephemeral swarm task: Docker has no remote exec, and with the leader guaranteed to be a
  storage node the local agent is always present, so no synthetic container is needed.
- `backup list`, restore and storage failover now see archives written by *other* nodes:
  the in-cluster store is indexed (S3 ListObjectsV2, prefix `backup-`, deduped, control-plane
  archives deliberately excluded) alongside the local archive directory.
- Tests: `TestUpdate_AddsLeaderToStorageNodes`, `TestRunNodeDemote_RefusesLeader`,
  `internal/backups/s3_list_test.go` (store discovery, pagination, store-only restore).

## v0.2.169.3 (2026-10-07)

The volume repair runs on standby managers too (BUG-026 follow-up 3).

- Root cause of the third miss: `pmcluster serve` on a non-leader manager **blocks in
  `waitForSwarmLeadership`** before it ever opens the store, so nothing registered after
  that point runs on a standby — the repair loop never started on nxt-sw-4-m (verified:
  no repair log line at all, `sfapp_db` still `Rejected`). The loop now starts right after
  the docker client, **before** the leadership wait, and reads an atomically published
  storage root (empty until the store is open, i.e. on a standby).
- Stack volumes no longer depend on that root: a volume mount is a local-driver bind whose
  `device` is pmcluster-generated, so the repair creates it directly (`runtime.Volume.Bind`
  marks it). Raw host binds are still restricted to the managed root.

## v0.2.169.2 (2026-10-07)

An empty `volume_root` row counts as unset (BUG-026 follow-up 2).

- `cluster up` persists `volume_root` as an **empty string** when the operator did not
  choose a directory, and `GetSettingDefault` returns the stored value — so both the
  startup storage-root creation and the volume repair compared against `""` and matched
  nothing, silently doing no work (verified live: no repair log line at all and `sfapp_db`
  still `Rejected` after the v0.2.169.1 deploy). Both now use a shared
  `localVolumeRoot(ctx, st)` helper: a blank setting falls back to
  `manifest.DefaultVolumeRoot` (`/var/stack/data`).

## v0.2.169.1 (2026-10-07)

Volume repair resolves the volume's host directory, not just raw binds (BUG-026 follow-up).

- v0.2.169's repair only looked at `bind` mounts, but a stateful stack volume is declared as
  a **named volume with a local-driver bind** (`driver_opts: {type: none, o: bind, device:
  <volume_root>/<app>/<name>}` in the rendered compose) — so the service mount is a `volume`
  mount whose source is just the volume name, and nothing got repaired (verified live:
  `sfapp_db` stayed `Rejected` on nxt-sw-4-m after the v0.2.169 deploy, while the leader had
  the directory). The repair now inspects each `volume` mount and uses the volume's `device`
  when it is under the volume root.
- `runtime.Client` gained `VolumeInspect` (name → driver, device, mountpoint).

## v0.2.169 (2026-10-07)

Storage directories are a node-local responsibility (BUG-026) — no remote mkdir, no
manual intervention.

- **The daemon creates the storage roots it owns at startup.** `pmcluster serve` now
  `mkdir -p`s the configured `volume_root` (the directory the operator selected, default
  `/var/stack/data`) plus the backup directory before it does anything else. The setting is
  read only when the store already exists, so a not-yet-initialised node still gets the
  familiar `run pmcluster init` error instead of a silently created empty database.
- **Every daemon repairs the volume directories of the stateful services the Swarm placed
  on it** (new `internal/cli/volume_repair.go`, at startup and every 30 s, leader or not).
  A pass lists the services, keeps the ones whose `io.pmcluster.node` pin is this hostname,
  and `mkdir -p`s any missing bind source under the volume root — then force-updates the
  services it repaired so the Swarm retries the tasks it had rejected. This fixes the case
  the earlier local-CLI step could not: the deploying host is not necessarily the node a
  stateful stack is pinned to, and the Swarm refuses to start a task whose bind source is
  missing (`failed to populate volume: mount /var/stack/data/sfapp/db_data: no such file or
  directory`, reported live in TC10-B). Services without a node pin (role-based or global —
  the platform agents) are left to `cluster up`/`update`; a worker (which cannot list
  services) stays silent and retries on the next tick.
- `runtime.ServiceInspectResult` now carries the task-template mounts (`runtime.Mount`) so
  the repair can see a service's bind sources.

## v0.2.168.3 (2026-10-07)

Node-loss recovery, take two: the platform services that actually got stuck now recover too
(BUG-023 follow-up).

- v0.2.168.2 changed the **default** restart policy for replicated services to `any`, but the
  four services that hung at 0/1 in the TC10-A failover campaign — `edge_pmcluster-edge`,
  `backup_volume-backup`, `backup_control-plane-backup`, `backup_seaweedfs` — set
  `restart: on-failure` **explicitly** in the platform manifests, so the explicit value kept
  winning and their tasks were still never replaced after a clean (exit 0) termination.
- Those four platform services now declare `restart: any` (they are long-running daemons:
  the edge proxy, the two offen backup agents and the in-cluster backup store), keeping their
  `restart_delay`.
- Verified live after deploy: every platform service now renders
  `{"Condition":"any","Delay":5000000000}`.

## v0.2.168.2 (2026-10-07)

Node-loss recovery: replicated services come back on their own, and the daemon
returns with Docker (BUG-021b, BUG-023 — both found live during the four-manager
failover campaign).

- **BUG-023 — a cleanly-exited replicated task was never replaced.** A
  long-running service whose container exited with code 0 (SIGTERM while its
  node's Docker daemon stopped, e.g. a Docker upgrade) is marked `Complete` by
  the Swarm, and `restart_policy: condition: on-failure` ignores a zero exit, so
  the task was never rescheduled: after the test cluster's daemons restarted,
  `edge_pmcluster-edge` and `backup_control-plane-backup` sat at 0/1 and
  `backup_volume-backup` at 1/2 until a manual `docker service update --force`.
  The writer now renders `condition: any` for the default replicated case — the
  same policy the global services already used, which is why they recovered on
  their own. An explicit `restart:` in a manifest still wins, and run-once jobs
  are unchanged (`none`, or `on-failure` with `max_attempts: 3` when the job has
  `depends_on`).
- **BUG-021b — starting Docker did not start the daemon.** `BindsTo=docker.service`
  (v0.2.167) propagates a *stop* but not a *start*, so
  `systemctl stop docker && systemctl start docker` left `pmcluster` inactive on
  the standby managers; a node with no control loop silently stops reconciling if
  it is (or becomes) the Raft leader. The generated unit now also carries
  `WantedBy=docker.service`, so starting Docker pulls the daemon back in.
  `ensureDaemonRunning` (run by `cluster up`/`update`/`reset`/`join`) rewrites and
  re-enables the unit, so `install.sh` rolls the change out.
- Tests: `TestRenderSystemdUnit` asserts `WantedBy=docker.service`; the translator
  goldens and `TestTranslate_Golden`'s expectations assert `condition: any` for
  replicated services (the run-once fixtures keep `condition: none`).

## v0.2.168.1 (2026-10-07)

Standby managers also get the current daemon + systemd unit.

- Follow-up to v0.2.168: the standby early-return in `cluster update` / `cluster reset` skipped
  `ensureDaemonRunning`, so a manager that has never been promoted kept the old unit
  (`Requires=docker.service`) until it was promoted. It now refreshes the unit and restarts the
  daemon on the standby path too, so unit changes (BUG-021's `BindsTo=docker.service`) roll out
  to every manager during an `install.sh` upgrade.

## v0.2.168 (2026-10-07)

CLI: `cluster update` / `cluster reset` no longer mistake a standby manager for a fresh box (BUG-022).

- Found in the four-manager campaign: `install.sh VERSION=…` on **nxt-sw-3-w** — a manager that
  joined the Swarm but was never promoted — failed with
  `No cluster configuration found — starting the interactive setup wizard` →
  `error: domain is required (interactive prompt or --domain)`.
- Root cause: a manager that never won a Raft election keeps an intentionally **empty local
  store** (the control plane lives in the Raft state snapshot and is restored when the node is
  promoted — 3-w had 0 `cluster_settings` rows while the promoted 2-m had 29), and both
  commands used "no stored `domain`" as the test for "no cluster".
- `classifyLocalCluster` now distinguishes three states: **provisioned** (settings present),
  **standby** (no settings, but the node is a member of an existing Swarm) and **fresh** (no
  settings, no Swarm). A standby manager gets a one-line explanation and exits 0 — nothing to
  update there — and only a genuinely fresh box drops into the wizard. The Docker probe used
  for classification is best-effort, so a host without a daemon still reaches the wizard.
- Tests: `TestClassifyLocalCluster` (fresh / standby / probe-error / nil client /
  provisioned-wins).

## v0.2.167 (2026-10-07)

Control plane: survive a leaderless swarm, and come back after a docker restart (BUG-020, BUG-021).

Found in the four-manager failover test (TC10-A). `systemctl stop docker` on the leader
elected a new leader in ~25 s, the new leader restored the whole control plane from the Raft
snapshot (configs + settings) exactly as designed, and a second consecutive leader loss was
likewise recovered — but two recovery paths were wrong:

- **BUG-020 — a daemon that starts while the swarm has no Raft leader crash-looped.**
  Docker answers every config call with `The swarm does not have a leader ...`, and the
  startup restore treated it as fatal, so systemd restarted the daemon every ~5 s (observed:
  28 restarts in ~2 min). `IsSwarmUnavailable` (exported; was `isNotSwarmManager`) now also
  recognises the no-quorum messages — `does not have a leader`, `too few managers are
  online`; snapshot and restore become a logged no-op, the startup restore falls through to
  the tarball archive (and keeps local data when there is none), and the daemon stays up and
  stands by until quorum returns.
- **BUG-021 — a docker-only restart left `pmcluster` inactive.** The generated unit used
  `Requires=docker.service`, which propagates stop but not start: stopping dockerd stopped
  the daemon and starting dockerd did not bring it back (verified live — a manager returned
  with no control loop, so a leader that is or becomes the Raft leader stops reconciling
  silently). The unit now uses `BindsTo=docker.service`.

Upgrade note: the unit is rewritten by `cluster up` / `join` / `cluster reset` (not by
`cluster update`), so re-run one of those — or `install.sh` — to pick up the new unit.

Tests: leaderless-swarm fixtures in `internal/controlplane` + `internal/cli` (verbatim
Docker no-leader errors), a six-case `IsSwarmUnavailable` table, and the updated unit
expectation. Falsified: disabling both no-quorum patterns makes the cli test fail with the
exact crash-loop error.

## v0.2.166.1 (2026-10-07)

CI: golangci-lint is green again — explicit returns after `t.Fatal` nil-checks.

- The main gate had been red for six consecutive commits: under **Go 1.25** staticcheck
  reported 30 `SA5011 possible nil pointer dereference` findings in test files (in that
  analysis state it does not treat `t.Fatal` as terminating; the same code is clean under
  Go 1.26, which is why it never reproduced locally).
- Fixed by adding an explicit `return` to each nil-checked branch — 15 sites across 10
  test files. No assertion was changed, no linter rule disabled, no production code touched.
- Test-only change: the v0.2.166.1 binaries are identical to v0.2.166.

## v0.2.166 (2026-10-07)

Secrets: `pmcluster secret heal` — find and repair rows the current key cannot open.

- New command `pmcluster secret heal [--dry-run]`: walks every stored secret and
  1. decrypts the row with the current `~/.pmcluster/.encryption_key`,
  2. repairs a **stale hash** (translation derives the Docker swarm secret name from the
     stored hash, so a mismatch referenced a swarm object that never existed),
  3. creates the content-addressed swarm secret when it is missing.
- Rows that do **not** decrypt were sealed by an older key (rotated or restored at some
  point). AES-GCM cannot be reversed without the key that sealed them, so the pass reports
  them as `unrecoverable` and prints the recovery recipe: read the plaintext from a
  container that still mounts it (`docker exec <c> cat /run/secrets/<name>`) and re-supply
  it with `printf '%s' '<value>' | pmcluster secret edit <name>`. A pass never changes a
  secret's value; `--dry-run` reports without writing anything.
- Exits non-zero when something still needs attention (unrecoverable rows, or a swarm
  mirror that could not be created because no daemon was reachable).
- Driven by a real incident: on nextrum 8 of 9 rows had been sealed under an older key, so
  `cluster update` warned `cannot rebuild swarm object (ciphertext not recoverable)` and a
  from-scratch rebuild would have failed. Those rows were healed by hand; this command
  makes it a one-liner.
- Core logic lives in `internal/secrets` (`Local.Heal` + `HealReport`, with the swarm
  mirror injected) so it is testable without Docker; `internal/cli` wires the mirror and
  prints the table. `mirrorSwarmSecret` now shares `swarmSecretMirrored` with the heal pass.
- Managed credentials (`traefik_dashboard`, `openobserve_admin`, …) are still healed by
  `pmcluster cluster update`; this command covers the user secret table.
- Tests: `internal/secrets/heal_test.go` (healthy row ok, mirror created, stale hash
  repaired, unrecoverable reported without a mirror attempt, dry-run changes nothing,
  mirror failure degraded, empty store) and `internal/cli/secret_heal_test.go` (command
  registered, run path creates the content-addressed swarm secret).

## v0.2.165 (2026-10-07)

Reconcile: a failed app-stack deploy no longer suppresses retries (BUG-018).

- Found live during the v0.2.164 rollout: `demoenv` and `uitest` failed to deploy
  (`service web: secret not found: demoenv_db_pass_3bd8cb98`) yet a revision carrying the
  **new** rendered hash was recorded anyway, so every later `Sync` compared the stored
  hash with the fresh render, saw them equal, and logged `no drift` forever — the stack
  never retried and stayed on a five-day-old task. (The affected swarm secrets were
  rebuilt by hand and both stacks re-applied.)
- Root cause: the app lane recorded the revision (with its rendered hash) **before**
  applying to the swarm. That is the BUG-009 class, fixed for platform stacks in v0.2.148
  (`SetRendered` only after a successful deploy) but never for apps.
- Fix: `Deploy`, `Sync` and `Rollback` now record the revision with an **empty**
  `rendered_hash` and stamp it — via the new `store.SetRevisionRenderedHash` — only after
  the swarm apply succeeds, on the synchronous path, the async/background (webhook) path
  and rollback. A failed apply leaves the hash empty, so the next `Sync` sees a mismatch
  and retries. The revision number, source YAML and audit trail are unchanged.
- Safety net in `stackdrift.InSync`: when the fresh render carries a label but **no** live
  service does, that is drift (`the labelled render was not applied`), which also heals a
  state already poisoned by the old ordering. A stack where none carry the label used to
  be treated as in-sync.
- Tests: `TestDeploy_FailedApplyLeavesHashEmptyAndNextSyncRetries` (fails without the
  fix), a `store` test for the new setter, a `stackdrift` case for the new rule; platform
  test fixtures now stamp the real rendered-hash label — no production rule was weakened.

## v0.2.164 (2026-10-07)

Reconcile: every deployed service carries its content hash — app stacks too (k8s-style).

- The v0.2.163 drift detection covered **platform** stacks only. App/customer stacks
  still decided "no drift" purely on the stored rendered hash (DB vs fresh render), so a
  customer stack that lost a service — or got a half-applied deploy — was reported
  in-sync forever. The same bug class, in the other lane.
- Every service of an app stack is now stamped with `io.pmcluster.rendered_hash` (at the
  `Deploy`, `Sync`, `Rollback` and per-level `depends_on` writer sites), and `Sync`
  compares the **live swarm** against the fresh render before declaring no-drift:
  a missing service, an absent stack, a stale label or a partial label set re-applies the
  stack (recording a new revision). A transient docker error is treated as in-sync so it
  can never cause a redeploy loop; `Docker == nil` (CLI deploys) is unchanged.
- The `depends_on` per-level path computes the hash **once** from the full-stack
  label-free render and reuses a single writer for every subset, so each level carries
  the identical hash. A per-subset hash would never match the full render and would
  redeploy on every pass (infinite loop) — pinned by
  `TestDeploy_OrderedLevelsStampSameHashOnEveryLevel`.
- The detector moved to a shared `internal/stackdrift` package (`InSync` +
  `ContentHash`) used by both lanes (`cluster update` and the app `Sync`).
- **Upgrade note:** the app render changes, so the first `cluster update` after upgrading
  redeploys every app/customer stack once (rolling, `start-first`) — that is what stamps
  the labels. Platform stacks behaved the same way in v0.2.163.
- Tests: `internal/stackdrift/check_test.go` (every rule: nil client, absent stack,
  missing service, drifted label, partial labels, all-unlabeled in-sync, extra live
  service ignored) and `internal/stacks/drift_test.go` (label on every service,
  identical hash across `depends_on` levels, `Sync` redeploys on a missing service /
  stale label, transient docker error → in-sync).

## v0.2.163.2 (2026-10-07)

Reconcile: a partially applied platform deploy counts as drift (BUG-017 follow-up).

- The drift check treated a service with **no** `io.pmcluster.rendered_hash` label as
  "in sync" (so the first update after upgrading would not storm through every stack).
  That was too lenient: when only **some** services in a stack carry the label the stack
  was left partially applied — e.g. a `docker stack deploy` cut off mid-flight because
  the daemon restarted during an install. Hit live on wafaa: `backup_volume-backup` and
  `backup_control-plane-backup` were left unstamped while `backup_seaweedfs` was stamped,
  and the drift went unnoticed. A mixed label state is now reported as
  **"some services are missing the rendered-hash label (partial deploy)"** and re-applied.
  Stacks where **no** service carries the label (pre-upgrade) are still left alone.
- New regression test `TestUpdate_RedeploysWhenServiceLabelMissing` (fails on the
  previous logic, passes now).

## v0.2.163.1 (2026-10-07)

Reconcile: name the reason a platform stack is redeployed.

- `cluster update` now prints **why** each stack is redeployed —
  `backup drifted from the rendered compose → re-deploying`,
  `backup a required service is missing → re-deploying`,
  `backup absent from the swarm → re-deploying`, `backup content changed → …`,
  `backup forced → …`. Previously every redeploy printed the generic
  "content changed", which was actively misleading for the drift case BUG-017
  exists to catch. `platformStackInSync` returns the reason alongside the
  verdict. No behaviour change.

## v0.2.163 (2026-10-07)

Reconcile: `cluster update` detects Swarm drift instead of trusting the stored hash (BUG-017).

- **BUG-017 fixed — updates no longer report "nothing to redeploy" while the Swarm
  has drifted.** The redeploy decision compared the STORED rendered hash against a
  fresh render, never against the LIVE Swarm: the store only records what pmcluster
  last *intended* to deploy, so a service that missed an update stayed stale forever.
  This is how a production cluster's control-plane backup kept running with **no
  remote destination at all** (local-only, on the same node as the DB) while every
  `cluster update` reported success.
- **Fix**: every platform service is now stamped with
  `io.pmcluster.rendered_hash` (the hash of the render that produced it), and
  `cluster update` compares those live labels against the fresh render — alongside
  the existing stored-hash comparison. A stack is redeployed when the hash changed,
  when the stack is absent, when a service the render expects is **missing**, or when
  a live service carries the label of a **different render**.
- Deliberately conservative: an extra live service the render no longer produces is
  ignored (`docker stack deploy` cannot remove it, so flagging it would redeploy
  forever), and a service with no label (predating this release) is treated as in
  sync — the render change that introduced the label already forces one redeploy.
  The check detects swarm-vs-render drift, not a hand-edited env on a live service.
- **Upgrade note**: the rendered compose changes, so the first `cluster update` after
  upgrading redeploys every platform stack once (that is what stamps the labels).
- Tests: `TestUpdate_RedeploysWhenLiveServiceLabelDrifted` (fails against the old
  DB-only comparison), plus the swarm fixtures now model the full rendered service
  set (the control-plane + store services).

## v0.2.162.1 (2026-10-07)

Backup: store-only clusters bootstrap their bucket; setup asks about S3 and the store node.

- **BUG-016 fixed — the store bucket is bootstrapped for store-only clusters.** The
  in-cluster store leg now ALWAYS uses the WebDAV backend (whenever the store is
  enabled): the SeaweedFS S3 API does NOT auto-create the bucket, so a store-only
  cluster's first backup failed with `NoSuchBucket`; the WebDAV gateway creates the
  `/buckets/<bucket>` path on the first PUT. Offsite S3 (`AWS_*`) is now rendered
  only when `backup_s3_*` is configured, so a store-only cluster has WEBDAV only.
- **setup asks about offsite backups**: "Upload backups offsite (S3/R2)?" — if yes it
  collects endpoint / bucket / region / access key / secret key (secrets entered
  masked, no echo); if no the cluster stays store-only.
- **setup asks where the store runs**: "Run the in-cluster backup store on 'leader'
  or 'a worker'?" → new `backup_store_on` setting (`leader` | `worker`; default
  worker). `pickStoreNode` pins SeaweedFS to the leader (preferring the elected
  leader, else any manager) or to a non-storage worker. Also `--backup-store-on`
  for non-interactive setup.

## v0.2.162 (2026-10-07)

Backup store: offen double-writes each archive — the rclone replicator is gone.

- **offen double-write**: when the in-cluster SeaweedFS store AND the offsite
  `backup_s3_*` target are both configured, the backup agents (volume +
  control-plane) upload every archive to BOTH — offsite via S3 (`AWS_*`, scheme
  stripped into `AWS_ENDPOINT_PROTO=https`) and the in-cluster store via a new
  WebDAV backend (`WEBDAV_URL` / `WEBDAV_PATH`). offen's native retention
  pruning keeps both copies in lockstep.
- **rclone replicator removed**: the `backup-replicate` sidecar (the `rclone
  sync` loop) is deleted — offen writes the two copies directly, nothing mirrors
  the bucket anymore.
- **WebDAV gateway, in-process** (`weed server … -webdav -webdav.port=7333`): the
  gateway runs inside the SAME SeaweedFS container (no separate service), so it
  reaches the filer on loopback and shares the container's network namespace. It
  serves the SAME filer namespace as the S3 gateway, so a WebDAV write at
  `/buckets/pmcluster-backups/<file>` IS the S3 object `<file>` (verified live on
  the 2-node cluster, both directions). Cluster-internal: no published port,
  never on the ingress network, no auth.
- SeaweedFS keeps advertising loopback (`-ip=127.0.0.1`, bind `0.0.0.0`): the
  published 8333 attaches the ingress network, whose auto-detected task IP is not
  self-reachable and so must not be advertised. Because WebDAV is in-process,
  there is no cross-container loopback problem.
- The daemon read path is unchanged: failover/restore still reads the store via
  S3 at `127.0.0.1:8333` (bucket `pmcluster-backups`).
- Tests: `TestLoadComposeFile_BackupSeaweedFS` (in-process webdav + offen
  double-write + no rclone), `TestLoadComposeFile_BackupSeaweedFSDisabled`, and
  a new `TestLoadComposeFile_BackupSeaweedFSStoreOnly`.

## v0.2.161 (2026-10-05)

Cluster lifecycle, self-healing reconcile, and swarm-wipe recovery.

- **`cluster update` is now existence-aware**: on a matching rendered hash it
  still verifies each platform stack is present in the swarm (via the stack
  namespace label) and redeploys it when absent, so a wiped/partial swarm no
  longer stays empty.
- **Self-healing update**: every `cluster update` now re-ensures the external
  overlay networks (traefik-net, monitoring-net), re-materializes ALL managed
  credentials' swarm secrets (incl. the non-rotatable `edge_api_token`) and
  rebuilds user secrets/configs from the DB index BEFORE deploying stacks, and
  detects a changed swarm ID (wipe) and forces a redeploy.
- **`cluster reset [--restore <archive>]`**: rebuilds the cluster from the DB —
  optionally restoring the store from a purge backup, then a forced,
  existence-aware reconcile. Idempotent; the recovery verb for a wrecked swarm.
- **`cluster down --purge` now purges everything**: it stops the daemon, writes
  a restorable store backup (`purge-backup-<ts>.tar.gz` = data.db +
  .encryption_key + config.yaml) to the data dir, then removes the platform
  resources AND the local store (the backup archive is kept). Without `--purge`,
  behavior is unchanged (DB preserved). `cluster down` on a fresh box with no
  store is a graceful no-op.
- Tests: existence-aware redeploy, network re-ensure, managed-credential
  re-materialization (plain + htpasswd), swarm-ID change, `--force`, purge
  backup+delete, backup round-trip + traversal guard, `reset` registration.

## v0.2.160.3 (2026-10-05)

SeaweedFS store fixes found deploying to the 2-node test cluster.

- **Swarm advertise IP**: a published port attaches the ingress network, so
  SeaweedFS auto-detected a task IP it could not self-reach and object uploads
  timed out (`dial tcp …:8080: i/o timeout`). It now advertises `127.0.0.1` and
  binds all interfaces (`-ip=127.0.0.1 -ip.bind=0.0.0.0 -s3.ip.bind=0.0.0.0`).
- **offen endpoint**: `AWS_ENDPOINT` must be host:port with the protocol in
  `AWS_ENDPOINT_PROTO`; a scheme (`http://`) is rejected ("conflicts with the
  secure option"). The in-cluster endpoint is now `backup_seaweedfs:8333` with
  `AWS_ENDPOINT_PROTO=http` (rclone keeps its own `http://` prefix).

## v0.2.160.1 (2026-10-05)

In-cluster backup store moved from MinIO to SeaweedFS (supersedes v0.2.160).

- **Why**: MinIO withdrew its public Docker Hub images (`minio/minio`,
  `minio/mc` → "repository does not exist"; `quay.io/minio/minio` → 401), so the
  v0.2.160 store could not pull and backups broke wherever it auto-enabled. Only
  the TC7 test cluster had it; production was never on v0.2.160.
- **Engine: SeaweedFS** (`chrislusf/seaweedfs:4.48`) — fully declarative, no
  admin API and no bootstrap: `server -s3 -s3.port=8333` with the S3 identity
  supplied via env (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`); buckets are
  created on first upload. Headless; pinned to a NON-storage node via an explicit
  placement (bypasses the volumed-service → storage-node auto-pin; first
  non-storage worker, else first non-storage node, else platform/manager).
- **Offsite replication**: an `rclone/rclone` sidecar `rclone sync` loop mirrors
  the bucket to the `backup_s3_*` target (deletions propagate, so retention
  pruning also cleans the offsite copy).
- **offen uploads to SeaweedFS** (`AWS_ENDPOINT=http://backup_seaweedfs:8333`,
  bucket `pmcluster-backups`); the `seaweedfs` service publishes 8333 on the
  routing mesh so the leader daemon reads backups at `127.0.0.1:8333` during a
  failover restore. Failover/restore code path unchanged (the store *is* the S3
  endpoint).
- **`seaweedfs_admin` managed credential** (access key `pmclusterbackup`, secret
  `seaweedfs_credentials`); `cluster update` self-heals it on existing clusters.
- Tests: `TestLoadComposeFile_BackupSeaweedFS` (image + command + plain volume +
  explicit non-storage placement + storage-pin bypass + offen→store + rclone
  mirror) and `TestLoadComposeFile_BackupSeaweedFSDisabled`.

## v0.2.160 (2026-10-05)

**Superseded by v0.2.160.1** — MinIO images are no longer pullable; see above.

In-cluster MinIO backup store (one combined backup stack).

- **MinIO + backup agents in one stack**: the backup stack now runs a headless
  MinIO (`MINIO_BROWSER=off`, no console) alongside the offen agents, so there
  is nothing to click — configure the offsite target once and forget it.
- **Durable copy in a different failure domain**: MinIO is pinned to a
  NON-storage node via an **explicit placement**, which bypasses the
  volumed-service → storage-node auto-pin (that pin only fires when placement is
  empty). Chosen at render time as the first non-storage worker, then the first
  non-storage node, falling back to the platform node / manager only when every
  node is a storage node.
- **offen uploads to MinIO** instead of straight to the offsite endpoint
  (`AWS_ENDPOINT=http://backup_minio:9000`, bucket `pmcluster-backups`).
- **Offsite replication**: a `backup-replicate` sidecar (`minio/mc`) continuously
  mirrors the MinIO bucket to the existing `backup_s3_*` target
  (`mc mirror --overwrite --remove --watch`), so retention pruning also cleans
  the offsite copy.
- **Failover/restore unchanged**: MinIO *is* the S3 endpoint now — the leader
  daemon's `fetchNewestArchiveFromS3` and `backup restore --from-s3` read from
  MinIO (`127.0.0.1:9000` via the routing mesh); the code path is identical.
- **`minio_admin` managed credential**: root user/password auto-minted at
  `cluster up` (username `pmcluster`, secret `minio_root_password`); `cluster
  update` self-heals it on existing clusters so MinIO enables on the next update.
- Tests: `TestLoadComposeFile_BackupMinIO` (headless MinIO, explicit non-storage
  placement, storage-pin bypass asserted by constraint count, offen→MinIO,
  replicator) + `TestLoadComposeFile_BackupMinIODisabled` (legacy offsite path).

## v0.2.159 (2026-10-05)

Storage failover safety, lifecycle & visibility (Test Case 8 follow-up).

- **`storage_failover` setting (opt-in)**: automatic failover is now gated on
  `storage_failover=true`. When a storage node goes down and the setting is off,
  the control loop leaves the stack paused and emits an alert signal instead of
  moving it. New clusters are prompted at setup when offsite S3 is configured
  ("Enable automatic storage failover?"); without S3 it stays off.
- **S3-only failover**: the automatic path always restores the newest offsite
  archive (`MoveOptions{FromS3:true}`); a down node can never be a source, so
  storage HA depends on S3. The HTTP mover remains for manual moves with both
  nodes up.
- **Failover marker + lifecycle**: a successful failover persists a
  `stack_failover` row (`from`, `to`, `at`, `acked`). Until acknowledged or moved
  back, the stack shows a `failover` status.
- **Failover badge (GitHub-shared)**: `/api/public/badge/{stack}` returns an
  amber `failover` status while an unacknowledged marker exists — the shared
  signal a human must act on.
- **Console + CLI**: stack detail shows a warning banner with [Move back] and
  [Acknowledge & mark healthy]; `pmcluster stack show` surfaces the marker and
  `pmcluster stack ack <stack>` acknowledges it. A move (including move-back)
  clears the marker.
- **OTel failover metrics**: `pmcluster.storage.failover.total`,
  `pmcluster.storage.node.down`, `pmcluster.storage.failover.disabled` — the
  operator builds alerts on top in OpenObserve.
- **Hourly backups**: new `backup_cron` setting (default `0 * * * *`) renders
  into the volume-backup agent's `BACKUP_CRON_EXPRESSION`, so the offsite archive
  is never more than an hour old (the failover restore source). Set e.g.
  `0 3 * * *` for the old daily cadence. Offen already prunes remote S3 objects
  via `BACKUP_PRUNING_PREFIX`/`BACKUP_RETENTION_DAYS`.

## v0.2.158.2 (2026-10-05)

Storage move transit fixes (round two) — found live in Test Case 8 (two-node storage HA).

- **Mover candidate ordering**: the mover now tries local interface IPs (RFC1918
  private addresses first) before the swarm advertise address. The advertise is
  often a public IP that hardened nodes silently drop on ephemeral ports, and
  busybox `wget -T` cannot reliably bound the connect on a dropped SYN — so a
  public-first order hung the mover until the task deadline even though a
  working private address existed. `moverCandidateURLs` now sorts private
  (10/8, 172.16/12, 192.168/16) local addresses first and keeps the advertise
  address as the final fallback.
- **Stale target cleanup**: the mover script now runs `rm -rf /data/<stack>`
  before `mv` so a leftover subtree from a previous failed move attempt (which
  made `mv` error with "Is a directory") no longer blocks a retry.
- Tests updated/added for the new candidate ordering (advertise last, dedup,
  no loopback leak, bare-IP advertise handled) + full suite green.

## v0.2.158.1 (2026-10-05)

Storage move transit fix — found live in Test Case 8 (two-node storage HA).

- **BUG-015:** the cross-node mover built its archive URL only from the swarm
  leader advertise address (public IP). On hardened clusters the public
  interface is not routed between nodes, so the mover task hung and failed
  (`non-zero exit (1)`) while the stack stayed safely on the source node.
  Fix: the mover now tries the advertise address first, then every non-loopback
  local interface IP (private-network reachability), deduped — the ephemeral
  archive server binds 0.0.0.0 so any of them serves it. Mover script loops the
  candidate URLs until one `wget` succeeds.
- Tests: `moverCandidateURLs` unit tests (advertise-first ordering, port
  stripping via SplitHostPort, bare-address + empty-address handling, no
  loopback leakage, dedup).

## v0.2.158 (2026-10-05)

Storage-node lifecycle, move + automatic storage failover — Test Case 8 (two-node storage HA).

- **`node promote <hostname>` / `node demote <hostname>`** — make any swarm node a storage node (updates the `storage_nodes` setting and stamps/clears the `pmcluster.storage` node label). Workers qualify, not just the leader.
- **Console storage promotion** — the Overview nodes table shows a storage pill per node and a promote/demote button (shield icon), backed by new daemon endpoints `POST /api/nodes/{hostname}/storage` + `DELETE /api/nodes/{hostname}/storage` (the `/api/nodes` list now also reports each node's `storage` flag).
- **`stack move` via console** — new `POST /api/stacks/{name}/move` daemon endpoint + a Move form on the stack detail page (target node input), matching the existing `pmcluster stack move <name> --to <node>` CLI.
- **Move with S3 restore** — `stacks.MoveWithOptions` with `MoveOptions{FromS3: true}` fetches the newest succeeded backup for the stack from the offsite store (`backups.FetchS3Object` is now exported) instead of triggering a local backup — for when the source storage node is down and its local archive is unreachable.
- **Automatic storage failover** — when a storage node goes down, the reconcile loop now picks a healthy alternate storage node (from `storage_nodes` ∩ healthy nodes) and moves each pinned stateful stack there with an S3 restore, instead of pausing indefinitely. Moves run async with a 5-minute per-stack cooldown (`FailoverMove` seam is injectable for tests).
- **Tests:** CLI promote/demote (6), API storage handler (6: promote/demote/idempotent/404/500/storage-flag), reconcile failover (move-to-healthy + cooldown + no-alternate), UI controller move (happy/empty-target), UI node promote/demote.

## v0.2.157 (2026-10-05)

Traefik runs on **every** node — found during Test Case 7 (1 manager + 1 worker bring-up).

- **Root cause:** `infra-stack.yml` rendered traefik as `mode: global` but with a placement constraint `node.hostname == <platform_node>` (or `node.role == manager` fallback). On a swarm with a single manager, that pinned the ingress gateway to the leader only — workers never got a traefik replica, so the requirement "traefik on all nodes" was not met.
- **Fix:** traefik is now placement-free (global, unconstrained) — one replica on every node, managers and workers alike, so any node can serve ingress traffic. The other platform stacks (edge, openobserve, backup, sso) keep their platform-node/manager pinning.
- **Tests:** `TestTraefikRunsOnAllNodes` (asserts global + no `node.role`/`node.hostname`/`constraints` with and without PlatformNode); `TestPlatformNodePinsAllStacks` and `TestPlatformNodeEmptyKeepsManagerRole` narrowed to the non-ingress stacks.

## v0.2.156 (2026-10-05)

Fix `secret(<name>)` env refs for **user/app manifests** — found during the post-v0.2.155 test-node purge + re-test.

- **Root cause:** `secret()` worked only on the platform path (`renderRefResolver`); the app-path resolver (`StoreConfigResolver`) did not implement `manifest.SecretValueResolver`, so a manifest like `env: APP_PASS: secret(t2_pass)` failed with `secret(t2_pass) requires secret-value resolution (not available)`.
- **Fix:** `StoreConfigResolver` gained a `Cipher *credentials.Cipher` field + `ResolveSecretValue(ctx, stack, name)` — fetches the DB secret row, refuses cross-stack rows (`belongs to stack %q`), decrypts the ciphertext with the cluster encryption key (Docker secrets are write-only, so the value can only come from the DB). Nil store / nil cipher / missing secret all fail loud with operator hints.
- **Wiring:** daemon (`serve.go`) passes the already-opened cipher; CLI local deploys (`openDeploySvc`) open the cipher from `cfg.EncryptionKeyPath()`.
- **Test:** `TestStoreConfigResolver_ResolveSecretValue` (5 subtests: own-stack decrypt, cross-stack refusal, missing-secret hint, nil cipher, nil store).
*current* state of the project; this file is the only place that tracks what changed
and when.

## v0.2.155.3 (2026-10-04)

- **fix(cluster): edge-stack TLS certresolver labels are ACME-conditional (prod still down on non-ACME clusters — 404/000 persisted after v0.2.155.2).** The edge-stack DSL conversion hard-coded `traefik.http.routers.{pmcluster-web,pmcluster-api,sso-bridge}.tls.certresolver=letsencrypt` unconditionally. On ACME clusters (test node) the resolver exists so the labels are harmless; on BYO-cert clusters (prod: `tls_mode` empty, ACMEEmail unset) the letsencrypt resolver is never declared (infra-stack only declares it under `[[ if .ACMEEmail ]]`), so Traefik logged `Router uses a nonexistent certificate resolver letsencrypt [pmcluster-api@swarm, sso-bridge@swarm, pmcluster-web@swarm]` and dropped every edge router — customer apps kept returning 404 even after the v0.2.155.2 mount fix. Fix: the three certresolver labels are now wrapped in `[[- if .ACMEEmail ]]`. Regression test: `TestLoadComposeFile_EdgeBYOModeNoLetsEncrypt` (BYO render has no letsencrypt references; ACME render still pins the resolver).

## v0.2.155.2 (2026-10-04)

- **fix(cluster): infra stack mounts the site certificate at its VERSIONED path (prod outage — apps 000/404).** The L4 platform-via-DSL conversion of infra-stack.yml listed the TLS cert/key in the traefik service secrets array under their LOGICAL names (`cert`/`key`) → compose mounted them at `/run/secrets/cert` + `/run/secrets/key`. The rendered Traefik dynamic config, however, references the VERSIONED content-addressed paths (`certFile: /run/secrets/cert_ab64977f`, `keyFile: /run/secrets/key_a6411269`) — the legacy raw-compose template resolved `secrets(cert)` to the versioned name, so the two agreed before L4. Result after v0.2.155: Traefik logged `Unable to parse certificate /run/secrets/cert_ab64977f ... no PEM data`, could not build the default TLS store, dropped every router that needs the `letsencrypt` resolver, and all customer apps returned HTTP 000/404. Fix: infra-stack.yml service secrets now list `[[.CertSecretName]]`/`[[.KeySecretName]]` (the versioned names) so the mount path matches the dynamic config. Host certs already used versioned names and were unaffected. Regression test: `TestLoadComposeFile_InfraBYOMode` now asserts `- cert_v001`/`- key_v001` in the service secrets array. Found live on prod nextrum-sy-1 after the v0.2.155 rollout.

## v0.2.155.1 (2026-10-04)

- **fix(cluster): rebuild-on-missing also applies to cluster-managed secrets' reuse path (migration regression).** The v0.2.155 content-addressed naming (cert_<sha8> instead of cert_v042) made `EnsureVersionedSecret`'s reuse path return the content-addressed name whenever the stored DB hash matched the current content — without verifying that the swarm object actually exists. On clusters upgraded from the old `_v<N>` naming era the DB hash matches unchanged content while the content-addressed object was never minted, so the infra deploy failed with `secret not found: cert_ab64977f`. Reuse now requires BOTH the stored-hash match AND `SecretExists` on the content-addressed name; a missing object is minted (and old `_v<N>` objects GC'd). Regression test: `TestEnsureVersionedSecret_RebuildsWhenSwarmObjectMissing` (seeds legacy `cert_v042` + matching DB hash, asserts the content-addressed object is minted and the legacy one GC'd). Found live during the v0.2.155 prod rollout on nextrum-sy-1.

## v0.2.155 (2026-10-04)

- **feat(config): configs always live in the swarm — content-addressed names, no
  numbered increments.** Per the design directive (configs always come from the swarm;
  the DB is the index but still holds values so it can rebuild missing swarm objects;
  no numbered increments anywhere), every config and secret now uses content-addressed
  naming: `<base>_<sha256-first-8-hex>` (`store.SwarmConfigName` / `store.SwarmSecretName`).
  Identical content → identical name → reuse; a real change mints a new name. First
  creation gets the bare name when the hash is short enough — no `_v1` at creation.
- **Swarm-first translate.** `stacks.StoreConfigResolver` now reads config VALUES from
  the Docker config (`ConfigInspect` of the name derived from the DB row hash) when a
  Docker client is available; if the swarm object is missing it REBUILDS it from the DB
  value (labels `io.pmcluster.managed` / `pmcluster.base`) and falls back to the DB row
  content when Docker is nil (CLI remote/tests). `config_path()` mounts also resolve via
  the swarm.
- **Rebuild-on-missing repair.** `cluster update` gained a "Repairing swarm configs and
  secrets from the DB index" pass: every indexed config/secret row is ensured to have a
  matching swarm object, created from the DB value when absent (secrets decrypted via the
  cipher; non-recoverable ciphertexts warn and skip).
- **CLI + console materialize swarm objects.** `config create/edit/rollback` and
  `secret create/edit` mirror their objects into the swarm (`mirrorSwarmConfig` /
  `mirrorSwarmSecret`, content-addressed, skip-when-exists). The daemon wires
  `StoreConfigResolver{Docker}` and `configs.Local{Docker}`.
- **Writer wiring.** `stacks.Service.configExternalName` feeds `ComposeWriter.ConfigNames`
  at all four render sites so `config_path()` mounts auto-reference the versioned swarm
  config; `secretExternalName` is content-addressed (hash-based, not `_v<rev>`).
- Removed the numbered-increment naming everywhere: platform `EnsureConfig` /
  `EnsureVersionedSecret` mint content-addressed names with GC of stale `<base>_*`
  objects; secret/config `_v<rev>` counters are gone (migration 0023 `swarm_rev` remains
  on the table but is no longer used for naming).

## v0.2.154 (2026-10-04)

- **feat(dsl): `secret(<name>)` prints a secret VALUE as-is — replaces the platform
  placeholder sentinels.** New reference kind `secret(name)` (whole env value or
  inline) resolves to the secret's *value* (unlike `secrets(name)`, which mounts a
  Docker secret file at `/run/secrets/<name>`). Manifest translation resolves it via
  an optional `SecretValueResolver`; refs.ReplaceRefs resolves it via an optional
  `SecretValueResolver` interface; malformed-ref validation advertises `secret(name)`.
- **Platform observability now uses the shared reference language instead of bespoke
  placeholders.** `observability-stack.yml` env `ZO_ROOT_USER_PASSWORD` is now
  `secret(oo_admin_password)` (OpenObserve only accepts the root password via env —
  it cannot read it from a mounted secret file); `otel-collector-config.yml` exporter
  `Authorization` is now `secret(oo_basic_auth)`. Both resolve through
  `renderRefResolver.ResolveSecretValue` / a new refs-style `otelRefResolver` in
  `internal/cluster/templates.go`. The `__OPENOBSERVE_PASSWORD__` /
  `__BASIC_AUTH_PLACEHOLDER__` sentinels are gone; `RenderOTelCollectorConfig` now
  renders through `refs.ReplaceRefs`. `renderRefResolver.ResolveSecret` restored
  (maps cert/key via `secretNameAliases`) so it satisfies the full `refs.RefResolver`
  surface.
- **Tests:** `refs_test.go` `TestReplaceRefs_SecretKind` + `TestParseEnvRef_SecretKind`
  (secret refs resolve via `SecretValueResolver`, error + text-preservation without
  one); `translate_test.go` `TestTranslate_SecretEnvRef` (secret() value lands in env
  verbatim, resolver-missing and validate-path error messages mention
  `secret(name)`).

## v0.2.153 (2026-10-04)

- **feat(dsl): `expose.external` publishes a real swarm port; `protocol` dropped.**
  `expose` gained `external: <port>` — when set, the exposed container port is also
  published to the swarm ingress (`target=port`, `published=external`, TCP), so a
  service can be reached directly (not only through Traefik). `host` is now only
  required when `external` is unset: `expose` with `external` and no `host` publishes
  the raw port with no Traefik router or routing-network membership (used for the OTel
  collector's node-local OTLP endpoint and Traefik's own 80/443). `ports` dropped the
  `protocol` field (all publishing is TCP) and gained `mode` (`ingress`/`host`).
  Platform embeds updated: `otel-collector` now uses `expose {port: 4318, external:
  4318, mode: host}` instead of a `ports` list.
- **feat(dsl): `config_path(<name>)` helper unifies config file mounting.**
  `configs:` entries are now `config_path(<name>)` expressions (one syntax for app and
  platform DSL). The config is mounted at the resolved path (default `/etc/<name>`)
  and the rendered compose references the versioned Docker config automatically.
  Traefik dynamic config → `/etc/traefik/dynamic/conf.yml`, OTel collector config →
  `/etc/otel-collector-config.yaml`. Validation errors for malformed expressions.

## v0.2.152.2 (2026-10-04)

- **fix(cluster): storage node is defaulted before platform stacks are rendered.**
  `cluster up` used to default `storage_nodes` to the leader hostname in
  `persistInstallState` — i.e. AFTER the platform stacks were deployed. The first
  `cluster update` therefore switched `backup_volume-backup` from replicated to global,
  which Docker rejects in place (`service mode change is not allowed`). The defaulting
  now runs as its own early workflow step (`Defaulting storage node`, extracted into a
  `defaultStorageNode` helper) before the render is built, so the backup agent is
  deployed global and storage-node-constrained from the very first bring-up and the
  first update is a content-aware no-op. Regression test:
  `TestUp_BackupRendersGlobalWhenStorageNodeDefaulted`.

## v0.2.152.1 (2026-10-04)

- **fix(cluster): legacy zero-padded name migration in EnsureConfig /
  EnsureVersionedSecret.** Clusters that minted `cert_v042` / `key_v042` /
  `pmcluster_otel_config_v005` under the old `%03d` naming were broken by the
  v0.2.152 naming unification: the reuse path reconstructed the current name as
  unpadded (`cert_v42`) without verifying it exists in the swarm, so the next
  `cluster update` / `cluster up` failed with `secret not found: key_v42` /
  `config not found: pmcluster_otel_config_v4`. The fix tracks the ACTUAL
  highest-version name present in the swarm (`maxName`) and reuses it verbatim;
  only genuinely new content mints `_v<N+1>`. Regression tests:
  `TestEnsureVersionedSecret_ReusesLegacyPaddedName`,
  `TestEnsureConfig_ReusesLegacyPaddedName`.

## v0.2.152 (2026-10-04)

- **feat: ONE pipeline for everything (consolidated L4 directive).** Platform stacks —
  `infra`, `edge`, `observability`, `backup`, `sso` — are no longer rendered as raw
  Compose via a separate path. The five embedded manifests are now DSL manifests
  (`app.platform: true`) that flow through the SAME pipeline as customer app stacks:
  `LoadComposeFile` runs the Go-template pre-pass (ACME/SSO/backup-branch conditionals
  + `${DOMAIN}`-style substitution) and then hands the result to
  `manifest.Parse → Interpolate → Validate → BuildIR → ComposeWriter`. One renderer,
  one hash, one deploy/reconcile/purge path.
- **feat: DSL surface extended for platform needs.** `app.networks` + per-service
  `networks` (join shared external overlays; the per-stack private net is skipped when
  app networks are set), `app.volumes` map (platform named volumes like
  `openobserve_data`/`traefik_acme`/`pmui-data` are declared verbatim — never relocated),
  `mode: global`, `restart`/`restart_delay`, raw `constraints`, `binds` (host paths
  emitted verbatim), `ports` (target/published/protocol/mode), `configs` mounts,
  `extra_hosts`, `resources` (cpus/memory reservations+limits), `user`, raw `labels`,
  `logging` (driver+options), `healthcheck.start_period`. New `settings(name)` env-ref
  kind resolves cluster settings at render time (`settings()` env refs, `ResolveSetting`
  on the resolver). `io.pmcluster.platform=true` is stamped on every service of a
  platform stack.
- **feat: unified rendered-config primitive.** The two non-service platform artifacts —
  the Traefik dynamic config and the OTel collector config — are now handled by one
  `RenderedConfig` abstraction (`EnsureRenderedConfigs`), so they version/persist/render
  exactly like every other platform artifact.
- **feat: versioning unified (user: keep the rev counter).** `EnsureConfig` /
  `EnsureVersionedSecret` now mint unpadded `_v<N>` names (were `_v%03d`), matching the
  user-secret swarm_rev counter. `cert_v1`, `pmcluster_otel_config_v1`, etc.
- **feat: user manifests may not claim `platform: true`.** The deploy/webhook path
  refuses the flag (reserved for pmcluster's own stacks), so a customer manifest cannot
  stamp `io.pmcluster.platform=true` on swarm services.
- Migration note: platform stack rendered output changes shape (env maps, label maps,
  long-syntax ports, platform label), so the first `cluster update` after this release
  performs one forced redeploy of the platform stacks — BUG-009 (fixed in v0.2.148)
  makes a failed redeploy retry on the next update.

## v0.2.151 (2026-10-04)

- **fix(cli): `credentials rotate` now applies the rotation immediately (BUG-012).** Rotating a
  credential (e.g. `openobserve_admin`) updated the DB row + re-materialized the swarm secret,
  but dependent configs (`pmcluster_otel_config` embeds the OpenObserve basic-auth header) and
  stacks (`observability` — OO reads `ZO_ROOT_USER_PASSWORD` at start) were only refreshed by a
  manual `cluster update`, so the rotation had no runtime effect until then. `runCredsRotate` now
  runs the cluster-update pipeline after a successful rotate (via an injectable `credsUpdateFn`
  seam + `applyRotatedCredential` helper), mirroring the per-host cert refresh. Regression tests
  `TestApplyRotatedCredential_TriggersClusterUpdate` / `TestApplyRotatedCredential_ErrorSurfaces`
  in `internal/cli/credentials_test.go`. Found live during Test Case 11 (credentials rotate).

## v0.2.150 (2026-10-04)

- **fix(cluster): per-host certs are now mounted into the Traefik service (BUG-011).** The
  Traefik dynamic config correctly referenced `hostcert-<host>_vNNN` / `hostkey-<host>_vNNN`
  in `tls.certificates`, but the `infra` stack never mounted those versioned secrets into
  the `traefik` service — so Traefik logged `Unable to parse certificate ... failed to find
  any PEM data` and served the default cert for per-host SNI. The `infra-stack.yml` template
  now iterates `HostCerts` and mounts every cert/key secret (service `secrets:` list + top-level
  `external: true` declarations). Regression test asserts both secrets appear in the rendered
  infra stack.
- Found live during Test Case 7 (per-host TLS) on the single-manager test node: BUG-010
  (row persisted after refresh) is fixed in v0.2.149, and BUG-011 (secrets not mounted)
  completes the per-host TLS path.

## v0.2.149 (2026-10-04)

- **fix(cluster): `tls hosts add` now applies the per-host certificate immediately (BUG-010).** `ApplyCert` ran the refresh pipeline (which re-renders the Traefik dynamic config from the `site_certs` table) *before* persisting the new row, so the first per-host cert was stored but Traefik kept serving the default certificate until some later `cluster update`. The per-host row is now persisted before the refresh. Regression test `TestApplyHostCert_PersistsBeforeRefresh` (fails on the old ordering, passes with the fix). Found during Test Case 7 (per-host TLS).

## v0.2.148 (2026-10-04)

- **fix(cluster): a failed platform-stack deploy no longer marks the stack
  up-to-date (BUG-009).** During the v0.2.147 rollout the backup stack's switch
  to storage-node mode hit Docker's "service mode change is not allowed", and
  `cluster update` then refused to redeploy it forever: the rendered hash was
  stamped into the store *before* the deploy ran, so a failed deploy left the
  hash matching the fresh render and every subsequent update skipped the stack
  even though the swarm never received it. Now each stack's rendered snapshot is
  stamped only *after* its deploy succeeds — a failed deploy leaves the stored
  hash stale and the next `cluster update` retries. Regression test
  `TestUpdate_FailedDeployLeavesStaleHashRetriedOnNextUpdate` proves the fix
  (fails on the old code: hash changes after the failed deploy + backup never
  redeployed).

## v0.2.147 (2026-10-04)

- **feat(cluster): backups run only on storage nodes.** The volume-backup agent is
  now constrained to nodes carrying the `pmcluster.storage=true` node label
  (label const `runtime.StorageNodeLabel`). Three-way rendering in the backup
  stack: `backup_all_nodes=true` → global on every node; otherwise when the
  `storage_nodes` setting is non-empty → global constrained to
  `node.labels.pmcluster.storage == true`; otherwise → the legacy replicated /
  platform-node / manager fallback. The label is set automatically when the
  default storage node is recorded at `cluster up` (leader), when
  `pmcluster join --storage-node` registers a node, and repaired on every
  `cluster update` so `storage_nodes` and node labels can't drift.
- **test:** three-way backup template rendering, up default-storage labels the
  leader, update repairs labels for every storage node, join `--storage-node`
  also runs `docker node update --label-add pmcluster.storage=true`.
- Requires a `docker stack deploy` of the backup stack (cluster update) to take
  effect; OTel collector already runs `global` (one per node) and OpenObserve
  log/metric ingestion is unchanged.

## v0.2.146 (2026-10-04)

- **fix(cli): secret rotation now works for live stacks (BUG-007).** Docker Swarm
  secrets are immutable and cannot be removed while a running service references
  them, so `pmcluster secret edit` silently updated only the DB — redeployed
  containers kept the old value with just a WARN line. Secrets now carry a
  `swarm_rev` counter (migration 0023); every edit bumps the revision and the CLI
  mirrors the new value into a **versioned swarm secret** `name_v<rev>`, leaving
  the in-use original untouched. The compose writer emits the versioned external
  name via a `SecretNames` resolver while the container mount path stays
  `/run/secrets/<logical-name>`. Verified empirically that `docker stack deploy`
  honors the `name:` override on external secrets.
- **fix(webhooks): retry policy is now live on the receive path (BUG-008).** The
  `MaxRetries`/`RetryDelay` wiring was dead code — `receiver.go` called
  `DeployAsync` directly and every delivery row recorded `retries:0`. The receiver
  now goes through `DeployAsyncWithRetries` under a phase budget; synchronous
  deploy failures are retried (2 retries, 30s apart) and the actual retry count is
  recorded on the delivery row and in the 502 body. Background (async) apply
  failures remain fire-and-forget and are surfaced via stack `last_error` and the
  reconcile loop, as before.
- **test:** store swarm_rev bump + `SwarmSecretName` cases, compose
  `name:` override rendering, receiver retry counts (502 body + delivery row),
  `DeployAsyncWithRetries` sync-phase retries.

## v0.2.145 (2026-10-04)

- **fix(cli): control-plane restore is startup-only — the leadership goroutine no
  longer restores over the live store (split-brain data-loss risk).** Found during
  Test Case 2 live testing right after the v0.2.144 WAL fix. On every Swarm
  leadership gain the daemon re-ran `ensureControlPlaneFresh` *after* `store.Open`,
  so `Restore` truncated `data.db` under the daemon's live SQLite connection:
  the daemon kept serving stale page-cache rows while fresh readers (CLI,
  `python3`) saw the clobbered file — the same split-brain symptom class as the
  WAL bug. Reproduction: leader daemon running → create a user via the CLI →
  restart the daemon → the user survives auth (stale cache) but is gone for fresh
  readers. **Fix:** restore now happens only at startup, before the store is
  opened (the standby/failover path). On leadership gain the daemon only
  *snapshots* the fresh control plane into the Raft-replicated configs. Verified
  live on the test node: CLI-created users now survive daemon restarts with full
  cross-process consistency (`integrity_check` = ok, all readers agree).

## v0.2.144 (2026-10-04)

- **fix(store): eliminate the multi-process WAL corruption bug (data-loss risk).**
  Found during Test Case 2 webhook/CLI feature testing. The control-plane DB was
  opened in `journal_mode=WAL` by both the long-running daemon and short-lived
  CLI processes (and any other reader, e.g. `python3`). When a second process
  wrote and closed its connection, SQLite deleted the `-wal`/`-shm` sidecars
  even though the daemon still held them open. The daemon then kept writing to
  the orphaned inode, producing a split-brain database: fresh CLI-created rows
  (users, tokens, settings) were invisible to the daemon (all auth 401s), index
  corruption appeared (`wrong # of entries in index idx_backups_started`), and
  `user list` failed with `database disk image is malformed`. Reproduction:
  open the store, have a second connection write+close, watch `data.db-wal`
  disappear while the first handle keeps it open. **Fix:** both stores (daemon
  `internal/store` and console `internal/ui/store`) now use `journal_mode=DELETE`
  — transactions commit directly to the main file, there are no long-lived
  sidecars to orphan, and a writer's close can never detach the DB from an open
  handle. `Store.WALCheckpoint` is now a no-op (nothing to flush). Regression
  test `TestMultiProcessNoWALSplitBrain` covers open/write/close/reopen
  cross-visibility and asserts no `-wal`/`-shm`/`-journal` sidecars ever exist.

## v0.2.143 (2026-10-04)

- **fix(cluster): `cluster update` now ensures the storage root directories exist.**
  Found during Test Case 2 (custom `volume_root`). `ensureStorageDirs` was only
  called from `cluster up` — changing `volume_root` after bring-up left the new
  root missing, so `backup_volume-backup` tasks were rejected with
  `bind source path does not exist`. `update.go` gained an explicit
  *"Ensuring storage root directories exist"* step.
- **fix(backup): `backup restore` default `--dest-root` now follows the cluster's
  `volume_root` setting** instead of the hardcoded `/var/stack/data`. Both the
  CLI (`restoreDestRootDefault`) and the REST API (`backups.HTTP.VolumeRoot`,
  wired from the store) resolve the setting, falling back to
  `manifest.DefaultVolumeRoot` when unset. BUG-002/BUG-003 from
  `docs/test-reports/TC2-backup-restore-s3-acme.md`.
- **docs: Test Case 2 report** — single-manager, Let's Encrypt ACME certs (all 3
  domains, Verify code 0), custom volume root `/srv/stack/data`, S3 upload +
  local/S3 restore round-trips proven (`docs/test-reports/TC2-backup-restore-s3-acme.md`).

## v0.2.142 (2026-10-04)

- **fix(cli): `WatchSwarmLeadership` re-emitted `leader-true` on every 15s poll.**
  Found during Test Case 1 on a live single-manager cluster. After emitting
  `leader=true` the function reset `current=false`, so every poll re-emitted
  `true` forever. The `serve` leadership goroutine reacted by cancelling and
  restarting the reconciler loop **and the control-plane snapshot loop every
  15s** — meaning `pmcluster_state_*` Raft snapshots silently never ran after
  startup (a data-loss window if the manager disk dies), plus repeated
  `local control-plane DB is current` restore spam.
  - Fix: the leader branch now sets `current, first = true, false` so a
    still-leader poll stays silent; the err/standalone branch was already correct.
  - Tests: `TestWatchSwarmLeadership_StableLeaderEmitsOnce` (one emit then
    silence over `2*leaderPollInterval`; fails without the fix) and
    `TestWatchSwarmLeadership_LeaderToNotLeaderReEmits` (race-safe leadership
    flip → `false` emit after the flip).
  - Verified live on `nxt-sw-3-w`: exactly one `became swarm leader` emit after
    restart and a fresh snapshot at the next 5-min tick.
- **Docs: first test report** `docs/test-reports/TC1-single-manager-bringup.md`
  (single-manager setup, multi-service deploy, logs/metrics, UI, control plane).

## v0.2.141 (2026-10-03)

- **Interactive `docker exec -it` over websocket (L3) — browser terminals.**
  The console now ships a real terminal into any service container:
  - **Daemon endpoint** `GET /api/services/{stack}/{service}/exec/ws`
    (`internal/server/execws.go`): Bearer-auth + stack-scope guard, runs an
    interactive TTY exec (`ContainerExecCreate{Tty}` + hijack) against the
    service's running task, and pumps frames — binary in = stdin, binary out
    = raw TTY output, `{"type":"resize",rows,cols}` text controls, and a
    final `{"type":"exit","code":N}` frame. `?cmd=` picks the program
    (default `sh`); `?rows=`/`?cols=` seed the size. Dispatched **outside**
    the `otelhttp`/status-span wrappers (neither implements
    `http.Hijacker`).
  - **New runtime port** `runtime.ServiceExecAttach` +
    `runtime.ExecStream` (Read/Write/Resize/Wait with live exit-code
    polling); Docker adapter does the exec hijack; `services.ExecAttacher`
    optional interface so the remote adapter is untouched.
  - **Console**: `GET /web/stacks/{name}/terminal?service=X` standalone
    xterm.js page (vendored, no CDN) and `.../terminal/ws` relay that
    bridges the browser connection to the daemon with the server-side API
    token (the browser never sees it). Operator role only. A **Terminal**
    button on every service row in the stack detail + services tables
    (Arabic labels included).
  - E2E-verified style tests: docker TTY attach (raw hijack), daemon WS
    handler (auth + frame pump + resize + exit frame), console relay round
    trip (browser→console→daemon→console→browser), page render. **Bug found
    by tests:** `execStream.done` channel was never initialized → Close
    panic; fixed.

## v0.2.140 (2026-10-03)

- **`pmcluster join --storage-node` — mark a joining node as a storage node.**
  New opt-in flag on `join`: after joining the Swarm, the node's hostname is
  added to the manager's `storage_nodes` cluster setting (reached over ssh), so
  round-robin placement can spread stateful stacks onto it too. **Workers
  qualify — storage is not leader-only.** Best-effort: if the ssh registration
  fails the join still succeeds and prints the manual
  `pmcluster cluster settings set storage_nodes=...` command to run instead.
- **The main node is a storage node by default.** `cluster up` now writes the
  leader's hostname into `storage_nodes` when the setting is empty, so a
  fresh cluster pins every stateful stack to the main node from day one.
  An operator-configured list is never clobbered.
- **Every service carries an `io.pmcluster.node` label** naming the node its
  placement pin targets (a resolved storage node / hostname pin; role-based and
  unconstrained services carry none). The console **Services** table, the
  **stack detail** services panel, the service card, and `pmcluster service ps`
  all display this node column — you can see where every stateful service runs
  at a glance (UI labels in English + Arabic).
- **Live `storage_nodes` refresh.** The daemon's settings hook hot-swaps the
  storage-node list when `storage_nodes` changes (no restart); the next
  reconcile re-renders and redeploys affected stacks.

## v0.2.139.1 (2026-10-03)

- **Fix: worker-node daemon crash at startup (control-plane restore).** The L2
  config-based restore passed the Docker client into the startup
  `ensureControlPlaneFresh` check; on a worker node Docker answers
  "This node is not a swarm manager", which is not the sentinel error the code
  expected, so the daemon treated it as fatal and exited (systemd FAILURE).
  `internal/controlplane` now detects the not-a-swarm-manager error and falls
  back to the tarball path (`Restore` → `ErrNoSnapshots`, `Snapshot` → no-op),
  so worker daemons start cleanly again. Deployed to the nextrum-sy-2 worker.

## v0.2.139 (2026-10-03)

- **`storage_nodes` setting + round-robin placement for stateful stacks (P3).**
  New cluster setting `storage_nodes` (comma-separated hostnames, e.g. via
  `pmcluster cluster settings set storage_nodes=node-a,node-b`). Stateful services
  (volume holders) with **no explicit placement** are now pinned deterministically
  across the listed nodes — FNV-1a of the stack name picks the node, so the same
  stack always lands on the same node and adding stacks never reshuffles existing
  ones. Explicit `placement:` (hostname / `manager` / `worker`) always wins; the
  `platform_node` setting remains the fallback when `storage_nodes` is empty.
  A change to `storage_nodes` changes the rendered hash, so the reconcile loop
  re-deploys affected stacks on the next pass.
- **Storage-outage pause in the control loop (P4).** The reconcile loop now builds
  a live node-health map (swarm `NodeList`: present, `Status==ready`,
  `Availability==active`) each pass and **skips syncing/deploying any stack whose
  storage pin is on a down node**, logging a warning ("storage node down; pausing
  app sync"). The pause clears automatically when the node returns — or when the
  stack is moved. This stops the loop from churning against a node that cannot
  serve its volumes.
- **`pmcluster stack move <stack> --to <node>` (P5).** Moves a stateful stack's
  storage to another node: triggers a whole-disk on-demand backup, restores the
  stack's `<stack>/` subtree on the target (locally when the target is this host,
  else via a one-shot swarm mover service pinned to the target that wget's the
  archive from an ephemeral HTTP server on the leader), writes the per-stack
  `stack_pin_<stack>` setting, and re-deploys so the new pin is rendered. The
  per-stack pin **outranks** the `storage_nodes` round-robin, so the reconcile
  loop never moves the stack back. Errors loudly on: unknown stack, no stateful
  storage, empty `--to`, missing backup trigger, a target that is absent or not
  ready/active in the swarm.
- **Path 1 is the HA answer (docs decision).** Replaced the "LINSTOR is the HA
  prerequisite" design directive with the Path 1 decision: pinned placement +
  backup/restore is the HA story (storage_nodes round-robin, outage pause,
  `stack move`), and LINSTOR/DRBD block replication is explicitly off the roadmap
  with its honest cost table preserved. Removed the L0 LINSTOR backlog item.

## v0.2.138 (2026-10-03)

- **Control-plane survivor kit via Raft-replicated Docker configs (L2).** The swarm
  leader now snapshots the failover-survivor kit (data.db with credential ciphertexts,
  settings, webhook secrets, token hashes, stack revisions; config.yaml; the rendered
  `config/` tree with TLS certs) into `pmcluster_state_<unixnano>` Docker configs. Swarm
  replicates configs to every manager through its own Raft store, so a promoted standby
  gets the latest control plane **without the tarball/rsync dependency**. **Security
  split:** the AES-GCM encryption key is written to a second config family
  (`pmcluster_state_key_<unixnano>`) so key and ciphertext never share one blob — a
  restore refuses a state config whose key config is missing, and the key never appears
  inside the data-kit payload.
  - **Freshness:** on `serve` startup and on leadership gain,
    `ensureControlPlaneFresh` restores from the newest state config when the local
    data.db is missing or older than the snapshot (and repairs a missing encryption key
    even when the DB is current). A local DB at least as fresh as the snapshot (the
    shared-storage/LINSTOR case) is never clobbered. Clusters without state configs fall
    back to the existing tarball archives.
  - **Snapshot cadence:** the leader publishes a snapshot immediately on promotion and
    re-checks every 5 minutes, re-snapshoting only when a kit file changed — so a deploy,
    settings change, secret rotation or webhook edit lands on all managers within one
    interval. Snapshots are pruned to the newest two per family.
  - **Guards:** payloads over the Docker config ceiling (500 KiB) fail loudly instead of
    half-writing; snapshots skip `logs/`; restored data.db is timestamped at the snapshot
    so repeated promotions never re-restore. `cluster down --purge` wipes the state
    configs with the rest of the managed footprint (the local SQLite survives by design,
    and the next leader re-snapshots it).
  - **Backup stack honors a custom storage root.** The embedded backup stack previously
    hardcoded `/var/stack/data` and `/var/stack/backup` as bind sources, so any
    non-default `volume_root` setting broke the volume/control-plane backup agents
    ("bind source path does not exist"). The compose template now renders `[[.VolumeRoot]]`
    and `[[.BackupDir]]` from the effective `volume_root` setting and the backup dir
    (default `/var/stack/backup`, overridable via the `PMCLUSTER_BACKUP_DIR` env var for
    hermetic/dev hosts that cannot create `/var/stack`).
  - **Snapshot interval overridable.** `PMCLUSTER_CONTROLPLANE_SNAPSHOT_INTERVAL` (seconds,
    default 300) tunes how often the leader re-checks the kit for changes — used by the
    e2e suite to exercise the snapshot loop at 1s.
  - **E2E verification.** New swarm-gated `TestControlPlaneSnapshotE2E` drives the whole
    loop against a live Swarm (Docker Desktop): both config families appear, the
    security split holds (data.db present, no `.encryption_key` entry, 32-byte key
    absent from the state payload), a store mutation is captured within one interval, and
    prune settles each family to at most two snapshots.

## v0.2.137 (2026-10-02)

- **Edge console is stateless in cluster mode (Path 1, P1).** With
  `EDGE_LOGIN_DISABLED=true` (the normal Swarm deployment, gated by Traefik
  admin-auth/sso-auth) the edge console now runs **completely without the
  `edge_pmui-data` volume**: its API link + token come from env vars
  (`PMCLUSTER_API_URL` / `PMCLUSTER_API_TOKEN`), the SQLite store opens
  in-memory (`:memory:`), the session is the synthetic admin, and nothing is
  persisted or written back. Standalone runs (login enabled) keep the local
  SQLite config file exactly as before. The edge stack template drops the
  volume declaration and mount when login is disabled. This removes one more
  stateful component from the cluster plane.

- **Tailscale option (M3).** `pmcluster join`, `pmcluster cluster up` and
  `pmcluster setup` gained opt-in `--tailscale` (+ `--tailscale-auth-key`, default
  `$PMCLUSTER_TAILSCALE_AUTH_KEY`). When set, the node is brought onto a private
  WireGuard tailnet via the `tailscale` CLI (`tailscale up` then `tailscale ip -4`)
  and the Swarm join/init advertises the **tailnet IPv4** — so node-to-node swarm
  traffic (2377/7946/4789 + any storage ports) needs no firewall rules between
  nodes. Explicit opt-in fails loudly on tailnet errors (a silent fallback to the
  public IP would defeat the purpose). No change when the flag is absent.

## v0.2.136 (2026-10-01)

- **Backup restore completion (M2).** `pmcluster backup restore <id>` gained two knobs and a resilient source story:
  - `--volume <name>` (and `volume` on `POST /api/backups/{id}/restore`) restores only one volume — matched as a full path segment inside the archive, so a per-volume restore never leaks entries from other volumes or stacks. The console's Restore form gained the volume field.
  - Restores are **local-first**: an archive still on the node's archive dir is restored from disk. When it is gone (pruned, or the node never held it) the archive is **fetched from the configured offsite S3 store** (`backup_s3_*` settings) — implemented with a minimal AWS SigV4 GET client (stdlib, path-style, R2-compatible; no new dependency). `--from-s3` forces the fetch even when a local copy exists. With no local archive and no S3 configured, the command fails loudly and says exactly where the archive lives (and that configuring `backup_s3_*` enables the fetch) instead of silently restoring nothing.
  - Refused control-plane archives, the `/backup/data` prefix strip and the path-escape guard all still apply to whichever source the bytes came from.

## v0.2.135 (2026-10-01)

- **Control-loop OTLP traces.** The reconcile loop now emits OpenTelemetry spans through the existing telemetry pipeline (visible in OpenObserve once the collector is connected): a `pmcluster.reconcile` root span per pass with the `run_id` attribute, and a `pmcluster.reconcile.platform` child span for the platform-render/hash-compare pass carrying `stacks_redeployed` when drift is applied. Errors are recorded on the span with a non-OK status. Span creation is lazy and no-op until `telemetry.Init` wires the global provider (same pattern as the deploy/rollback tracers).

## v0.2.134 (2026-10-01)

- **Control-loop logging.** The reconcile loop now logs `info` for its lifecycle (pass started/completed, skipped-when-in-flight) and every actionable result (drifted app stack synced with its new revision, platform stacks redeployed), and `debug` for the detail (per-service and per-stack status derivation with replicas/desired/update-state/run-once, each Swarm event that triggers a pass, debounce/safety-tick arming, stale-row pruning, and no-op "converged" syncs). `info` stays clean for day-to-day logs; `debug` shows exactly what the loop is doing.

## v0.2.133 (2026-10-01)

- **Swarm e2e for the control loop.** `TestControlLoopE2E` (PMCLUSTER_E2E_SWARM=1) drives a real single-node cluster through the loop: cluster up → `reconcile_interval=1` → serve → deploy a stack referencing a config → badge reads the DB snapshot and flips to `healthy` → edit the config (drift with no deploy trigger) → the loop auto-syncs it (revision advances) → scale the service to 0 → the badge flips to `degraded` → teardown. Covers loop snapshot writes, drift convergence, DB-driven badges, and non-restart semantics.

## v0.2.132 (2026-10-01)

- **Control loop (L1) — leader-only converge loop.** The Swarm-leader daemon now runs a reconcile loop (event-driven with a `reconcile_interval` safety tick, default 60s, `0` disables). Each pass: (a) re-renders the platform configs and re-deploys drifted stacks (the content-aware reconcile, no TLS/credential churn), (b) re-translates each app stack's latest source and syncs when the rendered hash drifted (no-op when unchanged), (c) writes a per-stack + per-service health snapshot to the new `stack_status` table. One pass at a time; the loop never restarts services — it only reports health.
- **Badges now read the DB snapshot, never live Docker.** `/api/public/badge/*` reads the `stack_status` snapshot the loop writes (healthy / in progress / degraded / error / unknown), so a single badge fetch no longer queries the swarm and the badge reflects converged state rather than per-request flicker. Unknown when no snapshot exists yet.
- **Leader channel.** `WatchSwarmLeadership` streams leadership changes; the daemon starts the reconcile loop and re-runs the control-plane restore check on promotion, stops the loop on leadership loss. The loop runs only on the leader node.
- **Docker events stream.** `runtime.Client.Events` subscribes to the engine's event stream (neutral `Event` type: type/action/stack/service/node/timestamp) — the loop's primary trigger, with the interval tick as the safety net.
- Migration `0022_stack_status.sql` (stack_status table: stack_name PK, status, services JSON, updated_at).

## v0.2.131 (2026-10-01)

- **Badge endpoints answer HEAD too.** The public badge routes only registered `GET`; chi fell HEAD requests through to the Bearer-protected `/api` group, which answered `401` — and GitHub's camo image proxy (and some image tools) preflight with HEAD, so the badge was refused and rendered broken in READMEs. All three badge routes (`/api/public/badge/{stack}`, `.../services`, `.../{service}`) now register both `GET` and `HEAD` with the same handler. Regression test added.

## v0.2.130 (2026-10-01)

- **Badge status word: `deployed` → `healthy`.** A service can be unhealthy *after* it deploys (crash-loop, a bug) — so the green state now reads **`healthy`**, not `deployed`, across the stack badge, the per-service badge, the combined `/services` badge, the CLI help, and the console copy text. Semantics unchanged: green = deployed *and* healthy; `in progress` / `degraded` / `error` / `unknown` as before.

## v0.2.129 (2026-10-01)

- **Combined stack + services badge.** `GET /api/public/badge/{stack}/services` returns ONE wide SVG showing the stack's main health first, then a segment per service (label = service name without the `stack_` prefix). Same status colors + no-auth + 60s cache. `pmcluster stack badge <stack> --services` prints the markdown; the console stack detail card gained a second copyable row for it. (Service-level badges from v0.2.128 ride along — both shipped in this release's binary.)

## v0.2.128 (2026-10-01)

- **Service-level status badges.** `GET /api/public/badge/{stack}/{service}` returns the same flat SVG scoped to one service (label `stack/service`): `in progress` while updating, `error` when paused (excluding completed one-shot jobs), `deployed`/`degraded`/`unknown`. Same no-auth + 60s cache as the stack badge.
- **Completed one-shot jobs no longer flag `error`.** The stack badge treated any service with a stale `update_state=paused` as failed — but Swarm leaves that marker behind when a `run_once` job finishes. A completed job (run_once, desired>0, 0 replicas) now reads as `deployed`. This is why the donation-campaign **prod** badge showed red despite a clean deploy history.

## v0.2.127 (2026-10-01)

- **Public stack-status badge for READMEs.** New unauthenticated endpoint `GET /api/public/badge/{stack}` returns a shields.io-style flat SVG reflecting the stack's live state: `deployed` (green), `in progress` (yellow — any service mid-update), `degraded` (red — under-replicated, excluding completed one-shot jobs), `error` (dark red — the current revision has a failed deploy outcome in the error history), or `unknown` (grey). Public on purpose: GitHub's README image proxy (camo) fetches it without credentials; cached 60s.
- **`pmcluster stack badge <stack>`** prints the ready-to-paste markdown line (`![<stack> status](https://pmcluster.<domain>/api/public/badge/<stack>)` — remote-mode origin, or the cluster's configured domain).
- **Copy badge from the console**: the stack detail page shows the markdown with a copy button (`data-copy`) when the console knows the cluster domain. EN/AR labels + hint added.

## v0.2.126 (2026-10-01)

- **Errors-only deploy history — no empty writes.** `RecordStackError` now records **failures only** (an empty error writes nothing); a clean deploy leaves the history untouched. The console's "Deploy failed" banner/pill is keyed to the **current revision**: `newestStackError(errors, currentRevision)` shows the newest failed outcome only when it belongs to the current revision — so after a successful redeploy the banner clears (the clean deploy has no new entry) while older failures remain in the history panel for debugging. The revision join (v0.2.125) still surfaces per-execution errors (`""` = applied cleanly, derived from absence in the history). Tests updated for failures-only semantics.

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
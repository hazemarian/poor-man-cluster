# pmcluster architecture

One package per bounded context (domain), each owning its model types, its
port(s), a local adapter and its REST handler. Composition happens in exactly
two roots: `internal/cli` (backend factory — the **only** local/remote switch)
and `internal/server` (HTTP `Deps` + route mounting).

## Dependency rule

Arrows point inward. Domain packages never import `remote`, `server` or `cli`.

```text
cli ──► remote ──► domain packages (ports) ◄── server
 │                   ▲            ▲            │
 └── (local adapters ├────────────┘) ──────────┘
                      └── store / credentials / docker / cluster core
```

- `internal/<domain>` — model + port + local adapter + HTTP handler. Depends
  only on `store` (and `credentials`, `cluster` core) — never on transport.
- `internal/remote` — one shared REST-client family implementing the domain
  ports against the daemon API.
- `internal/cli` + `internal/server` — the two composition roots that wire
  adapters together. No other package may choose between local/remote.

## Domain inventory

| domain | models | ports | REST routes |
| --- | --- | --- | --- |
| `apikeys` | `APIKey` | `Service` (Create/List/Delete) · sentinels `ErrEdgeUserProtected`, `ErrSelfDelete` | `GET/POST /api/api_keys`, `DELETE /api/api_keys/{id}` |
| `webhooks` | `Source` | `Service` (Create/List/Delete) · `SourceReader` (Secret/MarkUsed, **local-only**) | `GET/POST /api/webhooks`, `DELETE /api/webhooks/{source}`, `POST /webhook/{source}` (HMAC receiver, unauthenticated), `GET /api/webhooks/{source}/deliveries` |
| `secrets` | `Secret` | `Service` (Create/Get/Reveal/List/Update/Delete) | `GET/POST /api/secrets`, `PUT/DELETE /api/secrets/{name}`, `GET /api/secrets/{name}/value` |
| `configs` | `Config`, `ConfigVersion` | `Service` (Create/Get/List/Update/Rollback/Delete/ListVersions/ListRendered) · `Renderer` (SetRendered, **local-only**) | `GET/POST /api/configs`, `GET/PUT/DELETE /api/configs/{name}`, `GET /api/configs/{name}/versions`, `POST /api/configs/{name}/rollback`, `GET /api/cluster/rendered` |
| `backups` | `Run` | `Service` (Trigger/List/ListForStack/ListFiles/Restore) · sentinel `ErrTriggerNotConfigured` | `GET/POST /api/backups`, `GET /api/stacks/{name}/backups`, `GET /api/backups/{id}/files`, `POST /api/backups/{id}/restore` |
| `certs` | `Cert` | `Service` (SiteCert/ApplyHostCert/RemoveHostCert/GetSiteCert/List/MainDomain) | `GET /api/tls/hosts`, `PUT/DELETE /api/tls/hosts/{host}`, `GET/PUT /api/tls/site` |
| `stacks` | `Payload`, `Result`, `Stack`, `Revision` | `Deployer` (Deploy/Sync/Rollback/Undeploy) · `Reader` (Get/List/Revisions) | `POST/GET /api/stacks`, `GET/DELETE /api/stacks/{name}`, `GET /api/stacks/{name}/revisions/{rev}`, `POST /api/stacks/{name}/rollback`, `POST /api/stacks/{name}/sync` |
| `services` | `ServiceSummary`, `TaskRun`, `ExecResult`, `LogLine` | `Reader` (List/Tasks) · `Ops` (Restart/Exec) · `Logs` · `Service` (Reader+Ops+Logs) | `GET /api/services`, `GET /api/services/{stack}`, `GET /api/services/{stack}/{service}/tasks`, `GET /api/services/{stack}/{service}/logs`, `POST /api/services/{stack}/{service}/restart`, `POST /api/services/{stack}/{service}/exec` |
| `cluster` | — (engine) | `Service` (Up/Update/Down/Status) · `CredentialsService` | — (no REST surface; bring-up/tear-down is local-only) |
| `settings` | — (key/value map) | `Service` (Get/Update) | `GET/PUT /api/cluster/settings` |
| `usage` | `Usage` | `Service` (Get) | `GET /api/usage` |

## Composition roots

`internal/server.Deps` (the daemon) wires every port to its local adapter:

```go
type Deps struct {
    Lookup        auth.Lookup            // store (bearer auth)
    Docker        docker.Client
    Store         *store.Store
    DeployService stacks.Deployer        // stacks.Service engine
    Cipher        *credentials.Cipher
    Backups       backups.Service
    HostCerts, SiteCert *certs.HTTP      // same tlsSvc, two mounts
    Update        *UpdateService         // POST /api/update
    Webhooks      webhooks.Service
    WebhookSources webhooks.SourceReader // local-only secret material
    APIKeys       apikeys.Service
    Configs       configs.Service
    Secrets       secrets.Service
    Services      services.Service       // per-service ops (/api/services)
}
```

`internal/cli/backend.go` provides one `backendXxx` helper per domain. Each
returns the local adapter when `--api-url`/`PMCLUSTER_API_URL` is unset, or a
`remote.NewXxx` client otherwise. Remote-capable helpers: `backendConfigs`,
`backendSecrets`, `backendWebhooks`, `backendAPIKeys`, `backendBackups`,
`backendTLS`, `backendDeploy`, `backendStacks`, `backendServices`. The cluster
lifecycle (`cluster.go`) and credentials (`credentials.go`) commands are
**always local** — they run the engine directly, not through `backend.go`.
The `setup` wizard (`setup.go`) falls back to `runClusterUp`/`runClusterUpdate`
for fresh installs and updates respectively.

## Local-only vs remote-capable

`internal/remote` implements a port for every domain **except**:

- `cluster.Service` and `cluster.CredentialsService` — there are no REST
  endpoints for bring-up/tear-down or bootstrap-credential rotation; these run
  only on the node holding the Docker socket. (Settings and usage live in the
  `settings` and `usage` domain packages; their REST handlers are remote-capable.)
- `webhooks.SourceReader` — the decrypted HMAC secret never crosses the wire;
  the receiver runs on the daemon.
- `configs.Renderer` — `SetRendered` snapshots post-substitution content, an
  operation only `cluster update` performs locally against the store.

Everything else (`apikeys`, `webhooks.Service`, `secrets`, `configs.Service`,
`backups`, `certs.Service`, `stacks.Deployer` + `stacks.Reader`,
`services`) has a `remote` adapter in `internal/remote`.

## GHCR pull credentials

Pulls use the **stored registry credential** — a long-lived read-only PAT
registered by `pmcluster registry add ghcr.io`. The daemon applies it at
startup (`replayRegistryLogins`) and **once per deploy**, immediately before
the first `docker stack deploy --with-registry-auth` freezes it into each
service spec (`stacks.Service.BeforeApply` → `loginBeforeDeploy`, wired in
`internal/cli/serve.go`, `deploy.go` and `cluster.go`). A multi-level
`depends_on` deploy still logs in once: the hook belongs to the deploy, not to
a level.

There is no ticker and no interval. A PAT lives 90 days, so refreshing it
every 25 minutes would be motion without benefit; the credential is applied
when a deploy needs it and replaced when an operator rotates it. Every
deploy-login is a WARN naming the registry, the credential's source and its
age — the operator's early warning for the 90-day expiry. `doctor`'s `registry
credential` check carries the same signal independently and warns from day 60,
never failing. `registries.created_at` is reset on rotation, so the age is
the current credential's, not the first-ever row's.

All logins hold one mutex (they write the same `config.json`), pin
`DOCKER_CONFIG=/root/.docker`, and pass the password on stdin, never argv. A
failed login warns with docker's stderr and does not abort the deploy.

The GitHub App (`internal/githubapp`) is **not** a pull credential: it mints
one-hour installation tokens for clone, build and the Actions broker. That
split is deliberate. `--with-registry-auth` embeds the credential current at
deploy into each service spec and never rewrites it; an hour-long token would
be dead long before a task is rescheduled onto a node without the image. The
PAT outlives the spec, so the frozen copy keeps working — the residual risk is
a revoked or expired PAT, which doctor reports rather than a timer preventing.

There is no credential helper, no second listener and no node-certificate
verification on the pull path; that machinery was built and removed because it
fixed only manual pulls. Pre-pull and an in-cluster mirror remain unimplemented;
they would only matter for a PAT revoked mid-deploy.

## Self-hosted GitHub Actions runners

`internal/runners` owns one ephemeral Actions runner container per configured
private repository (`github_runners_enabled`, off by default). The reconcile
loop calls `Manager.Reconcile` on every pass — its own pass, not a
content-hashed platform stack, because the spec embeds a single-use,
hour-lived registration token: hashing it would change on every render and a
stale token would be redeployed.

- **Registration** mints a repo-scoped token through `internal/githubapp`
  (`POST /repos/{owner}/{repo}/actions/runners/registration-token`) with the
  same cached installation token the broker uses. Nothing is persisted: the
  token is written into a rendered compose and handed to the container.
- **Desired vs actual** is discovered from service labels
  (`pmcluster.runner=true`, `pmcluster.runner.repo=<owner/name>`), so a
  container started by an earlier daemon is visible without trusting a table.
  A settings row (`github_runners_state`) records the runner name and
  registration time, which is what teardown needs once the container is gone.
- **Teardown** removes the stack and de-registers every runner with the
  reserved `pmc-` prefix for that repo (`DELETE .../runners/{id}`). It is
  retried: a failed de-registration keeps the booking entry.
- **Failures are warnings.** A refused token, a public repo, an unreachable
  GitHub are per-repo warnings appended to the last-apply record, so one broken
  repo never stops the rest and `doctor`'s `last apply` check still names them.
- **Placement** is in the rendered spec (`node.labels.pmcluster.leader != true`
  unless the operator lifts it), not a runbook: a runner builds images and
  competes with the control plane. `restart_policy: none` keeps an ephemeral
  runner off a spent token, `update_config: order: stop-first` prevents two
  containers registering the same name.
- **The security model is the docker socket.** A runner mounts
  `/var/run/docker.sock`, so a workflow on it is root on that host. Hence:
  private repositories only (a public repo's fork PR would run arbitrary code
  there), an explicit `self-hosted,linux,x64,pmcluster` label set, the reserved
  name prefix, and a feature that is off by default.

`doctor`'s `github runners` check reports the enabled state, per-repo
container/registration agreement (ghosts, offline runners), and the missing
`administration: write` permission that would otherwise appear as a bare 403.

## Leaf packages

`internal/docscheck` is a test-only leaf package — no domain port, no adapter, no
non-test code. It extracts checkable claims from the prose (cluster setting keys,
CLI commands and flags, OpenAPI paths against the registered routes, local links,
in-repo file references) and asserts each against the source, plus a static
Mermaid syntax lint for the failures a reader cannot see: keyword collisions in
participant ids, and a `;` smuggled in by an HTML entity, either of which breaks
a sequence diagram while looking perfectly correct. Every assertion in it exists
because the claim it checks was false here.

It runs in the `docs` workflow rather than the Go one, because it verifies the
prose, not the behaviour. `scripts/check-mermaid-render.py` covers the half that
needs a renderer — whether a diagram is legible once GitHub fits it to its
content column, which no amount of reading the markup will tell you.

`internal/refs` is a leaf package (no domain port, no adapter). It provides
the shared `config()/config_path()/secret()/secret_path()` reference language consumed by
`internal/manifest` (DSL env values) and `internal/cluster` (platform
templates via `renderRefResolver`). Key exports: `RefResolver` interface
(`ResolveConfig`, `ResolveSecret`), `ReplaceRefs` inline scanner,
`ParseEnvRef`, `MalformedEnvRef`, `SecretMountPath`.

## Wire invariants

- REST routes, JSON shapes and sentinel errors are **stable** — the `remote`
  DTOs mirror the handler shapes exactly.
- `errors.Is` keeps working over HTTP: `remote.mapError` translates non-2xx
  responses back into the store/service sentinels by status code + body
  substring, so CLI error handling is identical local vs remote.
- Models carry no `sql.Null*`; the local adapters are the only place that
  maps store rows (which do use `sql.Null*`) into domain models.
- `stacks.Result.Changed` is `false` when a sync re-translates the latest
  stored source manifest and `sha256(rendered)` matches the stored
  `rendered_hash` — no new revision recorded, no stack deployed.

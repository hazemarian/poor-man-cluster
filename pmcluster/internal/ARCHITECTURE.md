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

`internal/runners` owns **one ephemeral, org-registered Actions runner container per
selected WORKER node** (`github_runners_enabled`, off by default). The capacity model
is the node list: N selected workers = N runners (`pmc-<node>-org`).

**It runs per node, not as a swarm service.** There is no stack and no placement
constraint. `startPerNodeLoops` (called from `serve.go` *before* the leadership wait,
because a worker blocks there forever) starts the filtered docker socket and the
node's runner half:

- a **worker** cannot read cluster settings at all (Docker configs are a manager-only
  read), so it runs `runners.Agent` against the assignment it pulls from the leader;
- a **manager** keeps the store-based `Manager.Reconcile`, which applies the same
  rules to its own node and is refused as a runner host anyway (`github_runner_allow_leader`
  exists for a single-node cluster).

**The assignment channel** (`internal/runners/assignment.go`, served by
`internal/cli/assignment.go`) is the leader's answer to "what should THIS node run":

- `GET /api/github/runners/assignment` on the leader's **swarm data-path address,
  port 9092** — a listener of its own, never `0.0.0.0`, never a published host port.
- Authenticated by **mutual TLS with the swarm node certificate**
  (`internal/auth/swarmtls`): chain to the swarm CA, `O` = this swarm's ID, `CN` = a
  node ID in the live node list (that is the revocation path), not expired. The
  middleware is mounted **per route** on this listener and never around a mux; the
  listener carries three node-facing routes — this one, the node report
  (`/internal/api/cluster/report`) and the cluster snapshot
  (`/internal/api/cluster/snapshot`) — and none of them is mounted on `/api/*`, on
  `/metrics`, or on the console, so a compromised worker gains no second readable
  endpoint. `?node=` is ignored: identity comes from the certificate only, and each
  route answers the projection that certificate entitles it to.
- The leader resolves the same decision the reconcile makes (switch, org, node list,
  manager/leader refusal, `selected` group visibility) and sends the **resolved
  spec**: org, group, socket path/proxy, memory, labels, poll interval, a `reason`,
  an operator `held` flag, and one single-use, one-hour **registration token**. No
  App key, no PAT, no setting, no secret. A refusal is a 409 with the reason.
- The **hold** crosses here because the console's Kill runs on the leader, which can
  only remove containers on its own node. The hold is persisted in the node's
  bookkeeping and sent in every answer; a held node removes its container and stays
  quiet until Restart clears it.

**The container** (`container.go`) comes from the pinned `DefaultImage`
(`myoung34/github-runner:2.338.2-ubuntu-jammy`, a constant rather than a setting),
with `RUNNER_SCOPE=org` — without it the image validates itself as a *repository*
runner, finds no `REPO_URL` and exits 1 with "Invalid configuration provided for
url" — plus `ORG_NAME`, `RUNNER_GROUP`, `RUNNER_NAME`, `LABELS=self-hosted,linux,x64,pmcluster`,
`EPHEMERAL=true`, a memory cap (default `2560m`; `1500m` was OOM-killed mid-job) and
`--restart no`. Desired-vs-actual is discovered by container **name** and the labels
`pmcluster.runner`, `pmcluster.runner.org`, `pmcluster.runner.name`, so the platform
only ever touches containers it owns.

**Bookkeeping** is the settings row `github_runners_state` (JSON, keyed by runner
name): registration time (the recycle cooldown), consecutive failures, and the
operator holds. It is what teardown needs once a container is gone, and it is why a
failed de-registration is retried instead of forgotten. The registration token is
never persisted.

**The security model is the docker socket.** The runner bind-mounts the daemon's
filtered socket directory (`internal/socketproxy`, `github_runner_socket_path`,
deny-by-default allow table) instead of `/var/run/docker.sock`; the directory rather
than the file, because the daemon replaces the socket on every restart. The image's
pre-created `docker-container` buildx builder is neutralised with
`BUILDX_CONFIG=/tmp/pmcluster-buildx` so builds use the daemon's own BuildKit;
`github_runner_socket_proxy=false` is the documented debug fallback and gives the
runner root on its node. Hence: workers only, `selected` group visibility only, off
by default.

`doctor`'s `github runners` check reports the enabled state, per-node
container/registration agreement (ghosts, offline runners, raw sockets), and the
missing `organization_self_hosted_runners: write` permission that would otherwise
appear as a bare 403. On a worker it answers the node-local subset (daemon, socket
proxy, last assignment fetched, local runner, disk) instead of refusing.

## Cluster state: one writer, readers that pull

`internal/clustersnapshot` serves `GET /internal/api/cluster/snapshot` — the state a
follower manager mirrors so it can answer reads from its own store instead of
"standing by". The decision, its alternatives and its costs are in
`docs/adr/0003-cluster-state-by-pull.md`; what the code constrains:

- **The version is a content identity, not a clock.** One `cluster_version` row is bumped
  inside the same transaction as every settings/secret/user write that changes
  something, and stamped at that moment by the store — never by the handler at read
  time, so two reads of one version report one `generated_at` and the age a follower
  shows is the age of the data. Rewriting a value that is already stored does not bump:
  reconcile reapplies settings every pass, and a bump per pass would make every poll a
  full body.
- **The ETag is `"<leader>:<version>"`.** A version alone is meaningless across a
  leadership change, where a different store can answer the same number for different
  content. A client must remember the PAIR it applied.
- **A projection, to managers only.** `settings`, `secrets` (ciphertext, never plaintext)
  and `users` — the API-key table — always present as arrays, so an empty table is `[]`
  and never `null`: a follower has to be able to tell "no rows" from "not sent", and the
  difference is every deleted row. `last_used_at`, logs, backup history, the CI board
  cache, metrics and per-node observations are deliberately absent. A worker certificate
  is refused: it has no store to mirror into and is told what to do through the
  assignment.
- **`cluster_version` rides the control-plane kit** with `store.CopyReplace`, because the
  migrations seed the row: a promoted leader then answers the version its adopted data
  actually has, rather than restarting at 1.

**Not built yet (slices 2–5 of the ADR):** no follower polls the route, settings still
travel in Docker configs, and the join-time key route does not exist. Slice 1 is additive
on purpose.

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

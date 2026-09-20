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
| `cluster` | — (engine) | `Service` (Up/Update/Down/Status) · `CredentialsService` | `GET /api/cluster/settings`, `PUT /api/cluster/settings`, `GET /api/usage` |

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

## Leaf packages

`internal/refs` is a leaf package (no domain port, no adapter). It provides
the shared `config()/secrets()` reference language consumed by
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

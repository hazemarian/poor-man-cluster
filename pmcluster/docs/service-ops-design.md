# Service operations design (drop Portainer phase)

Portainer's remaining value in this cluster is a set of hands-on service
operations that pmcluster has no replacement for: replica health / crash
history, log tailing, restart, inspect, and exec. This document specifies
the whitelisted replacement so Portainer can be removed from the infra
stack entirely.

**Security invariant: no raw `docker <anything>` passthrough.** Arbitrary
Docker API access is root-on-host. Every new endpoint maps to a fixed,
code-reviewed Docker SDK call behind the existing Bearer auth and edge rate
limiter. Nothing user-controlled ever reaches the daemon except a service
name, a stack name, an integer tail count, and an argv slice for `exec`.

**Status: implemented** (service-ops v0.2.43). The `services` domain, HTTP
routes, remote adapter, CLI group, and console Services tab are all live and
unit-tested; the `infra` stack no longer ships Portainer. Interactive
`docker exec -it` and follow-mode log streaming remain out of scope (SSH is
the path) — see §7.

---

## 1. What stays, what gets removed

- **Keep:** OpenObserve (log/metric search), OTel Collector, pmcluster
  deploy/rollback/delete, console UI/RBAC, TLS/backups/webhooks/API keys,
  registry/secret/config management.
- **Remove from `infra` stack:** `portainer/portainer-ce:2.39.5` service,
  its `portainer_admin_password` bootstrap secret, and any portainer routes
  in the edge proxy.
- **Add:** new `services` domain (below) covering ps / logs / restart /
  inspect / exec as **whitelisted** ops.

## 2. New domain: `internal/services`

Follows the established bounded-context pattern (model + port + local
adapter + http handler + remote adapter), mirroring `stacks`:

```
internal/services/
  services.go      // models + ports (ServiceSummary, TaskRun, port interfaces)
  local.go         // local adapter over docker.Client (+ fake in local_test.go)
  http.go          // chi handlers mounted under /api/services
  http_test.go
  local_test.go    // fake docker.Client drives each whitelisted op
```

### Models

```go
// ServiceSummary is one row of `pmcluster service ps` — replica health.
type ServiceSummary struct {
    Name      string  // swarm service name (stack_service)
    Stack     string  // derived from com.docker.stack.namespace label
    Replicas  uint64  // running
    Desired   uint64
    Image     string
    Mode      string  // "replicated" | "global"
    UpdatedAt int64
}

// TaskRun is one row of `docker service ps` — crash/restart history.
type TaskRun struct {
    TaskID   string
    Node     string
    Slot     int64
    State    string  // running | failed | shutdown | rejected
    Error    string  // non-empty when State != running
    StartedAt int64
    FinishedAt int64
}

// ExecResult is the buffered outcome of a non-interactive exec.
type ExecResult struct {
    ExitCode int
    Stdout   string
    Stderr   string
}
```

### Ports (interfaces in `services.go`)

```go
// Reader covers the read-only surface.
type Reader interface {
    // List returns every swarm service (pmcluster-managed or not).
    List(ctx context.Context) ([]ServiceSummary, error)
    // Tasks returns the task history for one service (crash/restart trail).
    Tasks(ctx context.Context, service string) ([]TaskRun, error)
}

// Ops covers the write surface; all operations are single, well-defined
// Docker SDK calls, never a free-form command string.
type Ops interface {
    // Restart forces a rolling restart (bumps ForceUpdate on the spec).
    Restart(ctx context.Context, service string) error
    // Exec runs a fixed argv in the first running task's container,
    // non-interactive. Fails with a clear error if no task is running.
    Exec(ctx context.Context, service string, argv []string) (*ExecResult, error)
}

// Logs tails a service's stdout/stderr; the full-text search story stays
// in OpenObserve (deep search is the OO console's job).
type Logs interface {
    Logs(ctx context.Context, service string, tail int) ([]string, error)
}
```

Implementation note: `internal/docker/client.go` gets the underlying SDK
calls — `ServiceList` gains image/mode/labels, plus new methods
`ServiceInspect` (for Restart's update-with-force), `ServiceTasks`,
`ServiceLogs`, and `ServiceExec`. The `services` local adapter composes
these behind the ports above so the fake in tests is small.

## 3. REST endpoints (mounted in `internal/server/server.go`)

All under `/api/services`, Bearer-auth'd by the existing middleware.
`Deps` gains `Services services.Service` (optional; routes omitted when nil).

| Method | Path | Body | Returns |
|---|---|---|---|
| GET  | `/api/services` | — | `{services: [ServiceSummary]}` |
| GET  | `/api/services/{stack}` | — | summaries filtered to that stack namespace |
| GET  | `/api/services/{stack}/{service}/tasks` | — | `{tasks: [TaskRun]}` |
| GET  | `/api/services/{stack}/{service}/logs?tail=200` | — | `{logs: [...]}` (tail 1..2000, default 200) |
| POST | `/api/services/{stack}/{service}/restart` | — | `{service, restarted: true}` |
| POST | `/api/services/{stack}/{service}/exec` | `{argv: [...]}` | `{exit_code, stdout, stderr}` |

Validation rules (fail fast, before touching Docker):

- `stack` must match `^[a-z0-9][a-z0-9_-]*$`.
- `service` is the **unqualified** name; the local adapter resolves
  `{stack}_{service}` and verifies it actually carries the
  `com.docker.stack.namespace={stack}` label before acting — prevents
  cross-stack targeting.
- `exec.argv` max 16 elements, each ≤ 200 bytes; empty argv → 400.
- `tail` clamps to `[1, 2000]`.

Rate limiting: these POSTs inherit the existing general per-IP limiter in
`server.go`. Exec/restart are inherently safe because the call graph is
fixed; the permissive burst is fine.

## 4. CLI commands (`internal/cli/service.go`, new file)

New `service` command group; every subcommand works **both locally and
remotely** via the existing `backendXxx` switchpoint pattern
(`internal/cli/backend.go` gains `backendServices` returning the local
adapter or `remote.NewServices(rc)`).

```
pmcluster service list [--stack NAME]            # all swarm services
pmcluster service ps STACK                       # replica health per stack
pmcluster service tasks STACK SERVICE            # crash/restart trail
pmcluster service logs STACK SERVICE [--tail 200]
pmcluster service restart STACK SERVICE
pmcluster service exec STACK SERVICE -- <argv...>
```

Notes:

- `service ps STACK` resolves services by stack namespace so the CLI and
  console agree on naming.
- `service exec` is strictly non-interactive (stdin not attached; output
  buffered and printed after exit). Interactive shell remains a
  documented SSH+`docker exec -it` path — a websocket feature is out of
  scope for this phase.
- `--api-url` / `PMCLUSTER_API_URL` remote mode works out of the box since
  the remote adapter is the same `remote.Client.do` used by every other
  data command.

## 5. Console buttons (`internal/ui`)

Extends the existing gin+HTMX pattern (`internal/ui/ui.go`, controllers,
`internal/ui/pmapi/client.go`).

- `pmapi/client.go`: `ListServices`, `ServiceTasks`, `ServiceLogs`,
  `ServiceRestart`, `ServiceExec` — thin wrappers over `Client.do`, with
  DTOs in `models.go`.
- `frag_stack.html`: add a **Services** card listing the stack's services
  with replica counts and a **Restart** button
  (`hx-post="/stacks/{name}/services/{service}/restart"`, `hx-confirm`).
- `frag_stacks.html`: add a "health" link from each stack row → services
  view; keep the list uncluttered.
- New controller `controllers/services.go` + routes in `ui.go`:
  - `GET /stacks/{name}/services` — health table (service, replicas,
    desired, image, restart button, "logs" link).
  - `GET /stacks/{name}/services/{service}/logs` — tail pane.
  - `POST /stacks/{name}/services/{service}/restart` — HTMX fragment re-render.
- All fragments re-render into `#view` exactly like the existing
  rollback/remove flows; zero JS beyond HTMX attributes.

## 6. Phase order (each lands green + unit tested)

1. **docker client additions** — extend `ServiceList`, add
   ServiceInspect/ServiceTasks/ServiceLogs/ServiceExec; fake-complete in
   `internal/docker` fakes used by tests.
2. **`internal/services`** local adapter + ports + unit tests (fake
   docker.Client): whitelist validation, cross-stack guard, exec non-TTY
   path, tail clamping.
3. **HTTP handlers** + `server.go` Deps wiring + route tests (same style as
   `configs_secrets_test.go`).
4. **remote adapter** `remote.NewServices` + `cli/service.go` + `backend.go`
   switchpoint; fast e2e additions to the existing suite.
5. **Console** — pmapi methods, controller, templates, ui_test fragments.
6. **Portainer removal** — drop the service + secret from the `infra`
   stack manifest, run `cluster update`, verify all remaining stacks still
   healthy. Update `ARCHITECTURE.md` + `docs/openapi.yaml`.

## 7. Explicit non-goals (kept out of this phase)

- Interactive `docker exec -it` over HTTP (needs websocket; SSH remains the
  path).
- Volume / image / raw-network browsing UI (read-only `docker volume ls`
  etc. can come later behind the same whitelisted pattern if requested).
- Follow-mode log streaming (tail-to-file works; streaming is an SSE/WS
  feature).
- Any free-form Docker API passthrough. The `services` domain is the only
  new Docker surface and every call is enumerated above.
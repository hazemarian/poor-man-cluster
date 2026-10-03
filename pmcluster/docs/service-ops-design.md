# Service operations design (drop Portainer phase)

Portainer's remaining value in this cluster is a set of hands-on service
operations that pmcluster has no replacement for: replica health / crash
history, log tailing, restart, inspect, and exec. This document specifies
the whitelisted replacement so Portainer can be removed from the infra
stack entirely.

**Security invariant: no raw `docker <anything>` passthrough.** Arbitrary
Docker API access is root-on-host. Every endpoint maps to a fixed,
code-reviewed Docker SDK call behind the existing Bearer auth and edge rate
limiter. Nothing user-controlled ever reaches the daemon except a service
name, a stack name, an integer tail count, and an argv slice for `exec`.

**Status: implemented.** The `services` domain, HTTP routes, remote
adapter, CLI group, and console surface are live and unit-tested; the
`infra` stack no longer ships Portainer. Interactive `docker exec -it`
and follow-mode log streaming remain out of scope (SSH is the path) — see
§7.

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

Rate limiting: these POSTs are gated by the **edge proxy's** per-IP
limiter (`internal/edgeproxy` — 200 req/s on `/api/*`, 40/s on
`/webhook/*`, with auto-ban), not by anything in the daemon's
`server.go`. The daemon on `127.0.0.1:9090` is deliberately unthrottled
(host-local by design). Exec/restart are inherently safe because the call
graph is fixed; the permissive burst is fine.

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
  buffered and printed after exit).
- **Interactive shell ships via websocket (v0.2.141)** — see §8. The
  documented SSH+`docker exec -it` path remains for out-of-band access.
- `--api-url` / `PMCLUSTER_API_URL` remote mode works out of the box since
  the remote adapter is the same `remote.Client.do` used by every other
  data command.

## 5. Console surface (`internal/ui`)

The operator console (`pmcluster-edge`, served at
`https://pmcluster.<domain>/web/`) exposes the service operations through
the gin+HTMX SPA.

- **Stack detail page** — each stack has a standalone page at
  `GET /web/stacks/{name}` (`internal/ui/controllers/stacks.go`). Its
  **Services in this stack** panel lists the stack's swarm services with
  replica health (pills: running / degraded / inactive), image (digest
  trimmed via `shortImage`, full ref on hover), mode, and updated time.
  Per service: **Tasks**, **Logs**, and **Restart** buttons.
- **Service detail** — `GET /web/services/{stack}/{service}/tasks|logs`
  (tabs), `POST /web/services/{stack}/{service}/restart|exec` (exec argv
  form). Logs poll every 3s into a dedicated `frag_servicelogs.html`
  fragment (`?tail=200&fragment=logs`) so the panel refreshes without
  re-rendering the page.
- **pmapi client** (`internal/ui/pmapi/client.go`): `ListServices`,
  `ServiceTasks`, `ServiceLogs`, `RestartService`, `ExecService` — thin
  wrappers over `Client.do`, DTOs in `models.go`.
- All actions are HTMX fragments re-rendered into `#view`; zero JS beyond
  HTMX attributes. All strings go through the i18n dictionaries
  (`internal/ui/views/i18n`, see `docs/console-i18n-contract.md`).

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

## 8. Interactive exec over websocket (shipped v0.2.141)

The console provides a full terminal into any service container — no SSH
needed. A browser terminal (`xterm.js`, vendored into
`internal/ui/views/static/`) talks to a websocket on the console, which
relays frames to a websocket on the pmcluster daemon; the daemon runs
`docker exec -it` against the service's running task with a real TTY.

**Topology:**

```
browser ──ws──▶ gin console /web/stacks/{name}/terminal/ws?service=X
  (session cookie, operator role)         │ (server-side Bearer token)
                                          ▼
                 daemon /api/services/{stack}/{service}/exec/ws
                                          │
                                          ▼
                 docker ContainerExecCreate(Tty)+ExecAttach hijack
```

The browser never sees the API token — the console dials the daemon with
its stored `PMAPIToken` in the `Authorization` header.

**Daemon side** (`internal/server/execws.go`):
- Route `GET /api/services/{stack}/{service}/exec/ws`, auth via the same
  `auth.Bearer` + `stackScopeGuard` as the rest of `/api`.
- The handler is dispatched **outside** the `otelhttp` / status-span
  wrappers (neither implements `http.Hijacker`; gorilla needs it to
  upgrade).
- Resolves the service to its running task's container, creates an
  interactive TTY exec (`runtime.ExecStream`), then pumps frames:
  - client→server **binary** frames = stdin bytes;
  - client→server **text** frames = `{"type":"resize","rows":N,"cols":N}`;
  - server→client **binary** = raw TTY output;
  - on exec exit the daemon sends `{"type":"exit","code":N}` then closes.
- `?cmd=` query picks the program (default `sh`); `?rows=`/`?cols=` seed
  the initial terminal size (default 24×80).

**Console side** (`internal/ui/controllers/terminal.go`):
- `GET /web/stacks/{name}/terminal?service=X` — standalone page (operator
  role) rendering `terminal.html` with xterm.
- `GET /web/stacks/{name}/terminal/ws?service=X` — upgrades the browser
  connection and relays frames verbatim to the daemon websocket (ws/wss
  from the API URL scheme).
- Each service row (stack detail + services page) gains a **Terminal**
  button linking to the page.

The CLI keeps the non-interactive `service exec` (stdout after exit); an
interactive `-it` CLI flag can ride the same endpoint later.

## 7. Explicit non-goals (kept out of this phase)

- ~~Interactive `docker exec -it` over HTTP (needs websocket; SSH remains the
  path).~~ **Shipped v0.2.141** — see §8.
- Volume / image / raw-network browsing UI (read-only `docker volume ls`
  etc. can come later behind the same whitelisted pattern if requested).
- Follow-mode log streaming (tail-to-file works; streaming is an SSE/WS
  feature).
- Any free-form Docker API passthrough. The `services` domain is the only
  new Docker surface and every call is enumerated above.
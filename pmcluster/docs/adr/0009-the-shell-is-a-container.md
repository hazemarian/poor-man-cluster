# ADR 0009 — the terminal runs in a throwaway shell, not in the service's container

**Status:** accepted (2026-11-17) · amends the interactive-terminal design in ADR 0004's L3 section

## Context

The console's interactive terminal exec'd into the service's running task. That works only when the
task shares a node with the daemon serving the console — and the console is served by the **leader**.
Measured on production: `aarma_db` ran on `nxt-sw-1-m` while the console talked to `nxt-sw-5-m`, so
every session died with

```
exec create: Error response from daemon: No such container: bea9aa4217bf…
```

…reported to the operator as `Connection closed. (1000: session ended)`. The `service exec` CLI has
the same limit, and its help text already admitted it: *"the first running task reachable from the
daemon node"*.

Two designs were considered and rejected:

- **Proxy to the node that holds the container.** Managers serve an mTLS listener on `:9092`, so a
  manager→manager proxy is possible — but workers serve **nothing** (measured: `nxt-sw-6-w` listens on
  `127.0.0.1:9090` only), so the worker case would need a multiplexed reverse channel: stream ids, a
  live-connection registry, keepalive, reconnect. And `swarm-e2e` runs a **single-node** swarm, so
  none of it could be tested before it reached production.
- **Docker's engine TLS port 2376.** Not listening on any node in this fleet, so it is not a
  transport this platform can rely on.

## Decision

**Create a throwaway container, on the node that serves the console, and exec into that.**

`POST /api/stacks/{stack}/shell` creates a one-replica service with:

- placement `node.hostname==<this node>` — the exec stays a local `docker exec`, which is the one
  thing that must hold;
- every network the stack's services are attached to (`StackNetworks`), so it reaches each service by
  name (`db`, `web`, …) — a stack network is created by swarm **without** `attachable`, so a
  `docker run` cannot join it; a service can;
- **all of the stack's secrets and configs**, mounted at `/run/secrets/<name>` (mode 0400) and
  `/run/configs/<name>` (0444), resolved from the control-plane store;
- a `sleep` command equal to the session ceiling.

The socket is `/api/shell/{serviceID}/exec/ws`, servable only to a caller holding the platform token;
the console reaches it through `/web/shells/{id}/ws` with a two-minute ticket, exactly as the
service terminal does.

**Lifecycle — nothing outlives its socket.** The operator's rule: no standing shells, and a ceiling.

- the daemon removes the service when the socket closes, whatever closed it;
- the container's own command is `sleep 1800`, so a crashed console cannot leave one behind;
- `pmcluster.shell=true` labels every session, with `pmcluster.shell.stack` and
  `pmcluster.shell.created`, so a leftover is identifiable and sweepable;
- every five minutes the daemon collects sessions whose `created` stamp is older than the ceiling.
  Without that, a console that dies without a close frame leaves a service whose task ends at 30
  minutes and is then **restarted by the orchestrator, forever** — the ceiling bounds the container,
  not the service.

**The image is `ghcr.io/hazemarian/pmcluster-shell:<version>`** (built by the release workflow from
`Dockerfile.shell`): Alpine with **both** client stacks — PostgreSQL (`psql`, `pg_dump`,
`pg_restore`) and MySQL/MariaDB (`mysql`, `mysqldump`, `mariadb-dump`) — plus curl, jq and less. A
shell that cannot open the database whose credentials it has mounted answers the wrong question.
`PMCLUSTER_SHELL_IMAGE` overrides the image for a fleet; `?image=` for one session.

## Consequences

- A terminal now works for **any** stack regardless of where its containers run, because the shell is
  scheduled onto the console's node rather than hunted for.
- **Mounting every secret of a stack is more power than "exec into `db`".** Anyone who can open the
  shell can read all of that stack's credentials. That is accepted because the route is operator-only
  and every session is logged with its stack, node and image — but it is the sharpest edge in this
  design, and it is why the session is destroyed with its socket rather than left running.
- Creating a container is a privileged act on the daemon. It is behind the same bearer + stack-scope
  chain as every other write, and the shell service carries labels that make it distinguishable from
  a workload at a glance.
- One new image to publish per release. The release workflow builds it on the tag, from the same
  commit as the binary, so a release's shell is the shell that release was tested with.
- The per-service terminal stays: for a container that *does* live on the console's node it is one
  hop cheaper, and it is the path the smoke tests exercise.

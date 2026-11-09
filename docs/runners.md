# Self-hosted GitHub Actions runners

When the org's GitHub-hosted Actions minutes run out, CI stops. This feature runs
**one ephemeral runner container per selected worker**, registered at
**organisation level** into a GitHub **runner group**. Nothing about it is manual:
turning the setting on registers and starts the runners, turning it off removes the
containers **and** de-registers the runners.

It needs the [GitHub App](github-app.md) — see [Why the App, not a
PAT](#why-the-app-not-a-pat) below.

## Settings

| Key | Default | What it does |
|---|---|---|
| `github_runners_enabled` | `false` | master switch. **Off by default.** Enabling is refused without an App, without an org, or with no node selected |
| `github_runner_org` | unset | the organisation the runner registers into |
| `github_runner_group` | `pmcluster` | the org runner group the runner joins |
| `github_runner_nodes` | unset | comma-separated **worker** hostnames that may run a runner — one container each. Managers are refused |
| `github_runner_socket_proxy` | `true` | route the runner's docker access through the daemon's filtered socket |
| `github_runner_socket_path` | `/run/pmcluster/docker.sock` | where that socket listens on every node |
| `github_runner_memory` | `2560m` | per-container memory limit (a Docker quantity: `2560m`, `3g`). `2560m` completed a `govulncheck` run on real hardware; at `1500m` the runner was OOM-killed mid-job |
| `github_runner_allow_leader` | `false` | allow a runner on a single-node cluster, where the only node is the leader |

```bash
pmcluster cluster settings set \
  github_runners_enabled=true \
  github_runner_org=acme \
  github_runner_group=pmcluster \
  github_runner_nodes=worker-1,worker-2 \
  github_runner_memory=2560m
```

The reconcile loop picks the settings up on its next pass (60 s default). No
`cluster update` is needed: a runner is not a platform config.

## Org-level registration, and why the group is pinned to `selected`

A **repo**-level runner is registered for one repository and holds a registration
token scoped to it. An **org**-level runner registers once
(`POST /orgs/{org}/actions/runners/registration-token`) and can take work from
every repository the group it joins allows. That is the shape the operator's
hand-made runner already had, and it means a new repository does not need a new
runner.

The group is the security boundary:

- The platform **creates** the group with `visibility: selected` and the org's
  private repositories as its members. A group with no repositories takes no jobs,
  and a public repository is never a member — its fork pull requests would run
  attacker-controlled workflow code on the runner.
- An **existing** group whose visibility is anything else is **refused**, by name,
  with the reason: an `all`-visible group lets workflow code from ANY repository in
  the organisation — including one an attacker can push to — run on the runner.
  The refusal is checked on every reconcile pass, not only at registration, so a
  visibility flipped in the GitHub UI stops the runner.

## Runners are per node, not a swarm service

A runner is a **local docker container** on the worker it is selected for. There is
no swarm service and no placement constraint: the daemon on node X runs *its* runner,
and a node that is not in `github_runner_nodes` removes any runner container it still
has and de-registers the name it owns.

Why not a service: a swarm service is one placement decision away from a build on a
manager, and a service on node A would have to reach the docker socket on node B.

Consequences worth knowing:

- **Capacity is the list.** N selected nodes = N runners, each capped at
  `github_runner_memory`. The console's node picker shows each node's free memory
  next to the checkbox so the cost is visible before it is committed.
- **Managers are refused.** The manager is the control plane (1 vCPU). A build
  there marks the node `Down` and takes the cluster API with it — this happened on
  the live cluster with the telemetry loop, which is why the refusal names the
  consequence instead of saying "not allowed". The picker shows managers greyed out
  with that reason rather than hiding them.
- **The leader is a manager**, so it is refused too. `github_runner_allow_leader`
  exists only for a single-node cluster, where there is nothing else to select.

## The docker socket: what a runner may do

A runner has to build images, which means talking to a docker daemon. The host's
`/var/run/docker.sock` is root on that node **and**, on a manager, the whole swarm.
Mounting it into a workflow means `docker service rm`, `docker node demote`,
`docker secret inspect` and `docker exec` are one command away.

So the daemon listens on a **filtered socket** and the runner gets that instead:

- `internal/socketproxy` runs inside the pmcluster daemon, on **every node with a
  docker socket**, unconditionally. It is inert when nothing mounts it, costs one
  goroutine, and starts and stops with the daemon.
- It listens on a unix socket (`github_runner_socket_path`, default
  `/run/pmcluster/docker.sock`, root:root 0660) and forwards to the real socket.
  No host port, no overlay exposure.
- The runner mounts that socket and gets
  `DOCKER_HOST=unix:///run/pmcluster/docker.sock`. It never sees
  `/var/run/docker.sock`.

The allow list is deny-by-default and segment-exact — a route that is not in the
table is refused, and there is no generic passthrough:

| Allowed | |
|---|---|
| `GET` | `/_ping`, `/version`, `/info`, `/events`, `/containers/json`, `/images/json`, `/volumes`, `/volumes/{name}`, `/networks`, `/networks/{id}`, `/containers/{id}/json`, `/containers/{id}/logs`, `/images/{id}/json`, `/images/**/json` |
| `POST` | `/containers/create`, `/containers/{id}/start`, `/stop`, `/restart`, `/kill`, `/wait`, `/images/create`, `/build`, `/volumes/create`, `/networks/create` |
| `DELETE` | `/containers/{id}`, `/images/{id}`, `/images/**`, `/volumes/{name}`, `/networks/{id}` |

Blocked outright, each one tested: `/services`, `/tasks`, `/nodes`, `/swarm`,
`/secrets`, `/configs`, `/plugins`, `/system`, `/session`, `/auth`, `/exec`, and
`exec` inside a container (`/containers/{id}/exec*`). A workflow cannot remove a
service, demote a node, read a swarm secret, or shell into another container —
those are impossible rather than discouraged.

Every refusal is a `403` naming the route, and the daemon logs the attempt:

```bash
journalctl -u pmcluster | grep "socket proxy: denied"
# WRN socket proxy: denied a workflow's docker request method=POST path=/v1.43/services/pmcluster_edge
#     family=/services reason="swarm services: `service rm` would delete the platform's own workloads"
```

The log is rate-limited (the first few per minute one by one, then one summary line
with the suppressed count) so a retry loop cannot flood the journal.

**This is blast-radius control, not a security boundary.** The socket still fronts
a root daemon, and the routes that are allowed (create a container, pull an image,
bind-mount a host path) are already enough to escalate on that node. Two rules
follow: a runner on a **manager** defeats the whole exercise, and a workflow from a
repository you do not trust should not run on a runner at all.

`github_runner_socket_proxy=false` mounts the raw socket instead. It exists to debug
a build against something the filter does not cover (an unfamiliar buildx flow, a
daemon version the table does not match). It is unsupported for untrusted code, and
`pmcluster doctor` warns when it is on.

### buildx

`docker buildx build` with the **docker driver** works through `DOCKER_HOST`: it
talks to the daemon's `/build` API, which the filter allows. A containerised
`docker-container` builder needs the socket inside the builder container plus
`--privileged`, which this feature deliberately does not arrange — the filter would
be bypassed by the nested daemon. If a workflow needs that driver, it needs a
self-hosted runner the operator runs and trusts, not this one.

## Lifecycle

1. **Enable.** The settings surface refuses an enabled feature that cannot work:
   no App, no org, or no selected node. The reconcile additionally refuses an
   `all`-visible group and any selected manager.
2. **Register.** The node's daemon mints an org registration token with the
   installation token and starts a local container with `ORG_NAME`, `RUNNER_GROUP`,
   `EPHEMERAL=true`, labels `self-hosted,linux,x64,pmcluster`, the memory limit and
   the filtered socket. The token is single-use, lives one hour, and is **never
   persisted**.
3. **Recycle.** The runner takes **one job**, then exits. The next pass mints a
   fresh token and starts a fresh container. A container that is still starting is
   left alone for a two-minute cooldown.
4. **Disable.** `github_runners_enabled=false` removes every runner container the
   platform owns (discovered by its `pmcluster.runner` label) and de-registers each
   name at GitHub. Rows disappear from the panel. It is idempotent: a second pass
   with nothing left makes no calls. If GitHub is unreachable the container still
   comes off the node and the warning says the de-registration is **unconfirmed** —
   the bookkeeping entry survives and the next pass retries it.
5. **Remove a node** from `github_runner_nodes` and that node stops and de-registers
   exactly its own runner; the others are untouched. Add one and it starts on the
   next pass.

The rendered container is deliberately boring: `--restart no` (an ephemeral runner
must not be restarted onto a spent token), a memory cap, the filtered socket, and no
host networking.

## The console

`/web/runners` (linked from Settings) has the node picker and the runner table:

- **Picker** — one row per node: hostname, role badge, current runner state, and
  the node's available memory. Managers are greyed out with the reason on hover.
  Saving writes `github_runner_nodes` in the page's order.
- **Table** — scope (org), name, node, state pill (`idle`/`busy`/`starting`/
  `offline`/`failed`), mode, age, last job.
- **Kill** per row removes the container and the registration, and holds the runner
  so no pass re-registers it. On a `busy` runner the button asks first, in words:
  killing it removes the container mid-workflow and fails that job.
- **Restart** re-registers the runner immediately; **Restart stopped** does that for
  every stopped row.

The same actions are on the API (`POST /api/github/runners/{name}/kill|restart`,
operator role), so a kill from curl and a kill from the panel are one code path.
A kill on a runner that lives on another node cannot remove that container from
here; it de-registers it and holds the name, and the owning node stops the container
on its next pass.

## Why the App, not a PAT

A registration token must be minted with `administration: write` on the
**organisation**. The App already mints one-hour installation tokens for clone,
build and the [token broker](github-app.md); using the same installation for runner
registration means no long-lived credential exists anywhere in the feature. A
personal access token could do the same job only by holding `administration: write`
on the whole org — a wider, longer-lived credential — so it is refused by name rather
than accepted as a quieter substitute.

The permission this feature adds is **Organization → Administration: Read & write**.
Grant it in the App's settings, then approve it on the installation; until then
registration returns a bare `403` and `pmcluster doctor` names the permission.

Reading the "last job" column additionally needs **Actions: read**; without it the
column stays empty and nothing else is affected.

## Failure handling

| What happened | What the platform does |
|---|---|
| registration token refused (`403`) | warning naming `administration:write` and the org level; the failure count is recorded for the doctor |
| the group is visible to the whole org | refusal naming the docker risk; no container is started |
| a selected node is a manager | refusal naming the node and why (a build marks it Down) |
| GitHub is unreachable | teardown still removes the container and reports the de-registration as unconfirmed; a registering node warns and retries next pass |
| the container exits after a job | recycled on a later pass with a fresh token |
| the socket proxy cannot bind | a WARN plus an apply warning; the node does not register a runner (a runner without its socket would fail every job) |

None of these crash the daemon, and none of them leave a container the platform
does not know about.

## Doctor

`pmcluster doctor` reports a `github runners` check, ENABLED/DISABLED first:

- **disabled** → `ok`; if anything is left behind (a container, a bookkeeping entry,
  a row) → `warn` naming `github_runners_enabled`, because leftovers are the failure
  mode of a half-disable;
- **enabled with no App, no org, or no node** → `fail`, naming the key;
- **App missing `administration:write`** → `fail`, naming the exact permission;
- **per selected node** → `warn` when registered-but-not-running, running-but-not-
  registered, offline, failing repeatedly, on a manager, or mounting the raw socket;
- **unmanaged runners** → a pmc-named runner GitHub knows that the platform does not
  manage (the operator's hand-made `pmc-org-runner`, or a leftover from a node that
  left the list) is named, reported as not covered by the socket proxy, and **never
  counted as healthy**.

## What is not built

- The runner image is `myoung34/github-runner:latest`, a constant rather than a
  setting: an arbitrary image here would run with access to a docker daemon.
- One runner per node; a node cannot run two, and a runner cannot be pinned to a
  block device or a cgroup beyond the memory cap.
- The socket proxy's allow table is code, not a setting. Widening it is a security
  decision that belongs in review.
- Rotation of the runner group's repository selection happens on every registration
  (adds and removes), but a repository added to the org while a runner is idle is
  picked up at that runner's next registration, not immediately.
- Only one page of 100 private repositories is selected. An org with more keeps the
  first hundred and the doctor's group warning says so.

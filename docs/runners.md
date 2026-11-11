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
| `github_runner_memory` | `2560m` | per-container memory limit (a Docker quantity: `2560m`, `3g`). `2560m` completed a `govulncheck` run on real hardware; at `1500m` the runner was OOM-killed mid-job. A cap above **70 %** of the target node's RAM is **refused** — see [A runner that cannot fit the node](#a-runner-that-cannot-fit-the-node) |
| `github_runner_allow_leader` | `false` | allow a runner on a single-node cluster, where the only node is the leader |
| `github_runner_assignment_interval` | `60` | seconds between a node's polls of the leader for its runner assignment. The settings surface **refuses** anything outside 10–3600 (a stored value that no longer parses is read as the default, and a stored number outside the range is clamped, so a bad row cannot turn the poll off or hammer the leader); below ten would spend the leader's GitHub quota, above an hour leaves an assignment stale. The leader sends the value inside every answer, because a worker cannot read the setting itself |

```bash
pmcluster cluster settings set \
  github_runners_enabled=true \
  github_runner_org=acme \
  github_runner_group=pmcluster \
  github_runner_nodes=worker-1,worker-2 \
  github_runner_memory=2560m
```

The same values are accepted from a fresh install, so a cluster can be brought up
with CI already wired instead of remembering a later `settings set`:

```bash
pmcluster setup \
  --github-app-id 123456 --github-installation-id 7890 --github-app-key-file ./app.pem \
  --github-runners-enabled --github-runner-org acme --github-runner-nodes worker-1,worker-2
```

The remaining runner flags are `--github-runner-group`,
`--github-runner-socket-proxy`, `--github-runner-socket-path`,
`--github-runner-memory`, `--github-runner-allow-leader` and
`--github-runner-assignment-interval`. Setup **refuses** to enable runners
without a stored App, an org, and at least one worker hostname, and its error
names what is missing and how to supply it. The App flags may be passed in the
**same** invocation: the App is stored before the runner gate is evaluated, so one
command brings up a cluster with CI ready.

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

## How a worker learns what to run: the assignment channel

A worker **cannot read the runner settings at all**. They live in Swarm configs, and
reading a config is a manager-only Docker call — on a worker `docker config inspect`
fails with *"This node is not a swarm manager"*. That single fact is why the first
version of this feature was inert: each daemon was supposed to read
`github_runner_nodes` for itself, and the only nodes allowed to host a runner are the
ones that cannot read it.

So the decision is made once, on the leader, and **pulled** by each node:

```
leader daemon ── resolves the runner spec from cluster settings ──┐
       ▲                                                          │
       │  GET /api/github/runners/assignment?node=<self>            │
       │  mutual TLS: the node's swarm certificate                  │
       │                                                          ▼
   worker daemon ── applies the answer locally (start / stop its runner container)
```

The leader applies the **same** rules the reconcile applies to its own node — feature
switch, org, node list, manager/leader refusal, `selected` group visibility — through
one function, so a node cannot get an answer the reconcile would refuse. The node
applies; it never decides.

The answer is the resolved spec, not a settings dump:

```json
{"node":"nxt-sw-6-w","enabled":true,"org":"acme","group":"pmcluster",
 "socket_proxy":true,"socket_path":"/run/pmcluster/docker.sock","memory":"2560m",
 "mode":"ephemeral","labels":"self-hosted,linux,x64,pmcluster",
 "interval_seconds":60,"reason":"selected as a runner node","held":false,
 "registration_token":"…","registration_token_expires_at":1700003600,
 "issued_at":1700000000}
```

`registration_token` is the one GitHub artifact on the channel: single-use, one hour,
org-scoped, and it exists because an ephemeral runner cannot be created without one.
Nothing else credential-shaped travels with it — no App key, no PAT, no cluster
setting, no secret.

`held` is the operator stop crossing the node boundary. The console's **Kill** runs
on the leader, which can only remove a container on its own node: a kill for a
runner living elsewhere used to be recorded on the leader and nowhere else, so the
owning node started it again seconds later and the button appeared to work while
changing nothing. The hold is persisted with the runner's bookkeeping and sent in
every answer for that node; a held node removes its container, mints no token and
stays quiet until **Restart** clears the hold.

### Transport: the leader's swarm address, not the console

A worker tries, in order, and logs the address that answered **once per boot**:

1. **A manager's swarm data-path address**, taken from the live swarm
   (`docker info` → `RemoteManagers`, e.g. `10.7.224.11`). This is the address Swarm
   itself uses between nodes: no DNS, no published port, no firewall change, and it
   is exactly the address the leader's assignment listener binds.
2. **`127.0.0.1:9092`**, the loopback address a published-forwards-to-this-host
   setup would give. It is second because nothing publishes it by default.

Two paths that look obvious and do not work, so nobody re-derives them:

- **The overlay service name** (`pmcluster-edge`, or the edge's service VIP) is not
  reachable from the daemon. The daemon is a host process, not a container: Docker's
  embedded DNS is per-container, and the host has no route into an overlay network.
  Measured on the live fleet: `getent hosts pmcluster-edge` on a worker returns
  nothing and a direct request to the edge task's overlay IP times out.
- **An ingress-published port cannot target this listener.** `mode: ingress` forwards
  the routing mesh to a *container task*; the assignment endpoint is served by the
  host daemon, and publishing a host process is `mode: host`, which this project
  refuses for the API. The console in front of the daemon is a plain HTTP proxy as
  well, so it cannot carry a client certificate either.

The listener binds the **node's swarm data-path address** (never `0.0.0.0`, never a
public host port). Port **9092** is on `harden-host.sh`'s `RESTRICTED_HOST_PORTS`
list: it is allowed from the private/docker subnets and **denied from the WAN**, the
same rule as 9090. A future change that widens the bind still cannot reach the
internet through an unhardened host.

An existing fleet must **re-run `harden-host.sh` on every node** to pick the rule up
(the script is idempotent). Until it runs, a host whose firewall still denies 9092 from
the private range will answer no assignments, and the worker's log says so: one bounded
WRN naming the address it could not reach.

### The credential: the certificate every node already has

No token is minted, copied or rotated for this. Docker already issued each node a
CA-signed certificate whose **organisation is the swarm ID** and whose **common name
is the node ID** (`/var/lib/docker/swarm/certificates/swarm-node.crt`). The leader
verifies, in this order, and refuses with a bare 403 (a short reason, never a hint
about which IDs would have been accepted):

1. the chain verifies against the swarm root CA (`/var/lib/docker/swarm/ca.pem`, or
   `/var/lib/docker/swarm/certificates/swarm-root-ca.crt` on Docker 28+, where the
   same certificate lives);
2. the certificate's `O` **is this cluster's swarm ID** — a node of another swarm has
   a valid chain from *its* CA, and the organisation is the second lock;
3. its `CN` is a node ID **in the live node list** — this is the revocation path:
   `docker node rm` removes the node, and its certificate stops being accepted at the
   next request. Nothing to rotate, nothing to remember;
4. it is not expired. Docker rotates node certificates; an expired one is a rejected
   one.

**Identity comes from the certificate only.** The handler ignores `?node=`: a
certificate for `nxt-sw-6-w` gets `nxt-sw-6-w`'s assignment whatever the query string
says. Ignoring (rather than refusing) keeps exactly one identity source and no branch
that could later be "helpfully" widened.

The honest trade: this is a port the daemon now opens where it opened none, and it
authenticates **another process on a cluster node** — root on that node can read that
node's certificate and ask for that node's assignment. That is the same node that
already runs the container, so it learns nothing it cannot already do. What an
attacker on the HOST of a node cannot do is ask for a *different* node's assignment,
read a setting, a secret or a credential: none of them are on this route, and the
route is the only thing this listener serves.

### Mounted once, on purpose

`internal/auth/swarmtls` is mounted on `GET /api/github/runners/assignment` **and on
nothing else** — not `/api/*`, not `/metrics`, not the console. A compromised worker
must not gain a second readable endpoint through its certificate. If a later change
mounts it elsewhere, that is a deliberate widening of the cluster's attack surface and
belongs in review.

### What this channel is not

Not a control plane, and not a step towards one:

- **no writes** — the node stops and removes its own container; de-registering a
  runner at GitHub stays on the leader, which is the only side holding the App;
- **no second endpoint** — one route, one answer, one connection;
- **no settings, secrets or credentials** — the response is the spec above;
- **no push** — the node pulls; there is no leader→worker connection to keep open;
- **no long-lived connection** — a request every `github_runner_assignment_interval`.

If a later change needs a second thing over this channel, it is a redesign, not an
extension.

### What a worker serves locally

A **standby** node's daemon — a worker, or a follower manager — binds its API on
**`127.0.0.1:9090`** and nothing else. It has no current cluster state to answer and a
worker does not open the store at all (an empty local database whose answers look
plausible is the failure the CLI's leader guard exists to prevent). `/health` answers,
which is how you can tell a standby daemon is alive; the leader keeps binding what it
always did (`0.0.0.0:9090` by default). When a standby is elected, it closes its
loopback listener before the leader listener binds the same port.

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
- The runner mounts that socket's **directory** (`/run/pmcluster`) and gets
  `DOCKER_HOST=unix:///run/pmcluster/docker.sock`. It never sees
  `/var/run/docker.sock`. The directory, not the file, because the daemon
  recreates the socket on every restart and a file bind-mount pins the old inode:
  a runner that was mid-job across an upgrade lost the daemon with `Cannot connect
  to the Docker daemon at unix:///run/pmcluster/docker.sock` while the socket was
  present on the host.

The allow list is deny-by-default and segment-exact — a route that is not in the
table is refused, and there is no generic passthrough:

| Allowed | | |
|---|---|---|
| `GET` / `HEAD` | `/_ping`, `/version`, `/info`, `/events`, `/containers/json`, `/images/json`, `/volumes`, `/volumes/{name}`, `/networks`, `/networks/{id}`, `/containers/{id}/json`, `/containers/{id}/logs`, `/images/{id}/json`, `/images/**/json` | the client's handshake and every read |
| `POST` | `/containers/create`, `/containers/{id}/start`, `/stop`, `/restart`, `/kill`, `/wait`, `/images/create`, `/build`, `/volumes/create`, `/networks/create` | a build and the `docker run` step around it |
| `POST` | `/auth`, `/images/{name}/tag`, `/images/**/tag`, `/images/{name}/push`, `/images/**/push` | `docker login`, retag, publish — bounded by the credential the job authenticated with |
| `POST` | `/session`, `/grpc` | BuildKit's session channel — how buildx attaches to the daemon's own BuildKit (see [buildx](#buildx)) |
| `DELETE` | `/containers/{id}`, `/images/{id}`, `/images/**`, `/volumes/{name}`, `/networks/{id}` | |

Blocked outright, each one tested: `/services`, `/tasks`, `/nodes`, `/swarm`,
`/secrets`, `/configs`, `/plugins`, `/system`, `/exec`, and `exec` inside a
container (`/containers/{id}/exec*`). A workflow cannot remove a service, demote a
node, read a swarm secret, or shell into another container — those are impossible
rather than discouraged. A path containing `.` or `..` is refused before any
pattern is consulted, because the `**` wildcards that match a registry-qualified
image name would otherwise match `x/../../services/…` too.

Every refusal is a `403` naming the route, and the daemon logs the attempt:

```bash
journalctl -u pmcluster | grep "socket proxy: denied"
# WRN socket proxy: denied a workflow's docker request method=POST path=/v1.43/services/pmcluster_edge
#     family=/services reason="swarm services: `service rm` would delete the platform's own workloads"
```

The log is rate-limited (the first few per minute one by one, then one summary line
with the suppressed count) so a retry loop cannot flood the journal.

**This is blast-radius control, not a security boundary.** The socket still fronts
a root daemon, and the routes that are allowed (create a container, pull or push an
image, bind-mount a host path) are already enough to escalate on that node. Two
rules follow: a runner on a **manager** defeats the whole exercise, and a workflow
from a repository you do not trust should not run on a runner at all. A push is
bounded by the credential the job supplied — a repository's `GITHUB_TOKEN` can only
write that repository's packages — which is why publishing is allowed at all.

`github_runner_socket_proxy=false` mounts the raw socket instead. It exists to debug
a build against something the filter does not cover (an unfamiliar buildx flow, a
daemon version the table does not match). It is unsupported for untrusted code, and
`pmcluster doctor` warns when it is on.

### buildx

The image this platform runs ships a **pre-created buildx builder named `default`
using the `docker-container` driver**, so a plain `docker build` delegates to a
BuildKit *container* — which the filtered socket can neither populate
(`PUT /containers/{id}/archive`) nor exec into. The result is a build that fails
inside buildx, with an error that names none of this.

So the container sets `BUILDX_CONFIG=/tmp/pmcluster-buildx` (an empty directory of
its own, `Spec.EnvFixups`), which neutralises that builder and makes the CLI fall
back to the daemon's **built-in** BuildKit through `DOCKER_HOST`. `POST /session`
and `POST /grpc` are on the allow list for the same reason: the docker driver
attaches a session to the daemon's BuildKit over them, and denying them does not
prevent a build — it silently forces the `docker-container` driver, which then dies
on the archive route above. `buildx create --driver docker` cannot repair it
either; that driver is a singleton.

What is still not arranged is a **containerised** builder: a `docker-container`
builder needs the docker socket inside the builder container plus `--privileged`,
which would bypass the filter by nesting a daemon. A workflow that creates one
explicitly will fail here; plain `docker build` and `docker buildx build` against
the daemon's own driver are the supported path.

## Lifecycle

1. **Enable.** The settings surface refuses an enabled feature that cannot work:
   no App, no org, or no selected node. The reconcile additionally refuses an
   `all`-visible group and any selected manager.
2. **Register.** The LEADER mints the org registration token (it is the only side
   that holds the App) and puts it in the node's assignment; the node starts a local
   container from the pinned image with `RUNNER_SCOPE=org`, `ORG_NAME`,
   `RUNNER_GROUP`, `EPHEMERAL=true`, labels
   `self-hosted,linux,x64,pmcluster`, the memory limit and the filtered socket. The
   token is single-use, lives one hour, and is **never persisted** — not on the
   node's disk, not in a log.
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
  so no pass re-registers it. The hold is **persisted on the leader and sent to the
  owning node in its next assignment** (`held: true`), so a kill for a runner on
  another node takes effect there within one poll instead of being recorded where it
  cannot act. On a `busy` runner the button asks first, in words: killing it removes
  the container mid-workflow and fails that job.
- **Restart** re-registers the runner immediately; **Restart stopped** does that for
  every stopped row.

The same actions are on the API (`POST /api/github/runners/{name}/kill|restart`,
operator role), so a kill from curl and a kill from the panel are one code path.
A kill on a runner that lives on another node cannot remove that container from
here; it de-registers it and holds the name, and the owning node stops the container
on its next pass.

## A runner that cannot fit the node

A memory cap larger than the machine is not a cap. Docker enforces `--memory` per
cgroup, so `2560m` on a node with 1839 MB of RAM is accepted, started, and then
the **host** runs out — because the cgroup's "limit" exceeds the resource it is
supposed to bound.

That is not hypothetical. On 2026-11-10:

| Node | RAM | Cap it was given | What happened |
|---|---|---|---|
| `nxt-sw-3-w` | 1839 MB | `2560m` (**139 %** of the host) | the first CI build OOM-killed the node; `sshd` died with it, so nothing could be restarted remotely |
| `nxt-sw-1-m` | — | — | OOM-killed in the same night; `Down` for hours while the cluster ran two of three managers |

So a runner whose cap exceeds **70 % of the node's total RAM** is **refused** —
not started, not registered, no registration token minted. The refusal names both
numbers and the fix:

```
refused: nxt-sw-3-w has 1839 MB; a 2560m runner can OOM this node and take it down
— lower github_runner_memory or pick a bigger node
```

The arithmetic, for the two sizes that matter here:

| Node RAM | Ceiling (70 %) | `2560m` | Verdict |
|---|---|---|---|
| 1839 MB | 1287 MB | 139 % of the node | **refused** |
| 3852 MB | 2696 MB | 66 % of the node | allowed |

**Why 70 % and not 60 %.** 60 % of 3852 MB is 2311 MB, which would refuse the
platform's own default (`2560m`) on a healthy 4 GB worker — a boundary that stops
CI everywhere is an outage, not a guard. 70 % refuses the case that killed the
node (139 %) and keeps the documented default working (66 %), and it is the same
ceiling the control plane gives itself (`MemoryMax`, see
[`health.md`](health.md#memory-boundaries-a-limit-larger-than-the-host-is-not-a-limit)).

**Where the check lives.** Wherever the decision is *resolved*, so it cannot be
outlived by a settings change:

- the **leader** applies it when it resolves a node's assignment, before it mints
  a registration token (`internal/runners/memory.go`, `CheckMemoryFit`) — and a
  node whose own store is current (single-node cluster, or the leader with
  `github_runner_allow_leader`) applies the same function in its reconcile, so
  the two paths cannot disagree;
- the **node** records the refusal it was sent, logs it once at WARN, and removes
  any container it still has;
- **`pmcluster doctor`** reports it on the leader (naming the node, its RAM and the
  cap) and on the node itself — a worker has no store, so it compares the cap in
  its applied assignment against its own `/proc/meminfo` (`runner memory` check);
- the **console** runner row stays `offline` and shows the reason under the state
  pill.

It is re-evaluated on **every pass**, which cuts both ways: raising
`github_runner_memory` refuses a node that was running, and lowering it releases
one on the next poll — no `cluster update`, no restart.

**Unknown RAM is not a refusal.** The leader reads a worker's RAM from the
node-stats rows (the swarm node list carries no memory), and a node whose
telemetry has not arrived — or a cluster where it was never enabled — has none. A
missing measurement allows the runner and logs **once** per node:

```
runners — node RAM unknown (no recent node-stats sample): allowing the runner, memory cap NOT checked
```

A sample older than an hour counts as unknown too: a node that stopped reporting
is not a node whose size is known. The alternative — refusing on missing data —
would turn a monitoring gap into a fleet-wide CI outage.

## Why the App, not a PAT

An **org** registration token is minted with `organization_self_hosted_runners: write`
on the **organisation**. The App already mints one-hour installation tokens for clone,
build and the [token broker](github-app.md); using the same installation for runner
registration means no long-lived credential exists anywhere in the feature. A
personal access token could only do the same job as a long-lived credential with
that scope on the whole org — or, for a repository-level token, with
`administration: write` on every repository — so it is refused by name rather than
accepted as a quieter substitute.

The permission this feature adds is **Organization → Self-hosted runners: Read &
write** — not Administration, which covers only the repository-level endpoints.
Grant it in the App's settings, then approve it on the installation; until then
registration returns a bare `403` and `pmcluster doctor` names the permission.
The exact endpoint-to-permission table is at the end of this chapter.

Reading the "last job" column additionally needs **Actions: read**; without it the
column stays empty and nothing else is affected.

## Failure handling

| What happened | What the platform does |
|---|---|
| registration token refused (`403`) | warning naming `organization_self_hosted_runners:write` at the organisation level; the failure count is recorded for the doctor |
| the group is visible to the whole org | refusal naming the docker risk; no container is started |
| a selected node is a manager | refusal naming the node and why (a build marks it Down) |
| GitHub is unreachable | teardown still removes the container and reports the de-registration as unconfirmed; a registering node warns and retries next pass |
| the container exits after a job | recycled on a later pass with a fresh token |
| the socket proxy cannot bind | a WARN plus an apply warning; the node does not register a runner (a runner without its socket would fail every job) |
| the leader cannot be reached for an assignment | the node keeps its last applied state (a running runner is not torn down) and logs one bounded WRN per five minutes, with the suppressed count |
| a worker holds no swarm certificate (not a swarm node) | one WRN per boot and no polling at all — there is no unauthenticated fallback, by design |
| a node is removed (`docker node rm`) | its certificate stops being a live node ID, so the leader refuses its assignment at the next poll (403) |
| the runner group visibility flips to `all` | the leader's answer becomes a refusal (409) on the next poll; the node keeps its last state and the log names the group |

None of these crash the daemon, and none of them leave a container the platform
does not know about.

## Doctor

On a **worker**, `pmcluster doctor` answers the node-local subset instead of
refusing (cluster state is only current on the leader, and the worker report does not
pretend otherwise):

```
✓ daemon             answering /health at http://127.0.0.1:9090/health
✓ socket proxy       listening on /run/pmcluster/docker.sock
✓ runner assignment  fetched 24s ago from https://10.7.224.11:9092: enabled=true, org=acme, runner=pmc-nxt-sw-6-w-org
✓ local runner       pmc-nxt-sw-6-w-org is running with the filtered socket
✓ disk               71.2% free (18.1 GiB of 25.4 GiB) on /var/stack/data
· swarm              this node is standing by: the node list and quorum are manager-only calls, so they are answered on the leader
· cluster state      not answered here: secrets, settings, stacks, the last apply and the control-plane kit are only current on the leader…
```

That report is the check that would have caught the whole defect in one line: before
it, a worker's daemon stood by without ever binding, so `daemon` failed with
"nothing listening" — and there was no way to ask.

On the **leader**, `pmcluster doctor` reports a `github runners` check,
ENABLED/DISABLED first:

- **disabled** → `ok`; if anything is left behind (a container, a bookkeeping entry,
  a row) → `warn` naming `github_runners_enabled`, because leftovers are the failure
  mode of a half-disable;
- **enabled with no App, no org, or no node** → `fail`, naming the key;
- **App missing `organization_self_hosted_runners:write`** → `fail`, naming the exact permission;
- **per selected node** → `warn` when registered-but-not-running, running-but-not-
  registered, offline, failing repeatedly, on a manager, or mounting the raw socket;
- **unmanaged runners** → a pmc-named runner GitHub knows that the platform does not
  manage (the operator's hand-made `pmc-org-runner`, or a leftover from a node that
  left the list) is named, reported as not covered by the socket proxy, and **never
  counted as healthy**.

## What is not built

- The runner image is pinned (`myoung34/github-runner:2.338.2-ubuntu-jammy`,
  `internal/runners.DefaultImage`) rather than a setting: an arbitrary image here
  would run with access to a docker daemon, and `latest` would let an upstream
  push change what runs on the host. The container is started with
  `RUNNER_SCOPE=org`; without it the image validates itself as a **repository**
  runner, finds no `REPO_URL` and exits 1 with *"Invalid configuration provided
  for url"* — a crash loop that names nothing relevant.
- One runner per node; a node cannot run two, and a runner cannot be pinned to a
  block device or a cgroup beyond the memory cap.
- The socket proxy's allow table is code, not a setting. Widening it is a security
  decision that belongs in review.
- Rotation of the runner group's repository selection happens on every registration
  (adds and removes), but a repository added to the org while a runner is idle is
  picked up at that runner's next registration, not immediately.
- Only one page of 100 private repositories is selected. An org with more keeps the
  first hundred and the doctor's group warning says so.
- The assignment channel is read-only and carries one route. A worker cannot push its
  state back through it, so the console's table is completed by GitHub's org runner
  list and by the leader's own view; a worker's local row lives on the worker and is
  what its own `pmcluster doctor` reads.
- There is no runner assignment for a manager. A manager is refused as a runner host,
  so its assignment would always be "off", and a second code path for the same
  decision is a second place for it to drift.

## App permissions: the exact one, because the obvious answer is wrong

Runner registration is an **organisation** operation, and GitHub splits the
permission by level:

| endpoint | permission | level |
|---|---|---|
| `POST /orgs/{org}/actions/runners/registration-token` | `organization_self_hosted_runners: write` | Organization |
| `GET|DELETE /orgs/{org}/actions/runners` | `organization_self_hosted_runners: write` | Organization |
| `GET|POST /orgs/{org}/actions/runner-groups` | `organization_self_hosted_runners: write` | Organization |
| `POST /repos/{owner}/{repo}/actions/runners/registration-token` | `administration: write` | Repository |

So **Organization → Self-hosted runners: Read & write** is what this feature
needs. `Administration` covers only the repository-level endpoints. An App can
hold `administration: write` *and* `organization_administration: write` and still
be refused at every organisation runner endpoint — that is not a hypothetical,
it is how this feature first failed on a live cluster, with the platform blaming
the permission that was already granted.

Three steps, all required, and skipping the third looks exactly like a bug:

1. App settings → Permissions → **Organization permissions → Self-hosted
   runners → Read & write** → Save.
2. Accept the new permissions on the installation
   (`github.com/organizations/<org>/settings/installations`). Until an org owner
   approves, the installation keeps its old scopes.
3. **Re-mint the installation token.** Tokens are cached for their lifetime (up
   to an hour), so a daemon that minted one before the approval keeps using the
   old scopes and keeps reporting the old failure. `systemctl restart pmcluster`
   on the leader forces a new mint; otherwise wait for expiry.

`pmcluster doctor` reports the missing permission by name before a registration
is ever attempted, because GitHub's own error does not: a 403 body is the bare
string `Resource not accessible by integration`, naming neither the permission
nor the fix. Where GitHub does supply it, the response carries
`X-Accepted-GitHub-Permissions` for the endpoint — that header, not a guess, is
the authority on what is missing.

## Troubleshooting: the failures this feature actually produced

Each of these cost real time on a live cluster. They are listed with the message
you see, because the message is what you will be staring at.

| what you see | what it means | fix |
|---|---|---|
| `github_runners_enabled is enabled but github_runner_org is empty` | the org is the registration scope; without it there is nothing to register into | set `github_runner_org=<org>` |
| `GitHub refused runner groups for org X (403) … Resource not accessible by integration` | the App is missing `organization_self_hosted_runners` (or has not been approved, or the token predates the grant) | the three steps above |
| `package ghcr.io/owner/repo/api:latest — GHCR answered 401 …` | GHCR answers 401 both for "no access" **and** for "no such package", deliberately, so it cannot be used to enumerate private packages | check the real name: `ghcr.io/<owner>/<repo>:<tag>`, no extra path segments. The probe now lists the package's actual tags when the name is right and the tag is wrong |
| `assignment refused … context deadline exceeded` for `:9092` on every manager | the leader is listening but the node cannot reach the port: `ufw` allows the swarm ports, not this one | `ufw allow from 10.0.0.0/8 to any port 9092 proto tcp` on every node (the harden script lists it in `RESTRICTED_HOST_PORTS`; rules created before the port existed must be re-applied) |
| runner state stuck at `starting`, no container on the node | the assignment never arrived, so the node never had a spec to act on | check the node's journal for `assignment read from the leader`, then the leader's log for the refusal reason |
| a node's `docker inspect` shows `/var/run/docker.sock` | `github_runner_socket_proxy=false`, or the container predates the feature | set it back to `true` and let the reconcile replace the container |

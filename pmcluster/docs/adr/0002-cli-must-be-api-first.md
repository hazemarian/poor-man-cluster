# ADR 0002 — The CLI is a local-store client on a multi-node control plane

Status: **proposed** (problem statement, no implementation yet) · Date: 2026-10-29

## Problem

The `pmcluster` CLI opens the **node-local** SQLite store directly and answers
from it. But the control plane is replicated across managers, elects a leader,
and only the leader's view is authoritative. The result is that the same command
gives different answers — or fails — depending on which node you happen to be
logged into.

Locality is implicit. Nothing in the command surface tells the operator whether a
command needs the leader, tolerates any manager, or works on a worker. That
knowledge lives in whoever has been burned by it.

## Evidence (observed, this cluster)

1. **A worker rejects a first-class command with a bootstrap prompt.**
   `pmcluster cluster update` on `nxt-sw-3-w` answered
   *"No cluster configuration found — starting the setup wizard"* while the
   cluster was healthy. Worker daemons run in standby and hold no control-plane
   state, so the CLI correctly found nothing — and then reported the wrong
   conclusion.

2. **Two nodes disagree about live state.**
   `pmcluster observability status` on a follower whose store still held the
   pre-migration credential (`openobserve_admin`, 2026-10-12) reported
   `TELEMETRY unavailable`, while the leader — holding `observability_admin`
   (2026-10-16) — reported `4/4 fresh`. Both were "right" about their own store.

3. **Write commands only work where the state is authoritative.**
   `pmcluster tls site set` must materialise versioned Swarm secrets and redeploy
   Traefik, so it is effectively leader-only. Nothing says so; it simply fails
   or appears to succeed depending on placement.

4. **The same command means different things in different node states.**
   `cluster up` refuses on an installed cluster ("already initialised — run
   `cluster update`") but wants to bootstrap on a fresh node. That is correct
   behaviour, but it is expressed as operator-visible error text rather than as
   a command surface that distinguishes bootstrap from operation.

5. **The leader moves.** Across this work the leadership changed three times
   (dockerd restarts, a manager being demoted and removed). Any rule of the form
   "run it on the leader" was stale within the hour.

6. **Remote mode is advertised but partial.** Run from a laptop with no local
   Docker and no local store, against the leader's API over a tunnel
   (`--api-url http://127.0.0.1:19090 --api-token …`), three commands gave three
   different answers:

   | Command | Result |
   |---|---|
   | `observability status` | **works** — answered from the daemon |
   | `cluster settings` | **works** — printed the settings table |
   | `cluster status` | **fails** — `docker info: Cannot connect to the Docker daemon at unix:///var/run/docker.sock`, ignoring `--api-url` entirely |
   | `credentials list` | **fails** — `data directory not initialised at ~/.pmcluster — run 'pmcluster init' first`, ignoring `--api-url` |

   So `--api-url`/`--api-token` are honoured by some command paths and bypassed
   by others. An operator cannot know which, and the failures accuse the machine
   ("run init", "is the daemon running?") rather than naming the real cause.

## Intended contract (what the CLI is supposed to be)

One binary, two modes, **one switch**:

```
PMCLUSTER_API_URL unset   → local mode
    read ~/.pmcluster store, talk to the local docker socket.
    Requires a cluster MEMBER node: a local store AND a local socket.

PMCLUSTER_API_URL set     → remote mode
    every read and write goes through the daemon API, authenticated with
    PMCLUSTER_API_TOKEN. Requires neither a store nor a socket.
```

The environment variable must be the **only** difference between the two
invocations. Nothing else changes — not the flags, not the command names, not
the output.

**Asymmetry, stated so it isn't discovered the hard way:** the modes are not
mirror images. Local mode only works on a cluster member; remote mode works
anywhere. "Runs locally" therefore means "runs on a member node", and a machine
that is neither should be told that, not handed `run pmcluster init`.

This contract is correct and should be kept. It is the implementation that
violates it (evidence 6).

## Impact

- **Silent drift is the worst outcome**: a plausible but wrong answer (item 2)
  is harder to notice than an error.
- **Writes can land in the wrong place.** A stale follower accepting a setting
  change can persist a value the leader will later overwrite, or render
  constraints from an outdated node list.
- **Operational cost** — every task requires knowing "which node am I on, and is
  it the leader", which is tribal knowledge that expires with each election.
- **The API already exists and is bypassed.** `pmcluster serve` exposes a daemon
  API with `--api-token` and remote mode (`PMCLUSTER_API_TOKEN`), and the CLI's
  global flags already advertise `--api-token` — but most commands read the
  store directly instead.

## Root cause

Resolution of *"where do I get my state"* is duplicated in every command instead
of happening once. Some commands build a store from `~/.pmcluster`, others build
a docker client, others use the API — and each decides for itself, so the modes
drift apart command by command. The CLI is a **store client that also has an API
mode**, rather than an **API client with one local fallback**.

Evidence 6 is what that looks like from the outside: with `--api-url` set, two
commands answered from the daemon and two ignored it entirely, and their errors
blamed the machine ("run `pmcluster init`", "is the daemon running?").

## Options

**A. Document "run it on the leader".** Cheapest, and the status quo in prose.
Rejected: elections invalidate it (evidence 5), and it cannot help an operator
without shell access.

**B. CLI discovers the leader and forwards itself (e.g. over SSH).** Rejected:
requires the CLI to hold credentials for other nodes, and makes failures
impossible to reason about.

**C. API-first CLI (recommended).** The CLI talks to the daemon API — local
socket by default, `--api-url` / `--api-token` for remote. The daemon answers
from its own store and, for writes, either forwards to the leader or refuses
with an explicit "this must run on the leader; leader is `X`" error. Direct
store access survives **only** for commands that precede a cluster: `init`,
`cluster up`, `cluster reset`, `join`.

**D. Improve store replication only.** Helps item 2's lag, does not address
write authority or command semantics (items 1, 3, 4).

## Acceptance criteria

- The same command produces the same answer from any node, **or** an explicit
  error naming the leader. Never a setup-wizard prompt on a healthy cluster.
- No non-bootstrap command reads the store directly.
- A follower's stale read cannot be served silently: the daemon reports its
  revision and the CLI warns when it is behind the leader.
- Remote mode works with a token, so an operator does not need shell access to
  the leader.
- Bootstrap commands detect "a cluster already exists elsewhere" and say so,
  instead of offering to create a second one.

## Command classification (to be confirmed during implementation)

| Class | Commands |
|---|---|
| Any node, local answer | `version`, `status` (clearly labelled as *this node's view*) |
| Needs authoritative state | `cluster update`, `cluster settings`, `credentials`, `deploy`, `backup`, `tls site set`, `node promote/demote`, `observability status` |
| Bootstrap only | `init`, `cluster up`, `cluster reset`, `join` |

## Implementation plan

1. **One resolver.** `resolveCluster()` returns an API-backed or store-backed
   implementation, decided solely by whether `PMCLUSTER_API_URL` is set. No
   command inspects `~/.pmcluster` or calls `docker info` on its own.
2. **Bootstrap is the only exception.** `init`, `cluster up`, `cluster reset`,
   `join` may use the store directly, because the cluster does not exist yet.
   The exception is declared in one place, not inferred per command.
3. **Messages follow the mode.** In remote mode a failure names the API and the
   token; in local mode it names the store and the socket. No message tells an
   operator to bootstrap a machine that will never host a cluster.
4. **A table test pins it.** For every command in the classification table,
   assert that it goes through the resolver — or is a declared bootstrap
   exception. That is what stops the next drift, which is how this one happened.

## Reproducing the evidence (no local Docker needed)

```bash
ssh -f -N -L 19090:127.0.0.1:9090 root@<leader>          # tunnel to the daemon
TOK=$(ssh root@<leader> cat /root/.pmcluster/bootstrap-token.txt)

for c in "observability status" "cluster settings" "cluster status" "credentials list"; do
  printf "%-24s " "$c"
  ./pmcluster $c --api-url http://127.0.0.1:19090 --api-token "$TOK" 2>&1 | head -2
done
```

Currently: first two answer from the daemon, last two fail on the local machine.
After the fix, all four must answer from the daemon.

## Handoff checklist for the next session

- [ ] Read this ADR first; it carries all findings from the 2026-10-29 session.
- [ ] Inventory the direct-access sites: `grep -rn "docker info\|\.pmcluster\|store.Open" internal/cli/`.
- [ ] Build `resolveCluster()` + the resolver table test (step 4 above).
- [ ] Convert the commands in the classification table, one commit per group.
- [ ] Rewrite the mode-specific error messages.
- [ ] Re-run the four-command matrix above; all four must honour `PMCLUSTER_API_URL`.
- [ ] Only then consider `status` merging views across managers (open question 2).

## Open questions

1. For writes, forward to the leader or refuse with a leader hint? Forwarding is
   friendlier but hides elections; refusing is honest but costs a retry.
2. Should `status` merge views (gossip between managers) or stay resolutely
   local with a "stale by N revisions" warning?
3. How does a bootstrap command distinguish "fresh machine" from "machine that
   was removed from a cluster" — the second must not silently re-init?

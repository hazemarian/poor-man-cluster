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

The CLI is a **store client**, not an **API client**. The daemon owns the store
and the reconcile loop; the CLI reaches around it. Every command therefore
inherits an unstated assumption that it is running on the authoritative node.

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

## Open questions

1. For writes, forward to the leader or refuse with a leader hint? Forwarding is
   friendlier but hides elections; refusing is honest but costs a retry.
2. Should `status` merge views (gossip between managers) or stay resolutely
   local with a "stale by N revisions" warning?
3. How does a bootstrap command distinguish "fresh machine" from "machine that
   was removed from a cluster" — the second must not silently re-init?

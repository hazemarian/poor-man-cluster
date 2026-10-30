# ADR 0001 — Engine seam: keep Swarm behind a translation layer

Status: accepted (layer 1 landed) · Date: 2026-10-27

## Context

pmcluster is a platform that sits above a container engine. Today the engine is
Docker Swarm; Nomad and k3s were both considered as alternatives — the whitepaper
records the k3s rejection ([`docs/whitepaper.md`](../../../docs/whitepaper.md),
§ *Orchestrator*), and Nomad's licensing cost is recorded below, in
Consequences. Whichever engine runs underneath, the platform
layer — placement policy, backend provisioning, backups, SSO wiring, TLS
rotation, host hardening — is the product. The engine is a dependency.

An audit of the code before this ADR found the engine coupling is real but
bounded:

| Kind | Scale | Where |
|---|---|---|
| `runtime.*` domain types used outside the engine package | ~20 refs / 8 files | `internal/cluster/update.go`, `internal/cli/observability_backend.go`, `internal/cluster/secrets.go`, `internal/cluster/networks.go`, `internal/controlplane/state.go`, `internal/cli/node.go`, `internal/stacks/resolver.go` |
| Engine-specific constraint syntax | **one function** | `internal/manifest/compose_writer.go` (the constraint-rendering block, before this change) |
| Deploy primitives (`ServiceCreate/Update`, `StackDeploy`) | 13 call sites | `internal/cluster/` |

Two findings drove the design:

1. The embedded stack templates contain **no** engine constraint syntax. They
   take a node *name* as a render variable. Only the constraint *serialisation*
   was engine-shaped — so the platform's placement intent was already neutral in
   everything but its output format.
2. The leak is not uniform: the expensive part is *semantic* (how ingress binds
   to nodes, how the control plane replicates, what "healthy" means), not the
   ~20 type references.

## Decision

Placement is expressed in platform terms in `internal/engine`, and rendered into
engine syntax by an adapter in `internal/engine/<engine>`.

- `engine.Placement{Node, Role, MatchLabels}` — where a workload may run, with
  **no** constraint strings in it.
- `engine.PlacementFor(declared, pinNode, stateful, global)` — the platform's
  rules in one place: role restriction, auto-pin of stateful workloads, explicit
  hostname pin, global-mode exemption.
- `engine.Capabilities` — capability flags (`RoutingMesh`, `NativeJobs`,
  `NativeCanary`, `NativeAutoscale`, `VolumesFollowWorkload`, `NodeLabels`), so
  the platform can prefer native behaviour and fall back deliberately instead of
  probing.
- `internal/engine/swarm` — renders `Placement` into `node.role == …` /
  `node.hostname == …`, and declares what Swarm cannot do.

A node pin wins outright over a role: engines combine placement constraints
conjunctively, so emitting both makes a workload unschedulable whenever the
pinned node holds the other role.

`VolumesFollowWorkload: false` is why stateful workloads are pinned at all. That
flag is the machine-readable form of the lesson from the `nextrum-landing` move:
a named volume lives on one host, so an unpinned stateful service either strands
its data or returns with an empty one.

## Consequences

**Good**

- The platform's placement rules are unit-testable without a swarm.
- Adding an engine means writing an adapter plus its translation tests, and
  declaring its capabilities — the platform's rules stay untouched.
- The capability flags make the platform's compensations (job reaping, canary
  weights, autoscale policy) explicit rather than emergent.

**Costs / limits**

- This does **not** make an engine swap a configuration flag. A second engine
  still needs: Compose→its-format translation, its ingress provider instead of
  Traefik's Swarm provider, its secret/variable store, and re-proving the e2e
  suite. Realistic estimate: 4–6 weeks for an adapter, after this seam.
- Three differences cannot be erased by any adapter, only declared: routing
  mesh vs per-node ingress, Raft manager set vs server set, and what
  "converged" means for health. `engine.Capabilities` is where they live.
- Nomad specifically carries a licensing constraint (BUSL-1.1 since 1.7)
  incompatible with selling pmcluster as a service; that is a business
  constraint on the choice of engine, not a technical one.

## Layer plan

1. **Landed:** neutral `Placement` + Swarm translator; `compose_writer` no longer
   contains engine syntax; rules and translation covered by table tests.
2. **Landed:** `engine.Engine` (Capabilities, Nodes, Leader, Workloads, Restart),
   `engine.Node` / `engine.Workload` read model, `engine/swarm` adapter, and
   `engine/contracttest.Run` — the parity suite every adapter must pass.
3. **Already satisfied — no work needed.** The parse/render split exists:
   `manifest.Parse() → *IR`, then `composeXFromIR` renders. It predates this ADR.
4. **Deliberately NOT moved**, after reading each site:

   | Remaining refs | What they are | Why they stay |
   |---|---|---|
   | ~15 × `runtime.SecretSpec/NetworkSpec/ConfigSpec` | transport payloads | they describe engine API calls, not platform intent; they belong to the transport |
   | `internal/cli/observability_backend.go` (`parseableLocator`) | a local narrow interface (NodeList/ServiceList/ServiceTasks) | already a test seam; routing it through `Engine` adds indirection without removing any engine knowledge |
   | `internal/controlplane/state.go`, `internal/stacks/resolver.go` | domain structs with engine-agnostic fields | no constraint syntax, no engine objects |

   The original audit over-counted: `runtime.Service`/`runtime.Node` are already
   *domain* types (`EngineVersion`, `Role`, `Mode`, `Platform`), not raw engine
   structs. The only genuine semantic leak was constraint serialisation, and it
   is now behind the adapter.
5. Only if justified: a second adapter (4–6 weeks: Compose translation, its own
   ingress provider, its own secret store, re-proved e2e).

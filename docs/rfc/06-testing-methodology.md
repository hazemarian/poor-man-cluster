# 06 — Testing methodology

## 1. The doctrine

> you should not do any manual work around if there is something that needs
> manual intervension that means it is a bug or an issue
> — user directive, 2026-10-02, issued four times in the same session

Three consequences, all visible in the reports:

1. Every campaign has a mandatory **"Manual interventions (logged)"** section.
   An intervention you had to perform is, by definition, a defect or an
   unimplemented behaviour — it is recorded as one or the other, never as a
   footnote.
2. A fix must be **deployed back to the same hardware and re-verified there**.
   "Fixed in a unit test" is not "fixed" until it is fixed on the node that
   broke.
3. A verification failure **stops the campaign** and is reported rather than
   worked around (the rollout sessions carry this rule explicitly: "if a
   verification fails, STOP and report instead of continuing").

## 2. The two-tier practice: sandbox vs production

| | Sandbox | Production |
|---|---|---|
| Nodes | 4 VPS: `nxt-sw-1-m` (leader, 2-core/3.7 Gi), `nxt-sw-2-m`, `nxt-sw-4-m` (1-core/1.8 Gi), `nxt-sw-3-w` (1-core, worker + backup store) | `nextrum-sy-1` (manager), `nextrum-sy-2` (worker), `wafaa` (single node, user `wafaa`) |
| Purpose | destroy freely; every campaign runs here | customer workloads (15 stacks on nextrum) |
| Domain / certs | `nextrum-sy.de` (BYO self-signed in TC1–TC9) | `nextrum-sy.de` / `nextrum-sy.com` behind an LB |
| Offsite S3 | IONOS eu-central-3, bucket `nextrum-bu` | same bucket, emptied between campaigns (93 → 0 objects before the TC10 rebuild) |
| Tailscale | present for operator SSH; **removed** for the "full 4-node test" so the cluster behaves like a plain public cluster | used for SSH; swarm traffic rides public IPs |

The sandbox's asymmetry is deliberate and is itself a test fixture: one big
node, three small ones, one of which doubles as the backup store. That
topology is what exposed BUG-09/BUG-A (a JVM-class workload on a 1-core node)
and BUG-026 (volume dirs created on the deploying host).

## 3. The campaign ladder

The ten campaigns are not a matrix of features — they are a **ladder of
topologies**, each rung adding one axis of failure:

| Campaign | Date | Topology | Axis added | Bugs |
|---|---|---|---|---|
| **TC1** | 2026-10-04 | 1 manager | bring-up: setup, multi-service deploy, observability, UI, control plane | BUG-001 |
| **TC2** | 2026-10-04 | 1 manager | custom `volume_root`, ACME through an LB, offsite S3, backup/restore round-trips | BUG-002, BUG-003 |
| **TC3** | 2026-10-04 | 1 manager | webhooks + CLI + remote API + multi-process store access | BUG-004, BUG-005, BUG-006 |
| **TC4** | 2026-10-04 | 1 manager | rotation, retries, platform deploys | BUG-007, BUG-008, BUG-009 |
| **TC5** | 2026-10-04 | 1 manager | **regression re-verification** of BUG-007/008 + the stack-deletion path | — |
| **TC6** | 2026-10-04 | 1 manager | the remaining surface (per-host TLS, credentials, …) | BUG-010, BUG-011, BUG-012 |
| **TC7** | 2026-10-05 | 1 manager + 1 worker | **multi-node**: join, placement, Traefik everywhere | BUG-014 |
| **TC8** | 2026-10-05 | 2 storage nodes | **storage HA**: promote/demote, `stack move`, failover with S3 restore | BUG-015 (×2 rounds) |
| **TC9** | 2026-10-07 | 1 leader + 1 worker | **the backup store**: placement, both legs, drift probes | BUG-017 |
| **TC10** | 2026-10-09 | 4 nodes (3 mgr + 1 worker) | **everything at once**: leader failover, storage failover, DR drill, pre-production | BUG-019a/b, BUG-020…BUG-032, BUG-03, BUG-05/06, BUG-09 (+2 open) |

Read the ladder as a deliberate escalation: single-node bugs (which are cheap
to find) were cleared in one day across TC1–TC6; the multi-node bugs
(BUG-014, BUG-015, BUG-017) needed two-node topologies; the *composition*
bugs (BUG-020 through BUG-032) only appeared when four nodes, a store, an
offsite bucket and a failover were all live at once.

## 4. The verify → tag → rollout workflow

> to make things faster do not cut tags wait for ci then deploy when you are
> sure all tests are done and we are fine we cut tag and merge to prod it will
> really speed up the process
> — user directive, 2026-10-02 (DEC-16)

The shape that actually ran (TC10 and the following rollout):

1. **Build a dev binary** and deploy it to all four sandbox nodes
   (`install.sh VERSION=…` or a direct binary copy). No tag.
2. **Run the campaign slice**; log every manual intervention.
3. **Fix, rebuild, redeploy to the same nodes, re-verify.** Repeat until the
   slice is clean. (TC10 went v0.2.166.1 → v0.2.175 this way, with every fix
   deployed back mid-campaign.)
4. **Cut exactly one tag** once CI is green.
5. **Purge the sandbox, empty the bucket, rebuild a fresh cluster from the
   released tag** — this is a distinct step and it matters: it proves the
   release works from a clean bootstrap, not just as an upgrade over a
   half-fixed cluster.
6. **Roll production** with `install.sh VERSION=vX.Y.Z`, verify per host, then
   re-run a second `cluster update` and confirm it is a no-op.

Hard rules that came out of the rollout sessions:

- **Write every SSH command inline.** zsh does not word-split a `$SSH`
  variable; a captured rollout log full of `no such file or directory` is
  useless evidence.
- **`systemctl start` is not `systemctl restart`.** If the daemon never
  stopped, `start` is a no-op and the *old* binary keeps running under a
  deleted inode (`/proc/<pid>/exe -> … (deleted)`). TC10's rollout hit this on
  `nextrum-sy-2` and logged it as an intervention.
- **Never improvise a destructive operation.** No `docker stack rm`, no node or
  settings changes "unless strictly required — if one is, log it".
- **Grep `ERR`, not `level=error`.** The daemon logs zerolog console format.

## 5. The e2e tiers

Three independent tiers, deliberately decoupled:

| Tier | Where | What | Gate |
|---|---|---|---|
| **Unit + golden + structural** | every push (`pmcluster.yml`) | ~49% test LOC; translator goldens, `TestClassifyLocalCluster`, `TestWatchSwarmLeadership_*`, `TestUpdate_RedeploysWhenLiveServiceLabelDrifted`, `TestSignV4Headers_SignsAmzDate`, `TestMultiProcessNoWALSplitBrain`, … | blocking |
| **Smoke e2e** | `make e2e` in the main workflow | the fast path (install/unit/smoke) | blocking |
| **Real-cluster e2e** | `swarm-e2e.yml` — "runs SEPARATELY from the main build gate" | cluster up + deploy + webhook on a real Swarm (`PMCLUSTER_E2E_SWARM=1`) | non-blocking by design |

The tiering is itself a decision: the real-cluster e2e exists because the
unit tier cannot see composition failures, and it is non-blocking because
Swarm e2e is flaky in ways a unit test is not — blocking on it would train the
team to ignore it. The campaigns (TC1–TC10) are the *third* tier: manual,
topology-driven, and the only one that has ever found a Critical.

## 6. The falsification discipline in regression tests

A regression test that cannot fail is a decoration. The reports are explicit
about which tests are **falsified by construction**:

- `TestUpdate_RedeploysWhenLiveServiceLabelDrifted` is described as "fails
  against the old DB-only comparison" (BUG-017).
- `TestUpdate_FailedDeployLeavesStaleHashRetriedOnNextUpdate` is "fails on the
  old code" (BUG-009).
- `TestWatchSwarmLeadership_StableLeaderEmitsOnce` "asserts one `true` then
  silence over `2*leaderPollInterval`; **fails without the fix**" (BUG-001).
- `TestMultiProcessNoWALSplitBrain` "asserts no `-wal`/`-shm`/`-journal` ever
  exist" (BUG-004).
- `TestIsSwarmUnavailable` is "a six-case table **falsified by disabling both
  patterns**" (BUG-020) — i.e. the test was run against a build where the fix
  was removed, to prove it catches the regression.
- The systemd unit guard `TestRenderSystemdUnit` asserts
  `WantedBy=docker.service` (BUG-021b), and `daemonExecStart` guards the
  "unit must include `serve`" regression from the field.

The pattern is consistent: **each test names the old behaviour it would
observe**. That is what makes the catalogue in Chapter 04 auditable.

## 7. What the campaigns cannot catch

- **Anything that needs a real ACME issuance** behind a load balancer
  (BUG-027 was found by reasoning + observation, then cancelled as unsolvable).
- **Provider-side key rotation** (BUG-028's second fault was an IONOS key
  rotated on the provider side — no test can see that coming).
- **Hardware-specific network behaviour** (BUG-015: the public IP silently
  drops SYN while the private interface works; BUG-030: the ephemeral port
  range was allowed only for the original two-node pair).
- **Long-window resource starvation** (BUG-A: an hour of swap death on a
  1-core node). Only a campaign that leaves the cluster running for hours
  catches this.

Those four categories are exactly why the campaigns run on real VPSes and not
in CI. They are also, not coincidentally, the four hardest entries in the bug
catalogue.

# 05 — Evolution timeline

Two clocks run through this project's history and they do not agree:

- **git** has 357 commits from `2982549` (2026-04-22, "Initial commit — poor
  man's Docker Swarm cluster") to `ad6e67f` (2026-10-09, v0.2.179).
- **tags** begin at `v0.2.158`. Everything before that exists only as a
  `CHANGELOG.md` entry (the file reaches back to v0.2.42), so a "what shipped
  in vX" question before v0.2.158 can only be answered from the changelog, not
  from a tag.

Commit density is a good proxy for the project's phases:

| Period | Commits | What was happening |
|---|---|---|
| 2026-04-22 → 2026-05-19 | 9 | shell-script origins (`bin/setup.sh`), then the "refactor use go app" turn |
| 2026-06-07 → 2026-08-22 | 39 | the docker-compose era: versioned configs, OTel, Portainer, install.sh |
| 2026-08-31 → 2026-09-13 | 25 | the Go service-layer refactor; the first real architecture docs |
| 2026-09-15 → 2026-09-21 | 111 | **the great conversion**: DSL, single pipeline, content addressing, control loop |
| 2026-09-22 → 2026-10-02 | 95 | storage/HA work, the LINSTOR reversal, the control plane, the surviving-kit work |
| 2026-10-03 → 2026-10-09 | 78 | **the campaign era**: TC1 → TC10 and v0.2.161 → v0.2.179 |

The last seven days of the project's recorded history contain 78 commits and
~40 releases. That is not instability — it is the campaign loop (Chapter 06)
running at full speed against real hardware.

---

## Phase 1 — Shell, then Go (2026-04 → 2026-05)

`2982549` "Initial commit — poor man's Docker Swarm cluster"; `06095a5` links
RFC issue #1 in the README (the project has been RFC-driven from the start);
`e022790` "Fix all critical issues from project review"; `5240157` unifies
setup into `bin/setup.sh` with a manager/worker flag. Then four commits in two
days — `95c443c`, `13a8a34`, `82ce2d7`, `a66a46b` — all titled "refactor use go
app": the move from shell to a Go binary.

## Phase 2 — The docker-compose era (2026-06 → 2026-08)

`f83f01e` versioned Docker configs, health checks with diagnostics, embedded
config files; `d0e025e` versioned TLS cert/key secrets with auto-GC; the OTel
receiver_creator fixes (`e0133c5` … `5046ac3`); `8ff5c54` "implement 6
security & reliability improvements" (the ancestor of
`docs/security-and-improvements.md`); `7cafc02` bumps Portainer 2.39.5 —
Portainer was still part of the stack and would later be removed outright
(directive: "is it worth it to keep portainer?").

The defining artefact of this era is the numbered-version scheme (`_v042`,
`_v043`…) for configs and secrets, which is exactly what DEC-03 would later
replace.

## Phase 3 — The service-layer refactor (2026-08-31 → 2026-09-13)

Twenty commits across two weeks: ports and adapters, the two composition
roots, the `internal/` domain layout, and the architecture document that still
governs the code (`pmcluster/internal/ARCHITECTURE.md`). The user's directive
for the shape of the work:

> isolate the docker client and leave that question to the future this was my
> point of that refactoring and isolation even the output of dsl translation
> should be isolated by interface and there multiple implimentation swarm yaml
> writer terraform writer (what if someone uses something different that docker
> swarm) or helm yaml writer for k8s

## Phase 4 — The great conversion (2026-09-15 → 2026-09-21, 111 commits)

The densest week in the project's history. In order:

1. **DB as source of truth** — "the sorce of true is the db cluster up and
   update should update the file and and db".
2. **Content addressing** — "i would suggest all config and secrets should be
   compared by hash" (DEC-03).
3. **Platform-via-DSL** — "why not use DSL and then we have a single
   translation layer" (DEC-02), shipped as v0.2.152.
4. **The control loop** (v0.2.132) — the leader-only reconcile loop with
   platform reconcile, app-stack sync and the `stack_status` snapshot;
   v0.2.134 intensive logging, v0.2.135 OTLP traces.
5. **The control-plane Raft kit** (v0.2.138) — `pmcluster_state_*` configs,
   startup-only restore, key/ciphertext split.
6. **Storage placement** (v0.2.139) — `storage_nodes` round-robin, the outage
   pause, `stack move`; **Path 1** (pin + backup) adopted, LINSTOR/DRBD
   explicitly dropped.
7. **Interactive exec** (v0.2.141, L3) — browser terminal over a console→daemon
   websocket relay.
8. **`depends_on` control-plane ordered deploys** (v0.2.118) replacing the
   shell wait-wrapper (DEC-06a).

## Phase 5 — Storage, HA and the surviving kit (2026-09-22 → 2026-10-02)

Ninety-five commits. The work is no longer about the render pipeline; it is
about what happens when hardware dies:

- the **storage-node model** takes shape (DEC-08), and the LINSTOR directive
  is reversed in favour of Path 1 (DEC-13);
- the **in-cluster backup store** decision chain runs its full course — MinIO
  (v0.2.160) → Garage → kopia → SeaweedFS (v0.2.160.1) — and the rclone
  replicator is removed in favour of offen's native double-write (DEC-06);
- the **control-plane Raft kit** is hardened (startup-only restore, DEC-07a);
- the cluster-lifecycle design is settled: `cluster reset`, `cluster down
  --purge` and the purge-backup safety net (they ship as v0.2.161, four days
  later, in the campaign era).

Most of this phase's design work shipped *after* it ended, interleaved with
the campaigns — which is why the release table below starts at v0.2.155
(2026-10-05) even though the decisions are from 2026-09-22 → 10-02.

It ends with the directive that governs everything after it:

> you should not do any manual work around if there is something that needs
> manual intervension that means it is a bug or an issue

(2026-10-02 — issued four times in the same session, together with the
four-node test-cluster specification: "we need 2 storage nodes … the 2 storage
node for testing the failover and the 2 manager nodes to test leader
failover".)

## Phase 6 — The campaign era (2026-10-03 → 2026-10-09)

Ten campaigns in six days, and ~40 releases. The campaign ladder (dates and
topologies from the reports themselves):

| Campaign | Date | Release under test | Headline |
|---|---|---|---|
| TC1 | 2026-10-04 | v0.2.141 (+`e145d6f`) | BUG-001 — leadership re-emit; snapshots never ran |
| TC2 | 2026-10-04 | v0.2.142 | BUG-002/003 — volume_root + restore dest |
| TC3 | 2026-10-04 | v0.2.143 → .144 → .145 | BUG-004/005/006 — WAL corruption, restore clobber |
| TC4 | 2026-10-04 | v0.2.145 → .148 | BUG-007/008/009 — rotation, retry, hash-before-apply |
| TC5 | 2026-10-04 | v0.2.148 | regressions re-verified live (3/3 PASS) |
| TC6 | 2026-10-04 | v0.2.149/150/151 | BUG-010/011/012 — TLS + credentials (two found live) |
| TC7 | 2026-10-05 | v0.2.156/157 | BUG-014 — Traefik placement (the prod 404 outage) |
| TC8 | 2026-10-05 | v0.2.158/158.2 | BUG-015 — mover transit across hardened nodes |
| TC9 | 2026-10-07 | v0.2.162.1/163/164 | BUG-017 — live-Swarm drift blindness |
| TC10 | 2026-10-09 | v0.2.166.1 → .175 | 16 bug write-ups; the two pre-production blockers |

The same six days produced the storage-HA directives (DEC-08), the
"leader is always a storage node" invariant (BUG-024), and the release
workflow (DEC-16).

### v0.2.16x → v0.2.179

| Tag | Commit | What and why |
|---|---|---|
| v0.2.161 | — | `cluster reset`, self-healing/existence-aware `cluster update`, `cluster down --purge` writes a restorable `purge-backup-<ts>.tar.gz` |
| v0.2.162 | — | backup store placement (`backup_store_on`) |
| v0.2.162.1 | — | BUG-016 store bucket bootstrap; setup-wizard prompts |
| v0.2.163 | `aba97eb` | **BUG-017** — the `io.pmcluster.rendered_hash` label + live-Swarm comparison |
| v0.2.163.1 | `bfb183c` | name the reason each platform stack is redeployed |
| v0.2.163.2 | `c1786d2` | a partially applied platform deploy counts as drift |
| v0.2.164 | `2b62287` | the same hash label for **app** stacks |
| v0.2.165 | `d0a171d` | **BUG-018** — stamp the hash after the apply, not before |
| v0.2.166 | `1972b9a` | `pmcluster secret heal` — verify decryptability, re-materialise swarm mirrors |
| v0.2.166.1 | `ad58824` | SA5011 lint gate under Go 1.25 |
| v0.2.167 | `9679e94` | **BUG-020/021** — survive a leaderless swarm; `BindsTo=docker.service` |
| v0.2.168 | `3a2228d` | **BUG-022** — standby-manager `cluster update`/`reset` |
| v0.2.168.1 | `8895c1a` | standby managers refresh the daemon unit too |
| v0.2.168.2 | `20da9bf` | **BUG-021b/023** — `WantedBy=docker.service`; `restart: any` for replicated services |
| v0.2.168.3 | `0dbfd61` | platform services that set `restart` explicitly recover too |
| v0.2.169 | `b1ad650` | **BUG-026** — storage dirs are a node-local responsibility; volume-repair loop |
| v0.2.169.1 | `e37393f` | repair resolves the local-driver volume `device` |
| v0.2.169.2 | `c1914e2` | an empty `volume_root` counts as unset |
| v0.2.169.3 | `1313f81` | start the volume repair **before** the leadership wait |
| v0.2.170 | `7fbb2d7` | **BUG-024/025** — the leader is always a storage node; index the in-cluster store; **no one-shot backup service** |
| v0.2.171 | `bec1d79` | storage follows leadership — promote the new leader, demote the former |
| v0.2.172 | `0213eb2` | **BUG-028** — `AWS_S3_BUCKET_LOOKUP: path`; the rclone replicator is gone |
| v0.2.173 | `3a408c1` | **BUG-029** — sign `x-amz-date` in SigV4 |
| v0.2.174 | `8f608ef` | **BUG-030** — move/failover transit via the object store; no cross-node host ports |
| v0.2.174.1/2/3 | `5e98dfe`/`3ae8fc0`/`f8f4279` | the three mover follow-ups |
| v0.2.175 | `37e38b1` | **BUG-031/032/019a/b/03/05/06/09** — failover/restore correctness + platform placement + rotate atomicity. The pre-production blocker release. |
| — | `91516f5` | TC10 report committed (documentation is versioned with the code) |
| v0.2.176 | `adf7191` | the hardening release: `$$` escaping, refuse the default session secret, subset keeps top-level blocks, fast-reject non-`pmc_` tokens, RBAC for rendered configs, worker standby, sync-error throttle |
| v0.2.177 | `6b42cd7` | platform/app separation — the read-only Platform page, `service list --platform` |
| v0.2.178 | `07b6100` | console SSO sign-out, **Traefik on managers** (DEC-11), `Secure` session cookie, vendored htmx, bcrypt 72-byte limit |
| v0.2.179 | `ad6e67f` | the brand system — six `--brand-*` palettes, pre-first-paint picker, inlined logo (DEC-14) |

---

## What the last 40 releases are *about*

Read as a sequence, v0.2.161 → v0.2.179 is one continuous argument:

1. **Make the store authoritative and rebuildable** (v0.2.161 reset/purge).
2. **Make drift visible** (v0.2.163 → .165: live label, app stacks, stamp
   after apply).
3. **Make the control plane survive its own leader** (v0.2.167 → .168.3:
   quorum loss, systemd unit, restart policy).
4. **Make node-local state a node-local responsibility** (v0.2.169.x:
   volume repair).
5. **Make storage follow leadership and be discoverable** (v0.2.170 → .171).
6. **Make the store path work end to end** (v0.2.172 → .174.3: offen's native
   double-write, SigV4, the store-transit mover).
7. **Close the pre-production blockers** (v0.2.175).
8. **Harden the trust boundary and the console** (v0.2.176 → .178).
9. **Give it an identity** (v0.2.179).

Steps 1–6 are exactly the six subsystems Chapter 02 describes. The architecture
document and the changelog are, for this project, the same document written in
two languages.

---

## Unverified / gaps

- `CHANGELOG.md` contains a **gap between v0.2.85 and v0.2.108**, and some
  duplicated v0.2.121/122/123 entries. Nothing in the code or the test reports
  contradicts the entries that do exist, but a release in that window cannot be
  verified from the changelog alone.
- The exact release that carried the BUG-001 fix (`e145d6f`) is recorded in
  TC1 as "not in any tagged release yet" — it shipped in whatever became
  v0.2.142. Unverified which tag first contained the commit, since no tag
  predating v0.2.158 exists in git.
- v0.2.152 is credited with the platform-via-DSL conversion in
  `docs/improvements.md`; the CHANGELOG entry for it predates the tagged range
  and was not re-read line-by-line for this RFC.

# RFC — Poor Man's Cluster (pmcluster) at v0.2.179

*A history-driven technical RFC. Every architectural claim is cited to code
(`file:line`), a release (`CHANGELOG.md` + git tag), a test campaign
(`docs/test-reports/TC1…TC10`), or a quoted user directive recovered from the
opencode session history.*

---

## Abstract

pmcluster ("Poor Man's Cluster") is a self-hosted PaaS control plane for cheap
VPS hardware. It turns a handful of 1-core / 1.8 GB servers into a
Swarm-scheduled application platform: a declarative YAML DSL, a desired-state
SQLite store, a leader-only reconcile loop that converges the Swarm onto that
store, content-addressed secrets and configs, node-pinned stateful storage with
opt-in S3-backed failover, in-cluster + offsite double-write backups, and an
operator console behind a Traefik SSO/basic-auth gate.

The platform is deliberately *poor*: the answer to "how do I make stateful data
highly available?" is **pin the service to a node and back it up hourly** — not
a replicated block device. Block replication (LINSTOR/DRBD9) was explicitly
evaluated and rejected for footprint (a JVM controller, ~1 GB RAM before any
app data, DRBD9 kernel modules) and for contradicting the project's premise
(`docs/storage-and-databases.md:46-119`).

This RFC is **history-driven**. The design is the residue of ~357 commits and
~138 releases (v0.2.42 → v0.2.179) and, more importantly, of a series of live
campaigns on real hardware (TC1–TC10) during which 40+ tracked defects
(BUG-001 … BUG-034, BUG-03/05/06/09, BUG-A) were found, root-caused, fixed and
re-verified. Chapter 03 is a decision log: for each significant choice it
records the options that were considered, the ones that were rejected and why,
the user directive that settled it where one exists, and the code that
implements it today.

The single most load-bearing rule in the project is a testing doctrine
recovered verbatim from the session history:

> "you should not do any manual work around if there is something that needs
> manual intervension that means it is a bug or an issue"
> — user directive, session `ses_f015e7886ffebPQLfTXFCjrRJx`, 2026-10-02

It is why the platform has a reconcile loop at all: every operator
intervention observed during a campaign became either a bug fix or a new
convergence behaviour.

---

## Chapters

| # | Chapter | Contents |
|---|---------|----------|
| 01 | [Problem and goals](01-problem-and-goals.md) | Who this is for, the hardware envelope, design goals, and the explicit non-goals. |
| 02 | [Architecture](02-architecture.md) | Desired vs actual state; the reconcile loop and drift detection; the Raft-replicated control plane; storage, backups and the store-transit mover; networking, edge/auth, observability; the DSL and the single render pipeline; the security model. |
| 03 | [Decision log](03-decision-log.md) | The heart of the document: 19 decisions (DEC-01 … DEC-16 plus DEC-06a/07a/08a) with context → options → decision → rationale (user directives quoted verbatim) → consequences → implementation. |
| 04 | [Bug catalogue](04-bug-catalogue.md) | BUG-001 … BUG-034, BUG-03/05/06/09 and BUG-A: symptom, root cause, fix, release, verification. Grouped by theme. |
| 05 | [Evolution timeline](05-evolution-timeline.md) | Six phases (2026-04 → 2026-10) by commit density, then the release table for v0.2.155 → v0.2.179 with tags and commits. |
| 06 | [Testing methodology](06-testing-methodology.md) | The TC1–TC10 campaigns, the sandbox-vs-production practice, the verify→tag→rollout workflow, e2e tiers, and the falsification discipline. |
| 07 | [Limitations and open questions](07-limitations-and-open-questions.md) | What is still broken or unsolvable, the operational constraints, and the deliberately-unfixed review items. |

---

## Version and provenance

- **Documented version:** `v0.2.179` (commit `ad6e67f`, 2026-10-09).
- **Repository:** `poor-man-stack`, Go module rooted at `pmcluster/`
  (`github.com/hazemarian/poor-man-cluster/pmcluster`).
- **History:** first commit `2982549` (2026-04-22, "Initial commit — poor man's
  Docker Swarm cluster") through `ad6e67f` (2026-10-09). 357 commits,
  235 tags (tags begin at `v0.2.158`; releases before that exist only in
  `CHANGELOG.md`).
- **Evidence base:**
  - code under `pmcluster/` (364 Go files, ~86k LOC, ~49% test LOC — measured
    in the 2026-10-08 code review, session `ses_ee3bd006cffefd33fNBUy0kz4v`);
  - `CHANGELOG.md` (1729 lines, v0.2.42 → v0.2.179);
  - `docs/*.md` and `docs/test-reports/TC1…TC10`;
  - opencode session history (`~/.local/share/opencode/opencode.db`), mined
    for user directives, rejected alternatives and live operational findings.

> **Scope note.** This RFC documents the platform as it exists at v0.2.179. It
> does not modify any source file, and it does not supersede the operational
> documents (`docs/*.md`) — where they disagree with this RFC, the code wins and
> the disagreement is called out in Chapter 07.

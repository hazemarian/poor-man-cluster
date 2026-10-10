# 07 — Limitations, stale documents and open questions

Everything below is either (a) a known, still-open defect at v0.2.179,
(b) a documented behaviour that no longer matches the code, or (c) a design
decision whose cost is visible but unresolved. Each item cites its evidence.

---

## 1. Open defects (status: OPEN at v0.2.179)

### BUG-033 — a worker's daemon runs a reconcile loop it cannot perform

*Evidence:* `docs/test-reports/TC10-four-node-storage-ha-and-preprod.md:295`;
TC10 conclusion (`:319`) names it as one of two items left open with "Next:
fix BUG-033/BUG-034, then re-run the TC10-D/TC10-E slices against
production."

A worker node runs the full daemon, which starts the reconciler
(`pmcluster/internal/cli/serve.go:254-299`) — but a worker cannot lead, so it
can never hold the leadership lease that `reconcile.RunOnce` requires
(`reconcile.go:177 TryLock`). Every pass therefore does nothing, yet still
pays the loop's cost (metrics, span emission, throttled state transitions at
`reconcile.go:147-172`). The design question the fix must answer: should a
standby/worker skip the reconciler entirely (the daemon already distinguishes
standby behaviour at `cli/leader.go:52/:74/:156`), or should it run a
degraded, read-only probe?

### BUG-034 — two orphaned stacks in the worker's local DB fail manifest parsing every pass

*Evidence:* `TC10-…md:300`.

Related to BUG-033: a worker's *local* store retains stack rows whose manifests
the worker cannot parse (or no longer exist in the render path), producing a
parse failure on every reconcile pass. Because the pass is already inert
(BUG-033), the failures are currently invisible noise — but they will not be
once BUG-033 is fixed. The orphan-clean-up policy (delete vs quarantine) is
unsettled.

### BUG-A — the 1-core/1.8-GiB node class has no resource guard (session-only, unfixed)

*Evidence:* session `ses_ee234a16fffebAYvvTgksndBgE` (2026-10-06, "we have
BUG-A now"); recorded in the session log only, never in a TC report or the
CHANGELOG.

`nxt-sw-4-m` — 1 core, 1.8 GiB — was OOM-starved for about an hour by an
`opentelemetry/nxt-sw-4-m` container (JVM-class heap behaviour on a
small-node class). The sandbox's own node spec (`ses_f015e7886ffebPQLfTXFCjrRJx`
directives @43-46: 1 core / 1.8 GiB / 58 G) makes this node class *normal*, not
pathological. There is no admission control, no memory reservation and no
per-node capacity model anywhere in the codebase; the only related logic is
node *label*-based (`pmcluster.storage=true`, `stacks/placement.go:75
ResolvePlacement`).

**Question:** does pmcluster need a per-node capacity budget (soft reservations
per platform stack) before it can safely place platform services on the
smallest node class it officially supports?

---

## 2. Cancelled / unsolvable

### BUG-027 — per-host ACME behind a round-robin load balancer

*Evidence:* TC10 (`:317`, "BUG-027 cancelled as not fixable from our side");
TC10 environment notes (BYO self-signed cert on `nextrum-sy.de`).

Three hosts sit behind one round-robin LB. Let's Encrypt HTTP-01 requires the
*correct host* to answer `/.well-known/acme-challenge/…` on the *requesting
IP*. With round-robin, any of the three hosts may receive the validation
request; each host's Traefik answers for its own ACME state. This cannot be
fixed in pmcluster — it is a property of the LB. The production deployment
therefore uses BYO certificates.

**Open question:** is DNS-01 (per-host, via the IONOS API used for the S3
bucket) a viable replacement, or is BYO-the-permanent-policy? The certificate
rotation machinery (`cluster/secrets.go:105 EnsureVersionedSecret`, `:167/:188
GC`) is host-agnostic and would support either.

---

## 3. Stale documents (docs that describe superseded behaviour)

These were found by reading the docs *against* the code. They are listed here
rather than fixed, because the top-level `docs/*.md` files are read-only for
this RFC.

| Document | Says | Code says | Verdict |
|---|---|---|---|
| `docs/security-and-improvements.md:137-146` (§6) | SQLite runs in **WAL** mode and `WALCheckpointer` runs `PRAGMA wal_checkpoint(TRUNCATE)` before snapshots | `store/store.go:40-50` explicitly sets `journal_mode=DELETE` (WAL was reverted after BUG-004, the WAL split-brain corruption, TC3); `store/store.go:78-87` `WALCheckpoint` is now a **no-op** for compatibility | The §6 mechanism still exists as code (`backups/trigger.go` calls it), but its stated *purpose* — flushing WAL — cannot occur. A `DELETE`-mode checkpoint is a plain `sqlite3_backup`-equivalent; the doc describes a mode the store no longer uses. |
| `docs/storage-and-databases.md:168` | "SQLite + **Litestream** (Recommended) … streams WAL changes continuously to cheap S3-compatible object storage" | Litestream is not a dependency; the offsite path is offen's double-write (DEC-06, v0.2.162) and the mover's S3 transit (v0.2.174) | Recommendation predates the store decision chain (MinIO → Garage → kopia → SeaweedFS). Should be re-scoped as "historical option" or removed. |
| `docs/improvements.md` (platform-via-DSL) | Credits v0.2.152 with the conversion | v0.2.152 predates the tagged range; the claim cannot be verified from git tags (tags begin at `v0.2.158`) | Unverifiable claim; not contradicted, just unanchored. |

---

## 4. Known design costs (accepted, visible, unresolved)

### 4.1 Two escaping/translation implementations

`escapeCompose` (`manifest/compose_writer.go:626`, "doubles every literal `$`
so the value survives docker stack") and `escapeComposeDollar`
(`cluster/templates.go:583`) implement the same rule in two packages, called
from two render paths: the **user-manifest** path (`compose_writer.go`) and the
**platform-stack** path (`cluster/templates.go:504 LoadComposeFile`,
text/template first). v0.2.176 fixed the `$`-escaping bug in *both* (`$$`
doubling); nothing prevents the next escaping fix from landing in one and not
the other.

**Question:** should the platform path route through `manifest`'s writer, or
should both call a single `manifest.Escape`? The one-pipeline intent (DEC-02,
DEC-04) was never fully carried into the platform path.

### 4.2 The `stacks` → `cluster` dependency

`stacks/deploy.go:24` imports `internal/cluster`, and `deploy.go:78` takes a
`cluster.StackDeployer` interface. The dependency direction runs *up*: the
domain package (`stacks`) depends on the infrastructure package (`cluster`),
which per `pmcluster/internal/ARCHITECTURE.md` should depend on the domain,
not the reverse. The interface was extracted to soften the coupling (the
refactor directive: "isolate the docker client … even the output of dsl
translation should be isolated by interface"), but the *package* edge remains.

*Unverified:* this RFC could not find a **port-number** coupling between the
two packages (no shared port constant; `2377` appears only in CLI help text and
`preflight.go:71`). If "stacks→cluster port coupling" refers to something else
(a compose port mapping, an advertise-address port), it was not located and
this entry should be read as incomplete.

### 4.3 Secret masking exists in three places

Three independent implementations redact secret-ish settings:

1. `settings/http.go:30-56` — `maskSecrets`, HTTP API, matches keys containing
   `secret`/`password`/`key`/`token`, "mirroring the CLI's masking" (the
   comment says so itself);
2. the CLI's masking (referenced by that comment; found exercised in
   `cli/cli_runpaths_test.go:130 TestSettingsMaskSecrets`);
3. `cluster/cluster_state.go:81` — the snapshot path masks `sso_client_secret`
   ("plaintext in the store, masked in the …").

The key-matching rules are *similar but not identical* (the HTTP matcher is
documented as "extended"), and a new secret-ish key added in one path but not
the others leaks through the others. No test asserts cross-path consistency.

### 4.4 The store's domain sentinels are package-local

`store/*.go` defines per-entity sentinels — `ErrConfigNotFound`,
`ErrConfigExists`, `ErrConfigVersionNotFound` (`configs.go:51-58`),
`ErrRegistryNotFound/Exists` (`registries.go:24-27`), `ErrStackNotFound`,
`ErrRevisionNotFound` (`stacks.go:50-53`), `ErrCredentialNotFound`
(`credentials.go:26`), `ErrBackupNotFound` (`backups.go:24`) — plus a generic
`ErrNotFound` (`failover.go:43` …). Callers must therefore know which *entity*
they are matching, and the generic `ErrNotFound` coexists with the typed ones.
`errors.Is` works, but there is no single "not found" predicate for the
control loop's status classification (`reconcile.go` snapshot classification).

### 4.5 `storage_nodes` expansion re-pins every unpinned stack

Placement is resolved **once, at deploy time**, into a stack pin
(`stacks/placement.go:154` roundRobinNode → `:183 StackPinKey`). Adding a node
to `storage_nodes` therefore does *not* rebalance existing stacks — it only
affects future deploys. The operator-visible consequence: after scaling the
storage set, most stacks still live on the old nodes until each is explicitly
`stack move`d or redeployed. This is by design (pins are what make
`stack move` meaningful) but it surprises: "we added a node and nothing moved."

### 4.6 The leader-is-also-a-storage-node invariant has no graceful degradation

v0.2.170 (BUG-024) made the leader *always* a storage node; v0.2.171 makes
storage follow leadership (promote/demote on failover). The inverse case — a
cluster where no node can serve both roles (e.g. the sole storage node is
unreachable while a healthy leader exists) — currently resolves to
**no storage at all**, with the outage pause (`reconcile.go:423
tryStorageFailover` → `:340` pause log) as the visible symptom. Whether that
fail-closed posture is correct for every workload class is a policy question,
not a bug.

---

## 5. Verifiability gaps in the historical record

These are *record* gaps, not code gaps. They constrain what any future RFC (or
auditor) can claim.

1. **BUG-013 does not exist.** The TC1–TC9 register uses `BUG-001…BUG-018`;
   TC10 uses `BUG-03/05/06/09` (two-digit, *different* bugs), `BUG-019a/b` and
   `BUG-020…BUG-034`. The identifier `BUG-013` appears nowhere in the repo. The
   TC6 register jumps BUG-012 → BUG-014. Most likely a recording slip, but it
   means **one bug slot in the TC1–TC9 sequence has no write-up**.
2. **CHANGELOG gap v0.2.85 → v0.2.108**, plus duplicated v0.2.121/122/123
   entries. Releases in that window are unverifiable from the changelog alone.
3. **No git tags before `v0.2.158`.** "What exactly shipped in vX" for
   v0.2.42–v0.2.157 rests on CHANGELOG prose, not on annotated tags.
4. **BUG-A is session-only.** It appears in the opencode session log
   (`ses_ee234a16fffebAYvvTgksndBgE`) but in no TC report and no CHANGELOG
   entry. If the session log is lost, the incident leaves no trace.
5. **The rollout intervention on `nextrum-sy-2`** (`systemctl start` vs
   `restart`, Chapter 06 §4) is recorded in this RFC from the rollout session
   only; it is not in TC10's report, which predates the production rollout by
   design.

---

## 6. Questions the next RFC must answer

1. **Worker reconcile (BUG-033/034):** skip, degrade, or fix the orphan
   policy first? The answer determines whether standby daemons need a store at
   all.
2. **ACME behind an LB (BUG-027):** DNS-01 via the IONOS API, or BYO
   permanent?
3. **Node capacity (BUG-A):** introduce soft per-node reservations for platform
   services, or accept that the 1-core class must not host JVM-class workloads
   and enforce it as a placement guard?
4. **One render pipeline (§4.1):** collapse the platform path into `manifest`,
   or extract a shared `manifest.Escape`?
5. **Secret masking (§4.3):** one matcher, one package, one test that asserts
   all three paths agree?
6. **Store sentinels (§4.4):** a single `store.IsNotFound(err)` predicate for
   the control loop's classification, keeping the typed sentinels as
   implementation detail?
7. **Stale docs (§3):** who owns re-scoping `security-and-improvements.md` §6
   and the Litestream recommendation? They are the two places where a reader
   would still believe WAL is in use.

---

## 7. What this RFC could not verify

- The exact tag that first contained commit `e145d6f` (BUG-001's fix); TC1
  records it as "not in any tagged release yet" at test time.
- The v0.2.152 → platform-via-DSL attribution (pre-dates the tagged range).
- Whether "stacks→cluster port coupling" (the item this chapter's §4.2
  addresses) refers to the package edge alone or to a specific port mapping; no
  shared port constant was found.
- Any release between v0.2.85 and v0.2.108 (CHANGELOG gap).
- The content of `~/.claude/projects/-Users-hazemarian-Documents-mywork-poor-man-stack/`
  — explicitly out of scope for this RFC's four sources.

# pmcluster — Open improvement backlog

The **shipped** history lives in [`CHANGELOG.md`](../CHANGELOG.md) (v0.2.42 →
today) and in the RFC: the [decision log](rfc/03-decision-log.md) records
what was chosen and why, the [bug catalogue](rfc/04-bug-catalogue.md)
records what was broken and fixed, and
[ch. 07](rfc/07-limitations-and-open-questions.md) records the accepted
design costs. **Do not re-propose anything recorded there.**

This file tracks only what is still **open**, ordered by leverage ÷ effort.
Legend: ⏱ effort is focused developer time. **Quick win** = simple + high
value.

---

## 🔴 Large (schedule deliberately)

### L5. A per-node capacity model (from BUG-A)

The 1-core / 1.8-GiB node class has no resource guard anywhere: no admission
control, no memory reservation, no per-node capacity budget. A JVM-class
container starved a node for an hour (session-only incident, 2026-10-06).
Question to answer before building: do platform services need soft
reservations before they can be safely placed on the smallest officially
supported node class? See [RFC ch. 07 §1, BUG-A](rfc/07-limitations-and-open-questions.md).

### L6. Unify the two escaping implementations (a one-pipeline debt)

`escapeCompose` (`manifest/compose_writer.go`) and `escapeComposeDollar`
(`cluster/templates.go`) implement the same `$` → `$$` rule in two packages,
called from the user-manifest and platform-stack render paths. v0.2.176
fixed the escaping bug in both; nothing prevents the next fix landing in one
and not the other. The DEC-02 intent (one render pipeline) was never fully
carried into the platform path. See
[RFC ch. 07 §4.1](rfc/07-limitations-and-open-questions.md).

### L7. Single secret-masking rule

Secret-ish settings masking exists in three places (HTTP API matcher, CLI
matcher, snapshot path) with *similar but not identical* key rules, and no
test asserts cross-path consistency. A new secret-ish key added in one path
leaks through the others. See
[RFC ch. 07 §4.3](rfc/07-limitations-and-open-questions.md).

---

## 🟡 Medium

### M4. A single not-found predicate for the store

Per-entity sentinels (`ErrConfigNotFound`, `ErrStackNotFound`, …) coexist
with a generic `ErrNotFound`; callers must know which entity they are
matching, and the control loop's status classification has no single
"not found" predicate. See
[RFC ch. 07 §4.4](rfc/07-limitations-and-open-questions.md).

### M5. Orphan-clean-up policy for worker-local stack rows

BUG-034: a worker's local store retains stack rows whose manifests fail to
parse on every reconcile pass. Invisible today only because the worker's
loop is inert (BUG-033). Decide delete vs quarantine before BUG-033 lands.
See [RFC ch. 07 §1](rfc/07-limitations-and-open-questions.md).

---

## 🟢 Quick wins

### Q7. Docs: re-scope the historical storage options

`storage-and-databases.md` still recommends options that the platform
itself does not ship (Litestream) and describes superseded behaviour in
places — tracked as "stale documents" in
[RFC ch. 07 §3](rfc/07-limitations-and-open-questions.md). Sweep them.

### Q8. Fix BUG-033 (worker runs an inert reconcile loop)

A worker's daemon starts the reconciler but can never hold the leadership
lease — every pass pays the loop's cost and does nothing. Either skip the
reconciler on non-leaders or run a degraded read-only probe. See
[RFC ch. 07 §1, BUG-033](rfc/07-limitations-and-open-questions.md).

---

## Writer-portability notes (for when a second Writer lands)

The isolation seam (DSL → `BuildIR` → neutral `IR` → `Writer.Write`) keeps
intent in the IR and mechanism in the writer. A future Helm/Terraform writer
reads the same IR and emits its own mechanisms. Known trade-offs,
deliberately deferred (YAGNI):

- **depends_on ordering** is enforced by the control-plane deploy pipeline
  (swarm backend). A k8s writer would express the same IR intent as
  `initContainer` waits / Job sequencing — the IR `DependsOn` field is the
  contract.
- **`ComposeWriter.CertResolver` / `PinNode`** are writer configuration
  inputs (ACME resolver name; default placement node). A k8s writer would
  map them to ingress annotations / nodeSelector.
- **`escapeCompose`** (doubling literal `$`) is a compose-interpolation
  concern — a writer-local detail, not part of the IR.
- The IR no longer carries a service port (the old port-probe was removed
  with the wrapper) — port knowledge, when needed, is re-derived per writer.

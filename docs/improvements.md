# pmcluster — Improvement Backlog (with estimates)

Captured 2026-09-30, re-written with effort estimates. Ordered by **leverage ÷ effort** so the quick wins come first.
Legend: ⏱ effort is focused developer time. **Quick win** = simple + high value.

---

## 🟢 Quick wins (simple, do these first)

### Q1. UpdateStatus surfaced in the CLI (was #4)
- `pmcluster service list/ps` + `cluster status` show paused updates + failing task error (console pills already exist since v0.2.110; the data is already in `ServiceSummary.UpdateState/UpdateError` — only the tabular output is missing).
- ⏱ **~2 hours.** No new plumbing, pure CLI rendering + tests.
- **Quick win ✅**

### Q2. Registry credentials on join (was #3)
- `verifyRegistryAuth` (v0.2.110) already warns. Add: `--copy-registry-creds <manager-host>` flag (scp manager's `~/.docker/config.json` to the joining node) OR verify a real `docker pull` of a pinned image before declaring the node ready.
- ⏱ **~3-4 hours.** join.go has the hooks; needs an scp step + a pull-verify helper + tests.
- **Quick win ✅** — kills the "stale cached image" failure class at the source.

### Q3. Webhook delivery retry (was #6)
- `server_error` deliveries get a bounded retry (2 retries, 30s apart) before being recorded as failed; record retry count + final error.
- ⏱ **~3-4 hours.** receiver.go already records deliveries at every exit path — add a retry loop + migration column `retries`.
- **Quick win ✅** — transient docker/network hiccups stop burning CI pipelines.

### Q4. Per-stack API token scoping (was #5)
- Allow creating a token bound to one stack (only that stack's deploy/rollback/service routes pass). Webhooks/CI per repo use scoped tokens.
- ⏱ **~4-6 hours.** apikeys store gains a `stack` column + middleware checks route stack vs token stack + CLI/UI flag.
- **Quick win ✅** — closes the "one global token for every repo" hole.

### Q5. OO-side alerting metrics (was #12)
- Emit custom OTLP metrics from the daemon: reconcile drift, paused updates, backup failures, image staleness. Alerts built in OpenObserve (per decision m4031).
- ⏱ **~4-6 hours.** Add a small metrics emitter to the existing telemetry package + a few counter/updates in update/deploy/backup paths.
- **Quick win ✅** — enables real alerting with zero new infra.

### Q6. `pmcluster deploy --compose` single-node mode (was #8)
- Deploy a raw `docker-compose.yml` on a single-node swarm without the DSL (no placement/networks/secrets mapping) — bridge for legacy compose users.
- ⏱ **~5-6 hours.** New deploy path that skips manifest translation; validation pass; tests. Some edge cases (compose v3 vs swarm) need care.
- **Quick win ✅**

---

## ✅ Shipped (do not re-propose)

- **M1. Health-gated updates + auto-rollback** — superseded: the v0.2.118 depends_on **control-plane ordered deploys** (per-level topo-sort deploy + wait-healthy + prune-once) replaced the shell wait-wrapper; the v0.2.132 **control loop** continuously converges drift. Rollback re-translates source + records `rollback_of` + ordered per-level deploy.
- **M2. Backup restore completion** — shipped v0.2.136 (restore_s3): per-volume restore (`--volume`, stack-anchored), offsite S3/R2 fallback (stdlib SigV4, local-first, loud no-S3 error), `--from-s3` force.
- **M3. Tailscale option** — shipped v0.2.137: opt-in `--tailscale [--tailscale-auth-key]` on `join` / `cluster up` / `setup`, tailnet IPv4 advertised to the Swarm. Enabled live on the nextrum-sy nodes (100.82.72.107 / 100.77.123.88).
- **L1. Continuous reconcile loop — self-heal** — shipped v0.2.132 (control loop): leader-only reconcile loop (Swarm events + `reconcile_interval` safety tick, default 60s, `0` disables) re-deploys drifted platform stacks, auto-syncs drifted app stacks, and writes the `stack_status` DB health snapshot; badges read the snapshot (never live Docker). Intensive logging v0.2.134, OTLP traces v0.2.135.

---

## 🔴 Large (bigger projects, schedule deliberately)

### L2. Control-plane DB snapshot into Raft-replicated Docker config (was #7)
- Leader snapshots the failover-survivor kit into a `pmcluster_state` Docker config → replicated to all managers; `ensureControlPlaneFresh` restores from it. Security: split the encryption key into a second config.
- ⏱ **~1-2 days.** Snapshot serialization + config create/read + restore wiring + security split + tests. Removes the tarball/rsync dependency.

### L3. Interactive exec via websocket (was #9)
- Interactive shells via a websocket endpoint (or documented SSH fallback).
- ⏱ **~2-3 days.** Hijack handling through the edge proxy + console terminal UI + auth. Meaningful UI work.

---

## ✅ Shipped already (do not re-propose)

- depends_on real wait on Swarm — **replaced in v0.2.118** by control-plane ordered deploys (topo-sorted levels, deploy+wait per level, prune once; no rendered wrapper — works for every image). The v0.2.110–112 shell-wrapper approach is superseded.
- Stateful-aware defaults (auto stop-first + auto-pin for volume services) — v0.2.110
- Registry-auth warning + image-freshness pills — v0.2.110
- Paused updates surfaced in console — v0.2.110
- Secret edit swarm mirror + referenced-by backlinks — v0.2.110
- Inventory page, retag scope/stack from console, stack-aware config resolution — v0.2.108
- Run-once jobs shown as Complete (not Degraded) — v0.2.108/109.1
- Runtime `NEXT_*` env injection for Next.js apps (app-side pattern) — donation-campaign-frontend

---

## Writer-portability notes (for when a second Writer lands)

The isolation seam (DSL → `BuildIR` → neutral `IR` → `Writer.Write`) keeps intent in the
IR and mechanism in the writer. A future Helm/Terraform writer reads the same IR and
emits its own mechanisms. Known trade-offs, deliberately deferred (YAGNI):

- **depends_on ordering** is enforced by the control-plane deploy pipeline (swarm
  backend). A k8s writer would express the same IR intent as `initContainer` waits /
  Job sequencing — the IR `DependsOn` field is the contract.
- **`ComposeWriter.CertResolver` / `PinNode`** are writer configuration inputs (ACME
  resolver name; default placement node). A k8s writer would map them to ingress
  annotations / nodeSelector.
- **`escapeCompose`** (doubling literal `$`) is a compose-interpolation concern — a
  writer-local detail, not part of the IR.
- The IR no longer carries a service port (the old port-probe was removed with the
  wrapper) — port knowledge, when needed, is re-derived per writer.
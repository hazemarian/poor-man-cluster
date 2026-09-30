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

## 🟡 Medium effort (worth it, plan a session each)

### M1. Health-gated updates + auto-rollback (was #2)
- After deploy/sync, wait for all services healthy (bounded) before reporting success; on degradation auto-rollback to the previous revision (machinery exists in `stack_revisions`). Webhook delivery records `rollback` outcome.
- ⏱ **~1-2 days.** Reuse `WaitHealthyStacks` + `Rollback`; new wired orchestration in deploy.go + tests. Needs care: rollback must respect stop-first/pinning.

### M2. Backup restore completion (was #10)
- Per-volume restore, worker-node restore, `--from-s3` (S3-enabled agents), cross-host archive transport.
- ⏱ **~1-2 days.** browse.go/restore.go extension + S3 client + CLI flags + tests. Restore safety rules already exist.

### M3. Tailscale option (was #11)
- Optional Tailscale integration for control-plane/backup transport between nodes.
- ⏱ **~1 day.** Daemon + join option to join a tailnet; keep SSH key transport as fallback. Needs a working Tailscale account to test.

---

## 🔴 Large (bigger projects, schedule deliberately)

### L1. Continuous reconcile loop — self-heal (was #1)
- Daemon background loop re-rendering platform configs comparing `rendered_hash` + auto-sync app stacks by rendered hash + health self-heal (rate-limited force-update) + concurrency guard + backoff + `/health last_reconcile_at` + `pmcluster cluster reconcile`.
- ⏱ **~2-3 days.** New `internal/reconcile` package; extract the update.go reconcile step for reuse; app-stack auto-sync via existing Sync; health loop. The biggest payoff but the biggest surface.

### L2. Control-plane DB snapshot into Raft-replicated Docker config (was #7)
- Leader snapshots the failover-survivor kit into a `pmcluster_state` Docker config → replicated to all managers; `ensureControlPlaneFresh` restores from it. Security: split the encryption key into a second config.
- ⏱ **~1-2 days.** Snapshot serialization + config create/read + restore wiring + security split + tests. Removes the tarball/rsync dependency.

### L3. Interactive exec via websocket (was #9)
- Interactive shells via a websocket endpoint (or documented SSH fallback).
- ⏱ **~2-3 days.** Hijack handling through the edge proxy + console terminal UI + auth. Meaningful UI work.

---

## ✅ Shipped already (do not re-propose)

- depends_on real wait on Swarm (wait wrapper + port probe + on-failure retries) — v0.2.110–112
- Stateful-aware defaults (auto stop-first + auto-pin for volume services) — v0.2.110
- Registry-auth warning + image-freshness pills — v0.2.110
- Paused updates surfaced in console — v0.2.110
- Secret edit swarm mirror + referenced-by backlinks — v0.2.110
- Inventory page, retag scope/stack from console, stack-aware config resolution — v0.2.108
- Run-once jobs shown as Complete (not Degraded) — v0.2.108/109.1
- Runtime `NEXT_*` env injection for Next.js apps (app-side pattern) — donation-campaign-frontend
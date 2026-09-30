# pmcluster — Improvement Backlog

Captured 2026-09-30. Ideas for the control plane, ordered by leverage.
Not a promise to build — a record so nothing learned is lost.

## High priority

1. **Continuous reconcile loop (self-heal)**
   - Daemon background loop (interval setting, default 60s, 0 disables): re-render the
     7 platform configs, compare `rendered_hash`, redeploy drifted stacks
     (observability → infra → edge → backup → sso), `SetRendered`, drift-prune.
   - App stacks: re-translate each stack's latest stored `SourceYAML`, compare
     rendered hash vs latest revision → auto-sync (deploy) on change (the Sync button,
     automated).
   - Health self-heal: services with `Replicas < Desired` beyond a grace window get
     `docker service update --force` — rate-limited to avoid restart storms; respects
     placement/pinning (stateful services restart in place, never reschedule).
   - Concurrency guard (skip tick while running), backoff for repeatedly failing stacks.
   - `/health` + console gain `last_reconcile_at` + `last_reconcile_result`.
   - Manual trigger: `pmcluster cluster reconcile`.

2. **Health-gated updates + auto-rollback**
   - After a deploy/sync, wait for all the stack's services to be healthy (bounded)
     before reporting success; on degradation, auto-rollback to the previous revision
     (the revision machinery already exists in `stack_revisions`).
   - Webhook delivery records `rollback` as an outcome.

3. **Registry credentials on join (finish Point 3 of v0.2.110)**
   - `pmcluster join` currently only *warns* (`verifyRegistryAuth`) when
     `~/.docker/config.json` is missing/empty. Next step: offer to copy the manager's
     registry credentials to the joining node (scp `~/.docker/config.json`), or at
     minimum verify a real `docker pull` of a pinned image succeeds before the node is
     considered ready. Prevent the "7-month-old stale cached image" failure class at
     the source.

4. **UpdateStatus surfaced in the CLI, not just the console**
   - `pmcluster service list/ps` should show paused updates + the failing task error
     (v0.2.110 added console pills; mirror in CLI tabular output + `pmcluster cluster status`).

## Medium priority

5. **Per-stack API token scoping**
   - Tokens today are global. Allow creating a token bound to one stack (only that
     stack's deploy/rollback/service routes). Used by webhooks/CI per repo.

6. **Webhook delivery retry for transient failures**
   - `server_error` deliveries (e.g. temporary docker/network hiccup) get a bounded
     retry (e.g. 2 retries, 30s apart) before being recorded as failed. Record retry
     count + final error in `webhook_deliveries`.

7. **Control-plane DB snapshot into a Raft-replicated Docker config**
   - Leader periodically snapshots the failover-survivor kit (encryption key, managed
     credentials, settings, webhook secrets, API keys, stack pointers) into a
     `pmcluster_state` Docker config → replicated to all managers automatically.
   - On promotion, `ensureControlPlaneFresh` restores from that config instead of
     (or in addition to) the ctlplane tarball — removes the tarball/rsync dependency.
   - Security note: key + ciphertext in one config is readable by anyone with config
     read; split the key into a second config or keep `.encryption_key` synced out-of-band.

8. **`pmcluster deploy --compose` (single-node mode)**
   - Allow deploying a raw `docker-compose.yml` on a single-node swarm without the DSL
     (no placement/networks/secrets mapping) — a bridge for legacy compose users.

## Lower priority / design questions

9. **Interactive exec via websocket**
   - `service exec` is non-interactive over HTTP today. Interactive shells need a
     websocket endpoint or documented SSH fallback.

10. **Backup restore completion**
    - Per-volume restore, worker-node restore, `--from-s3` (S3-enabled agents),
      cross-host archive transport. `docs/restore-design.md` lists these as open gaps.

11. **Tailscale option**
    - Optional Tailscale integration for control-plane/backup transport between nodes
      (from the original RFC future-work list).

12. **OO-side alerting via custom metrics**
    - Decision (m4031): alerting lives in OpenObserve, not pmcluster. Improve emitted
      traces + custom metrics (reconcile drift, paused updates, backup failures,
      image staleness) so alerts can be built OO-side.

## Shipped already (do not re-propose)

- depends_on real wait on Swarm (wait wrapper + port probe + on-failure retries) — v0.2.110–112
- Stateful-aware defaults (auto stop-first + auto-pin for volume services) — v0.2.110
- Registry-auth warning + image-freshness pills on join — v0.2.110
- Paused updates surfaced in console — v0.2.110
- Secret edit swarm mirror + referenced-by backlinks — v0.2.110
- Inventory page, retag scope/stack from console, stack-aware config resolution — v0.2.108
- Run-once jobs shown as Complete (not Degraded) — v0.2.108/109.1
- Runtime `NEXT_*` env injection for Next.js apps (app-side pattern) — donation-campaign-frontend
# pmcluster — Improvement Backlog (with estimates)

Captured 2026-09-30, re-written with effort estimates. Ordered by **leverage ÷ effort** so the quick wins come first.
Legend: ⏱ effort is focused developer time. **Quick win** = simple + high value.

---

## ✅ Shipped (do not re-propose)

- **M1. Health-gated updates + auto-rollback** — superseded: the v0.2.118 depends_on **control-plane ordered deploys** (per-level topo-sort deploy + wait-healthy + prune-once) replaced the shell wait-wrapper; the v0.2.132 **control loop** continuously converges drift. Rollback re-translates source + records `rollback_of` + ordered per-level deploy.
- **M2. Backup restore completion** — shipped v0.2.136 (restore_s3): per-volume restore (`--volume`, stack-anchored), offsite S3/R2 fallback (stdlib SigV4, local-first, loud no-S3 error), `--from-s3` force.
- **M3. Tailscale option** — shipped v0.2.137: opt-in `--tailscale [--tailscale-auth-key]` on `join` / `cluster up` / `setup`, tailnet IPv4 advertised to the Swarm. Enabled live on the nextrum-sy nodes (100.82.72.107 / 100.77.123.88).
- **L1. Continuous reconcile loop — self-heal** — shipped v0.2.132 (control loop): leader-only reconcile loop (Swarm events + `reconcile_interval` safety tick, default 60s, `0` disables) re-deploys drifted platform stacks, auto-syncs drifted app stacks, and writes the `stack_status` DB health snapshot; badges read the snapshot (never live Docker). Intensive logging v0.2.134, OTLP traces v0.2.135.

---

## 🔴 Large (bigger projects, schedule deliberately)

### L2. Control-plane DB snapshot into Raft-replicated Docker config (was #7)
- ✅ **Shipped (v0.2.138; restore semantics tightened later).** The swarm leader snapshots the failover-survivor kit into `pmcluster_state_<ts>` Docker configs — replicated to every manager by Swarm's own Raft store, so no tarball/rsync shipping is needed. **Security split:** the AES-GCM encryption key goes in a second config family (`pmcluster_state_key_<ts>`), so key + ciphertext never share one blob; a restore refuses a state config whose key config is missing. Restore is **startup-only** (before the store is opened — restoring under a live SQLite connection split-brains the daemon); on promotion the leader only publishes a fresh snapshot. Worker/leaderless swarms degrade gracefully instead of crash-looping. Details in the changelog and `docs/control-loop-design.md`.

### L3. Interactive exec via websocket (was #9)
- ✅ **Shipped (v0.2.141).** Browser terminal (vendored xterm.js) → console websocket → daemon websocket → `docker exec -it` TTY hijack. Operator-role only, Bearer auth on the daemon, frame protocol (binary stdin/stdout, `resize`/`exit` JSON controls), Terminal button per service row. Details in `pmcluster/docs/service-ops-design.md` §8.

### L4. ONE pipeline for everything — unify secret/config versioning AND platform-via-DSL (combined directive)
- ✅ **Shipped (v0.2.152).** The consolidated directive — "combine everything together and do it in one go" + "the config of traefik and otelcollector templates handled the same" — is implemented:
  1. **Versioning unified, `_v<N>` unpadded everywhere** (user: keep the rev counter). Platform `EnsureConfig`/`EnsureVersionedSecret` mint `_v%d` (were `_v%03d`), matching the user `swarm_rev` counter. `cert_v1`, `pmcluster_otel_config_v1`, etc.
  2. **Platform-via-DSL:** the five platform stacks (`infra`/`edge`/`observability`/`backup`/`sso`) are now DSL manifests (`app.platform: true`) rendered through the SAME pipeline as app stacks (`LoadComposeFile` template pre-pass → `Parse → Interpolate → Validate → BuildIR → ComposeWriter`). One renderer/hash/deploy/reconcile/purge path. `io.pmcluster.platform=true` stamped on every platform service; user manifests are refused the flag.
  3. **Rendered-config primitive:** Traefik dynamic + OTel collector configs share one `RenderedConfig`/`EnsureRenderedConfigs` path — versioned/persisted/reconciled exactly like every other platform artifact.
  - DSL gaps filled: `mode: global`, node-label constraints, `settings(name)` env refs, conditional rendering, plus raw `constraints`/`binds`/`ports`/`configs` mounts/`extra_hosts`/`resources`/`user`/`labels`/`logging`/`start_period`/`restart_delay`, app-level `networks` + `volumes` map (plain platform volumes never relocated).
  - Migration: one forced platform redeploy on first `cluster update` (BUG-009 makes failures retry).

---

## ✅ Shipped already (do not re-propose)

- UpdateStatus surfaced in CLI — Q1: `service list/ps` + `cluster status` show paused updates + failing task errors (PAUSED: <error> markers)
- Registry credentials on join — Q2: `--copy-registry-creds` + `--verify-registry-pull` flags
- Webhook delivery retry — Q3: bounded deploy retry (default 2 × 30s), retry count recorded on the delivery row
- Per-stack API token scoping — Q4: `user create --stack <name>` → stack-scoped tokens, 403 guard on every other route
- OO-side alerting metrics — Q5: `pmcluster.services.paused` / `pmcluster.services.stale` gauges + `pmcluster.reconcile.total` counter via OTLP
- **Q6 (`deploy --compose` single-node) — DISCARDED** (was #8): raw compose deploys are out of scope; the DSL is the supported path.

- Storage placement + outage pause + `stack move` — v0.2.139 (P3/P4/P5): `storage_nodes` round-robin for stateful stacks, control-loop pause while a pinned storage node is down, `pmcluster stack move <stack> --to <node>`. Path 1 (pin + backup/restore) is the HA decision; LINSTOR/DRBD explicitly dropped.
- Interactive exec via websocket — v0.2.141 (L3): browser terminal into any service container (console→daemon WS relay, `docker exec -it` TTY), operator-role only.
- Control-plane DB snapshot into Raft-replicated Docker config — v0.2.138 (L2)
- depends_on real wait on Swarm — **replaced in v0.2.118** by control-plane ordered deploys (topo-sorted levels, deploy+wait per level, prune once; no rendered wrapper — works for every image). The v0.2.110–112 shell-wrapper approach is superseded.
- Stateful-aware defaults (auto stop-first + auto-pin for volume services) — v0.2.110
- Registry-auth warning + image-freshness pills — v0.2.110
- Paused updates surfaced in console — v0.2.110
- Secret edit swarm mirror + referenced-by backlinks — v0.2.110
- Inventory page, retag scope/stack from console, stack-aware config resolution — v0.2.108
- Run-once jobs shown as Complete (not Degraded) — v0.2.108/109.1
- Runtime `NEXT_*` env injection for Next.js apps (app-side pattern) — donation-campaign-frontend

---

## ✅ Shipped v0.2.150 → v0.2.179 (the current architecture)

- **Content-addressed configs/secrets (v0.2.155)** — every swarm config/secret is named `<base>_<sha256-first-8-hex>`; identical content reuses the object, a change mints a new one, and the numbered `_v<N>` increments are gone. The **DB is the index and also holds the value**: translate reads values from the swarm (swarm-first) and **rebuilds a missing object from the DB**; `cluster update` runs a rebuild-on-missing repair pass; `secret heal` verifies/re-mirrors every secret. Rotation is safe because the old, in-use swarm secret is immutable and stays mounted.
- **DSL additions** — `secret(name)` env ref (prints a value, vs `secrets(name)` which mounts a file) + `settings(name)` (v0.2.154/152); `expose.external` + `expose.mode` and TCP-only `ports` with `mode` (v0.2.153); `config_path(<name>)` file mounts with one syntax for app + platform stacks (v0.2.153); literal `$` escaped as `$$` in every rendered value (v0.2.176); `replicas: 0` rendered verbatim (v0.2.178).
- **In-cluster backup store** — MinIO (v0.2.160) was replaced by **SeaweedFS** (v0.2.160.1, MinIO images unpullable; production never ran MinIO): S3 :8333 published on the routing mesh, WebDAV :7333, pinned to a non-storage/non-platform node, `seaweedfs_admin` managed credential. The v0.2.160.1 `rclone` replicator sidecar was itself replaced (v0.2.172) by **offen's native double-write** (WebDAV to the store + AWS_* to the offsite target) — no replicator service exists any more.
- **Storage failover & lifecycle** — `node promote/demote`, console storage pills + Move form (v0.2.158); opt-in `storage_failover` with the failover marker, amber badge, `stack ack`, and hourly `backup_cron` default (v0.2.159); the leader is always a storage node and storage follows leadership (v0.2.170/171); store-transit mover — no cross-node host ports (v0.2.174); failover restores the *source node's* archive (v0.2.175); `backup restore` routes to the owning node and `--from-s3` really pulls offsite (v0.2.175).
- **Cluster lifecycle** — `cluster reset [--restore]`, self-healing/existence-aware `cluster update`, `cluster down --purge` writes a restorable `purge-backup-<ts>.tar.gz` before deleting the store (v0.2.161); `platform_node` defaults to the leader (v0.2.175).
- **Drift detection** — every service carries `io.pmcluster.rendered_hash` (stamped after a successful apply); `stackdrift.InSync` compares content-vs-live so a manual `docker service update` or half-applied deploy is re-applied even when the stored hash matches (v0.2.16x).
- **Platform/app separation (v0.2.177)** — `io.pmcluster.platform=true` surfaced end-to-end: a `platform` field on the services API, a read-only console **Platform page** (`/web/platform`) plus a separated panel on Services, platform stacks hidden from the stacks list, `service list --platform`.
- **Hardening (v0.2.176/178)** — daemon role tiers (admin/operator/viewer; migration 0025), RBAC on rendered configs, masked secret-ish settings for non-admin tokens, fast-reject garbage bearers, CSRF Origin/Referer guard, secure session cookie flag, vendored htmx, bcrypt 72-byte limit, random persisted console session secret, worker standby (no doomed reconcile spam), throttled sync-error + storage-pause logging, backup trigger retry, Traefik pinned to managers (the swarm provider needs the manager socket — fixes intermittent 404s through the routing mesh).
- **Brand identity (v0.2.179)** — six-palette `--brand-*` token system, a palette picker persisted before first paint, the inlined combination-mark logo; README lockup.

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
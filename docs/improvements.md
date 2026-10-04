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
- ✅ **Shipped (v0.2.138).** The swarm leader snapshots the failover-survivor kit into `pmcluster_state_<ts>` Docker configs — replicated to every manager by Swarm's own Raft store, so no tarball/rsync shipping is needed. `ensureControlPlaneFresh` restores from the config on promotion. **Security split:** the AES-GCM encryption key goes in a second config family (`pmcluster_state_key_<ts>`), so key + ciphertext never share one blob; a restore refuses a state config whose key config is missing. Details in the changelog.

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
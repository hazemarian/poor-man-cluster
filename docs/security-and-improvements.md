# Security Findings & Improvement Plan

This document outlines key security findings, architectural vulnerabilities, and reliability improvements for `pmcluster`. Each section includes the root cause, impacted files, and actionable steps for implementation.

> **Maintenance note:** Add a status line (implemented / outstanding) to each
> section as fixes land, so this file stays a living tracker.

---

## Security posture summary

| Area | Mechanism |
|---|---|
| TLS | Operator-provided cert/key or Let's Encrypt ACME; secrets `cert_vNNN`/`key_vNNN` in the swarm, Raft-replicated |
| Secret storage | AES-256-GCM encryption in SQLite (`managed_credentials`), mirrored to swarm secrets; Docker secrets are write-only, so content hashes are persisted in the store |
| Token auth | Argon2id-hashed API tokens (`pmc_<token_id>_<secret>`), indexed lookup |
| Webhooks | HMAC-SHA256 over `timestamp + body`, ±5 min window, uniform 401 |
| Edge rate limits | `pmcluster-edge` proxy: 200 req/s on `/api/*` (burst 400), 40 req/s on `/webhook/*` (burst 60), automatic IP ban after repeated failures |
| RBAC | Console roles admin > operator > viewer; `EDGE_LOGIN_DISABLED` mode runs as synthetic admin behind the Traefik gate |
| RealIP | XFF/X-Real-IP honored only from trusted CIDRs (`trustedRealIP`) |
| Auth gateway | GitHub SSO via oauth2-proxy (org-restricted) or Traefik basicAuth (htpasswd), gating `/web/*` + `observ.<domain>` |

---

## 1. Denial-of-Service in Token Lookup — ✅ Implemented

### Original Problem
In `pmcluster/internal/store/users.go`, the original `UserByToken` iterated through **all** registered user rows and executed `auth.VerifyToken` (Argon2id) on every single row until a match was found:

```go
func (s *Store) UserByToken(ctx context.Context, token string) (*auth.User, error) {
    rows, err := s.db.QueryContext(ctx, `SELECT id, name, token_hash FROM users`)
    ...
    for rows.Next() {
        ...
        ok, err := auth.VerifyToken(token, hash) // Runs Argon2id (64MB memory) per row
    }
}
```

Since Argon2id is intentionally CPU and memory heavy (64 MiB RAM per call), an unauthenticated attacker sending arbitrary invalid tokens would cause the daemon to evaluate Argon2id $N$ times per request (where $N$ is the number of stored users), leading to host CPU/memory exhaustion.

### What was implemented
- **v2 token format:** Generated API tokens now use `pmc_<token_id>_<secret>` where `token_id` is an 8-hex-char public lookup key.
- **Indexed lookup:** `UserByToken` extracts the `token_id` from v2 tokens and performs a `WHERE token_id = ?` indexed query, then runs Argon2id **once** against the single matched row.
- **Legacy fallback:** Pre-v2 tokens (no `pmc_` prefix) still work via the old O(N) scan path for backwards compatibility, but new tokens always use the fast path.
- **DB migration:** `migrations/0006_token_index.sql` adds the `token_id` column and a unique index.
- **Impacted files:**
  - `pmcluster/internal/auth/token.go` — `GenerateToken`, `SplitToken`
  - `pmcluster/internal/store/users.go` — `UserByToken`, `userByTokenID`, `userByTokenLegacy`
  - `pmcluster/internal/auth/middleware.go`

---

## 2. Webhook Replay Attack Vulnerability — ✅ Implemented

### Original Problem
The webhook receiver verified HMAC-SHA256 signatures over the payload body only:

```go
mac := hmac.New(sha256.New, secret)
mac.Write(body)
```

Without timestamps or nonces, an attacker who intercepted a legitimate request could replay it indefinitely.

### What was implemented
- **Required timestamp header:** `X-Pmcluster-Timestamp` (Unix seconds, decimal) is mandatory. Missing or unparseable timestamps produce a 401.
- **HMAC over `timestamp + body`:** The signature input is now the decimal timestamp string concatenated with the raw body bytes (no separator). The HMAC covers both.
- **5-minute tolerance window:** Requests with timestamps more than 5 minutes from the server clock (past or future) are rejected.
- **Constant-time comparison:** Digests are compared with `hmac.Equal`.
- **Impacted files:**
  - `pmcluster/internal/webhooks/receiver.go` — `verifyHMAC`, `TimestampHeader`, `MaxClockSkew`

---

## 3. Unvalidated `RealIP` Proxy Headers — ✅ Implemented

### Original Problem
In `pmcluster/internal/server/server.go`, chi's default `middleware.RealIP` was mounted at the top of the middleware stack:

```go
r.Use(middleware.RealIP)
```

`middleware.RealIP` blindly accepts `X-Forwarded-For` and `X-Real-IP` from any caller. If `pmcluster` receives requests directly (bypassing Traefik) or if Traefik passes untrusted upstream headers, callers can spoof client IP addresses in audit logs.

### What was implemented
- **Trusted Proxies:** Generic `middleware.RealIP` replaced with `trustedRealIP` (`pmcluster/internal/server/server.go:113`), which honors `X-Forwarded-For` / `X-Real-IP` **only from trusted CIDRs** (localhost and Docker network gateways). Requests from any other source use the direct peer address.
- **Impacted Files:**
  - `pmcluster/internal/server/server.go` — `trustedRealIP` mount + CIDR validation

---

## 4. Rate-Limiting Protection — ✅ Implemented (edge proxy)

### Original Problem
Neither `/api/*` nor `/webhook/{source}` enforce rate limits. An attacker or malfunctioning CI job can flood endpoints with requests.

### What was implemented
The `pmcluster-edge` proxy (the only public entry path behind Traefik) applies per-IP rate limits:
- **`/api/*`:** 200 req/s with a burst of 400.
- **`/webhook/*`:** 40 req/s with a burst of 60 — the path CI integrations hit.
- **Automatic IP ban** after repeated violations; banned IPs are served 403.
- The daemon itself (`127.0.0.1:9090`) is deliberately unthrottled — it is host-local by design (only reachable through the edge proxy or localhost).
- **Impacted Files:**
  - `pmcluster/internal/edgeproxy/` — rate limiter + ban list

---

## 5. Strict Mode for Pre-Deployment Backups — ✅ Implemented

### Original Problem
In `pmcluster/internal/stacks/deploy.go`, pre-deployment backups ran in "best-effort" mode:

```go
if app.BackupBeforeDeploy {
    s.runPreDeployBackup(ctx, app.Name, revision)
}
```

If the backup failed, the failure was logged, but `DeployStack` executed anyway. For critical applications, operators may prefer to fail the deployment if the pre-deploy backup fails.

### What was implemented
- **Manifest field:** `strict_backup: true` in the DSL top level (alongside `backup_before_deploy: true`). The `StrictBackup` boolean is in the `dsl.App` struct.
- **Abort logic:** When `strict_backup: true` and the pre-deploy backup returns an error, the deployment is aborted and the error propagates to the caller.
- **Impacted files:**
  - `pmcluster/pkg/dsl/types.go` — `StrictBackup` field
  - `pmcluster/internal/stacks/deploy.go` — pre-deploy backup abort

---

## 6. SQLite WAL Checkpoint Before Snapshot Backups — ✅ Implemented

### Original Problem
`offen/docker-volume-backup` creates tarball snapshots of Docker volumes. Since `pmcluster` operates SQLite in WAL mode (`journal_mode(WAL)`), taking a file-level snapshot while transactions are active in the WAL file can lead to incomplete database snapshots.

### What was implemented
- **Checkpoint Exec:** Before executing volume backups, `WALCheckpointer` (`pmcluster/internal/backups/trigger.go:75-93`) runs `PRAGMA wal_checkpoint(TRUNCATE)` on the SQLite connection so all transactions are flushed to the main `.db` file before the snapshot.
- **Impacted Files:**
  - `pmcluster/internal/backups/trigger.go` — `WALCheckpointer`
  - `pmcluster/internal/store/store.go` — WAL-mode connection

---

## Shipped since v0.2.4x — security-relevant additions

The sections below document hardening and auth changes that landed after the original tracker above was written.

### OpenObserve auto-auth surface
- Traefik's `openobserve-auto-auth` middleware injects the root-admin credential as an `Authorization: Basic …` header **and** a deterministic `auth_tokens` session cookie (`Set-Cookie`, 30-day) on every `observ.<domain>` response.
- The `/sso-bridge` page (served by the edge console on the `observ` origin) writes the `localStorage userInfo` entry the OO SPA needs, after the SSO gate.
- **Threat model:** the root credential is only reachable **behind** `admin-auth@file` / `sso-auth@file` — the middleware is never deployed unpaired (the template comment enforces this). If the Traefik gate is disabled, the auto-auth middleware must be removed too.
- Files: `pmcluster/internal/cluster/embeds/traefik-dynamic.yml`, `pmcluster/internal/cluster/templates.go` (`openObserveSessionCookie`), `pmcluster/internal/ui/controllers/sso_bridge.go`.

### Restore safety
- Control-plane archives (`pmcluster-ctlplane-*.tar.gz`) are **refused** into the volume root (`browse.go` — "control-plane archive: cannot restore %s into the volume root").
- Whole-disk restores strip the `/backup/data` prefix so files land at `<dest_root>/<app>/<vol>/…` — no accidental nesting.
- Files: `pmcluster/internal/backups/browse.go`.

### Leader-aware daemon + control-plane restore (v0.2.82)
- `pmcluster serve` only serves on the Swarm Raft **leader**; non-leader managers stay in standby (15 s poll), so only one control plane is ever active.
- On promotion, `ensureControlPlaneFresh` restores `~/.pmcluster` from the newest control-plane archive **only when the local DB is missing or older** — a current DB (e.g. shared storage / LINSTOR) is never clobbered.
- Files: `pmcluster/internal/cli/leader.go`.

### systemd unit managed by the CLI (v0.2.83/83.1)
- `cluster up`, `cluster update`, and `join` install and start `/etc/systemd/system/pmcluster.service`. The unit's `ExecStart` **must include `serve`** — a bare-binary unit crash-looped in the field; a regression test guards it (`daemonExecStart`).
- Files: `pmcluster/internal/cli/daemon.go`, `pmcluster/internal/cli/join_test.go`.

### EDGE_LOGIN_DISABLED semantics
- When the Traefik gate owns authentication (`EDGE_LOGIN_DISABLED=true`), the console runs as a **synthetic admin** — every `/web/*` request is treated as admin. This is intentional (the gate is SSO/basicAuth) but must be understood: there is no per-user console RBAC in that mode.
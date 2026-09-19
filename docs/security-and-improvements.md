# Security Findings & Improvement Plan

This document outlines key security findings, architectural vulnerabilities, and reliability improvements for `pmcluster`. Each section includes the root cause, impacted files, and actionable steps for implementation.

> **Maintenance note:** Add a status line (implemented / outstanding) to each
> section as fixes land, so this file stays a living tracker.

---

## 1. Denial-of-Service in Token Lookup — ✅ Implemented

### Original Problem
In [`store/users.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/store/users.go), the original `UserByToken` iterated through **all** registered user rows and executed `auth.VerifyToken` (Argon2id) on every single row until a match was found:

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
- **DB migration:** `migration/0006_token_index.sql` adds the `token_id` column and a unique index.
- **Impacted files:**
  - [`internal/auth/token.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/auth/token.go) — `GenerateToken`, `SplitToken`
  - [`internal/store/users.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/store/users.go) — `UserByToken`, `userByTokenID`, `userByTokenLegacy`
  - [`internal/auth/middleware.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/auth/middleware.go)

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
  - [`internal/webhooks/receiver.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/webhooks/receiver.go) — `verifyHMAC`, `TimestampHeader`, `MaxClockSkew`

---

## 3. Unvalidated `RealIP` Proxy Headers (Medium Priority — outstanding)

### Problem
In [`server/server.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/server/server.go#L40), chi's default `middleware.RealIP` is mounted at the top of the middleware stack:

```go
r.Use(middleware.RealIP)
```

`middleware.RealIP` blindly accepts `X-Forwarded-For` and `X-Real-IP` from any caller. If `pmcluster` receives requests directly (bypassing Traefik) or if Traefik passes untrusted upstream headers, callers can spoof client IP addresses in audit logs.

### Solution / Task Steps
- **Trusted Proxies:** Replace generic `middleware.RealIP` with header validation restricted to trusted proxy CIDRs (e.g. Docker network gateways or localhost `127.0.0.1`).
- **Impacted Files:**
  - [`internal/server/server.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/server/server.go)

---

## 4. Rate-Limiting Protection — Partially implemented (webhook edge rate limit)

### Original Problem
Neither `/api/*` nor `/webhook/{source}` enforce rate limits. An attacker or malfunctioning CI job can flood endpoints with requests.

### What has been implemented
- **Webhook edge rate limit:** The `pmcluster-edge` Traefik proxy applies a per-IP rate limit to `/webhook/*` (default 40 req/s, burst 60) with automatic IP blocking after repeated failures. This is the path CI integrations hit.
- **API rate limiting:** The `/api/*` endpoints still rely on Bearer auth and network-level controls; no application-level rate limiter is in place yet.

### Remaining work
- **Middleware:** Add a rate-limiting middleware for `/api/*` using `golang.org/x/time/rate` or a bucket rate-limiter.
- **Limits:** Set sane per-IP and per-source limits (e.g., 100 req/min for general API, 20 req/min for webhooks).
- **Impacted Files:**
  - [`internal/server/server.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/server/server.go)

---

## 5. Strict Mode for Pre-Deployment Backups — ✅ Implemented

### Original Problem
In [`deploy/deploy.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/deploy/deploy.go), pre-deployment backups ran in "best-effort" mode:

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
  - [`pkg/dsl/types.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/pkg/dsl/types.go) — `StrictBackup` field
  - [`internal/deploy/deploy.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/deploy/deploy.go)

---

## 6. SQLite WAL Checkpoint Before Snapshot Backups (Reliability Priority — outstanding)

### Problem
`offen/docker-volume-backup` creates tarball snapshots of Docker volumes. Since `pmcluster` operates SQLite in WAL mode (`journal_mode(WAL)`), taking a file-level snapshot while transactions are active in the WAL file can lead to incomplete database snapshots.

### Solution / Task Steps
- **Checkpoint Exec:** Before executing volume backups via `internal/backup`, execute `PRAGMA wal_checkpoint(TRUNCATE)` on the SQLite database connection to ensure all transactions are flushed to the main `.db` file.
- **Impacted Files:**
  - [`internal/store/store.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/store/store.go)
  - [`internal/backup/backup.go`](file:///Users/hazemarian/Documents/mywork/poor-man-stack/pmcluster/internal/backup/)

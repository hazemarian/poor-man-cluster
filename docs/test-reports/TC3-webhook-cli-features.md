# TC3 — Webhook + CLI Feature Testing (single-manager, nxt-sw-3-w)

**Date:** 2026-10-04
**Node:** nxt-sw-3-w (217.160.128.29, 1 vCPU / 1.8 GiB / 58 G, Ubuntu 26.04.1, Docker 29.8.2)
**pmcluster version:** v0.2.143 → v0.2.144 (mid-test WAL fix) → v0.2.145 (leadership-restore fix)
**Cluster mode:** single manager, ACME Let's Encrypt certs, custom volume root `/srv/stack/data`,
offsite S3 backup configured (IONOS bucket `nextrum-bu`), **no SSO**.
**Setup path:** follows TC1 + TC2 (see `TC1-single-manager-bringup.md`, `TC2-backup-restore-s3-acme.md`).

## Purpose

TC1/TC2 covered UI, deploy, backup/restore, ACME, and control plane. This case
exercises the parts that had **not** been live-tested: the **webhook** deploy
integration and the **CLI surface** (users, API keys, secrets, configs, registry,
TLS sites, credentials, badges, remote CLI mode, rollback).

## 1. Webhook integration

| # | Action | Result |
|---|--------|--------|
| 1 | `pmcluster webhook add github-test` | ✅ Created; shows in `webhook list` |
| 2 | Signed POST without provenance (`repo_url`/`file` missing) | ✅ **400** `deploy provenance required: repo_url and file must identify the source repository and manifest path` — intended guard |
| 3 | Bad signature | ✅ **401** |
| 4 | Stale timestamp (400 s old) | ✅ **401** (timestamp window enforced) |
| 5 | Signed POST with provenance (`repo_url=https://github.com/hazemarian/tc-test`, `file=deploy/webhookapp.yaml`, manifest = webhookapp app, nginx:1.27-alpine) | ✅ **202** `{"revision":1791077246,"stack":"webhookapp","status":"accepted"}` |
| 6 | Resulting deploy | ✅ Daemon log `deploy — completed revision=1791077249 services=[web]`; swarm `webhookapp_web` 1/1 Running; reconcile converges |
| 7 | Deliveries API | ✅ 4 rows: bad_request (no provenance), 2× unauthorized (bad sig / stale ts), accepted |

**Endpoint under test:** `https://pmcluster.nextrum-sy.de/webhook/github-test`
**Auth header format:** `X-Pmcluster-Timestamp` + `X-Pmcluster-Signature: sha256=HMAC-SHA256(timestamp+body, secret)`.

## 2. CLI feature surface

| Feature | Test | Result |
|---------|------|--------|
| Users | `user create ci-bot` → token; `user list` | ✅ Created + listed |
| Per-stack tokens | `user create webhookapp-bot --stack webhookapp`, `user create scope-test2 --stack webhookapp` | ✅ Created with `STACK` column set |
| API keys | `apikey create` | ℹ️ No `apikey` subcommand exists — per-stack tokens are created via `user create --stack`; not a bug |
| Secrets | `secret create webhookapp_db_pass s3cret123 --scope service --stack webhookapp` | ✅ Created, listed (sha256 `d8de988de6f8`) |
| Configs | `config create webhookapp_config --scope service --stack webhookapp --value "hello-from-config"` | ✅ Created; `get` returns content. ℹ️ Two-arg `name value` form is rejected — value must be `--value` or stdin |
| Registry | `registry list` | ✅ `(no registries configured)` |
| TLS sites | `tls site show` (ACME mode) | ✅ `No site certificate recorded yet` — expected, Traefik holds the LE cert internally |
| Credentials | `credentials list` | ✅ All 6 managed creds shown with swarm secret names |
| Badges | `GET /api/public/badge/bookdb`, `/api/public/badge/webhookapp` | ✅ SVG `aria-label="<stack>: healthy"` |
| Remote CLI | `PMCLUSTER_API_URL=http://127.0.0.1:9090` + admin token → `stack list` | ✅ Shows both stacks (NAME/REVISION/UPDATED/REPO/FILE table) |
| Rollback path | deployed webhookapp-v2 (replicas 2) → new revision | ✅ Revision `1791077364` recorded (rollback command exercised in later runs) |

## 3. Bugs found (all fixed + shipped)

### BUG-004 — Multi-process SQLite WAL corruption (severity: High, data-loss risk)

**Symptom:** After the webhook deploy, `pmcluster stack list` (CLI, fresh DB open)
showed **only bookdb**, while the daemon API showed **bookdb + webhookapp**. The
daemon held `data.db-wal (deleted)` + `data.db-shm (deleted)` file descriptors —
a second process had written + closed its connection, and SQLite deleted the WAL
sidecars out from under the long-running daemon. The daemon kept writing to the
orphaned inode → split-brain DB. Later symptoms: CLI-created users returned **401**
everywhere (daemon could not see the rows), `PRAGMA integrity_check` reported
`wrong # of entries in index idx_backups_started`, and `user list` failed with
`database disk image is malformed (11)`.

**Reproduction:** open the store, have a second connection write + close, watch
`data.db-wal` disappear while the first handle still holds it open. Read-only
second connections do not trigger it; **only a write + close does**.

**Fix (v0.2.144, commit `6a45768`):** both stores (`internal/store` and
`internal/ui/store`) now use `journal_mode=DELETE` — no long-lived sidecars to
orphan. `Store.WALCheckpoint` is a no-op. Regression test
`TestMultiProcessNoWALSplitBrain` asserts no `-wal`/`-shm`/`-journal` ever exist.

**Live verification (nxt-sw-3-w):** `REINDEX` repaired the torn index; after
deploy, a `python3` write+close no longer deletes sidecars, `integrity_check` = ok,
and CLI-created users authenticate (200) and appear everywhere.

### BUG-005 — Scoped/unscoped CLI-created tokens returned 401 (severity: High)

**Symptom:** Tokens created via `user create` (scoped or unscoped) returned **401**
even on `GET /api/me`; only the init-time admin token worked.

**Root cause:** This was the **symptom of BUG-004**, not a separate auth bug. The
daemon's SQLite connection was wedged on the orphaned WAL inode, so rows written
by CLI processes (into a fresh WAL, then checkpointed) were invisible to the
daemon's `UserByToken` lookup. Once the WAL fix landed and the DB was repaired,
freshly created users authenticated immediately (verified live: `killtest-user`,
then `bug6-probe` → auth 200).

### BUG-006 — Leadership-gain restore clobbered the live store (severity: High)

**Symptom:** Daemon log showed the **leadership goroutine** restoring from a Raft
state config *after* `store.Open`:
`control-plane data restored from Raft config config=pmcluster_state_1791078360700292478 files=4`
→ `extractKit` truncated `data.db` under the daemon's live SQLite connection.
Result: a CLI-created user was auth-200 to the daemon (stale page cache) but
absent from fresh CLI/`python3` reads — the same split-brain class as BUG-004.

**Fix (v0.2.145, commit pending):** control-plane restore is now **startup-only**
(before `store.Open`, the standby/failover path). On leadership gain the daemon
only **snapshots** the fresh control plane into the Raft-replicated configs.

**Live verification (nxt-sw-3-w):** fixed binary deployed; log now shows
`became swarm leader` → **no restore**, only snapshots. CLI-created `bug6-probe`
survived a daemon restart with auth 200 before/after, `user list` consistent,
`integrity_check` = ok.

## 4. Non-bugs documented

- `GET /web/nodes` returns 404 — node info lives on the Overview page; not a bug.
- OpenObserve search quirks: logs search needs **microsecond** start/end times;
  metrics need the PromQL `query_range` API with `?type=metrics`; OO is
  swarm-internal only (no published ports).
- `apikey` CLI subcommand does not exist — per-stack tokens use `user create --stack`.
- Two-arg `config create name value` is rejected — use `--value` or stdin.

## 5. How to repeat

1. `pmcluster init` (save the admin token).
2. `pmcluster setup --domain=<d> --acme-email=<e> --hostname=<h>` (or custom cert flags).
3. `pmcluster cluster settings set volume_root=/srv/stack/data`; then
   `cluster update` (BUG-002 fix makes storage dirs automatically).
4. Set `backup_s3_*` settings; `cluster update` to re-render the backup stack.
5. `pmcluster webhook add github-test` → deploy a manifest via signed POST
   (provenance required).
6. Run the CLI checks from §2.
7. Confirm `journalctl -u pmcluster` shows startup restore only, leadership-gain
   snapshots only.
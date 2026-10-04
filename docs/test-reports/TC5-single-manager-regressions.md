# TC5 — Single-Manager Regression Re-Tests (BUG-007, BUG-008, stack deletion)

- **Date:** 2026-10-04
- **Version under test:** v0.2.148 (commit 2b1c150)
- **Node:** nxt-sw-3-w (217.160.128.29, 1 vCPU / 1.8 GiB / 58 GB, Ubuntu 26.04.1, Docker 29.8.2)
- **Cluster:** single manager, ACME (Let's Encrypt) certs for `*.nextrum-sy.de`, `volume_root=/srv/stack/data`, S3 backup (IONOS `nextrum-bu`), node labeled `pmcluster.storage=true`
- **Purpose:** live re-verification of the two bugs fixed in v0.2.146 + the untested stack-deletion cleanup path
- **Result:** 3/3 PASS

---

## Test 1 — BUG-007 secret rotation (live re-test) ✅

**Setup:** `rotstack` stack (nginx:1.27-alpine) with `secrets: [rot_pass]` + `env ROT_PASS_VALUE: secrets(rot_pass)`; `pmcluster secret create rot_pass rot-secret-v1 --scope service --stack rotstack`.

**Action:** `pmcluster secret edit rot_pass --value rot-secret-v2` → redeploy.

**Verified:**
- CLI reports `+ mirrored to the Docker Swarm as rot_pass_v2`
- Swarm has **both** `rot_pass` (v1) and `rot_pass_v2` (v2) — the in-use original is never removed
- DB `swarm_rev = 2`
- After redeploy (rev 1791104699), running container reads `/run/secrets/rot_pass` → **`rot-secret-v2`**
- Container path unchanged (`ROT_PASS_VALUE=/run/secrets/rot_pass`)

**Conclusion:** rotation works live end-to-end — DB rev bump → versioned swarm secret `name_v2` → container gets the new value on redeploy.

**Manifest gotcha hit:** the service `secrets:` array must contain the ref name (`secrets(del_pw)` without a matching `secrets: [del_pw]` on the service fails validation: "is not mounted — add to the service secrets: array").

---

## Test 2 — BUG-008 webhook retry (live re-test) ✅

**Setup:** webhook source `github-test` (HMAC secret known), payload with a manifest that fails validation synchronously (missing `env`).

**Action:** signed POST to `/webhook/github-test` (through the public LB + direct to daemon).

**Verified (daemon log):**
- Run 1: deploy attempts at 09:12:26 → 09:12:56 → 09:13:26 (**3 attempts, exactly 30s apart** = initial + 2 retries)
- Run 2: 09:13:30 → 09:14:00 → ... (same pattern)
- Delivery row: `status=server_error, retries=2, error="validate: env: required (e.g. production, staging)"`
- HTTP 502 returned

**Conclusion:** the retry policy is live — synchronous deploy failures retried 2× (30s apart), real retry count recorded on the delivery row.

**Observation (not a functional failure):** the 502 body comes back empty through the public edge proxy. The handler blocks through the full retry sequence (~60s+ of 30s delays), which exceeds the daemon HTTP server's 60s WriteTimeout — the client connection is cut before the JSON body is flushed (delivery row still written correctly on the detached context). Direct curl to the daemon also timed out (~60s, HTTP 000). If a prompt 502 body matters, this could be improved (return 202 immediately + record outcome async) — currently the record is correct, only the response body is lost.

---

## Test 3 — Stack deletion cleanup ✅

**Setup:** `delstack` stack (postgres:14-alpine, named volume `db_data`, secret `del_pw`) deployed at rev 1791105433.

**Before delete:** service `delstack_db`, swarm secret `del_pw`, volume `delstack_db_data`, DB rows (stacks/secrets/revs) all present.

**Action:** `DELETE /api/stacks/delstack` (Bearer admin token).

**Verified after delete — everything removed:**
- Swarm service `delstack_db` — removed
- Swarm secret `del_pw` — removed
- Named volume `delstack_db_data` — removed
- DB rows — stacks: [], secrets: [], stack_revisions: 0, backups: 0

**Conclusion:** the Undeploy cleanup path (services → secrets → volumes → DB rows) works correctly.

---

## Summary

| Test | Feature | Result |
|---|---|---|
| 1 | BUG-007 secret rotation (versioned `_v2` swarm secret) | ✅ PASS |
| 2 | BUG-008 webhook retry (3 attempts, 30s apart, `retries:2` recorded) | ✅ PASS |
| 3 | Stack deletion cleanup (services/volumes/secrets/DB rows) | ✅ PASS |

**Open note:** the 502-response-latency artifact (empty body through proxy due to 60s WriteTimeout during the retry phase) is documented for future improvement; it does not affect correctness of the retry or the delivery record.
# Deploy Webhooks

`pmcluster` exposes an HMAC-verified webhook endpoint so CI systems (GitHub Actions, GitLab CI, Jenkins, …) can trigger a deploy without holding a bearer token. The HMAC signature **is** the authentication — the endpoint sits outside the bearer-protected `/api/*` subtree.

- **Endpoint:** `POST https://pmcluster.<your-domain>/webhook/{source}`
- **Auth:** HMAC-SHA256 shared secret (per named `source`)
- **Replay protection:** required timestamp header, 5-minute window

The webhook runs through `pmcluster-edge`, so it is subject to the edge's per-IP rate limit for `/webhook/*` (40 req/s, burst 60 by default) and automatic IP blocking after repeated failures.

---

## 1. Create a webhook source

A *source* is a named `(integration, secret)` pair. Create one per CI integration (e.g. one per repository or environment):

```bash
pmcluster webhook add github-prod
```

```
✅ Webhook source "github-prod" created.
🔑 Shared secret (shown once — save it now):

   a1b2c3…64 hex chars…

   POST /webhook/github-prod with X-Pmcluster-Signature + X-Pmcluster-Timestamp
```

The secret is 32 random bytes rendered as **64 hex characters**, shown **once**. Only an AES-GCM-encrypted copy is stored (in `~/.pmcluster/data.db`); it cannot be retrieved later. Store it as a CI secret (e.g. `PMCLUSTER_WEBHOOK_SECRET`).

Manage sources:

```bash
pmcluster webhook list                 # names + descriptions + last_used_at (never the secret)
pmcluster webhook add <source> --description "prod deploys from repo X"
pmcluster webhook remove <source>      # revokes the secret immediately
```

The same operations are available in the operator console (**Webhooks**) and over the REST API (`GET`/`POST /api/webhooks`, `DELETE /api/webhooks/{source}`).

### Delivery history

Every request the receiver handles — accepted or rejected — is recorded as a delivery with a status:

| Status | Meaning |
|--------|---------|
| `accepted` | signature + provenance OK; the deploy was queued |
| `unauthorized` | HMAC signature invalid / timestamp outside the ±5 minute window |
| `bad_request` | provenance missing (`repo_url` + `file`) or body malformed |
| `server_error` | deploy pipeline failed (e.g. invalid manifest) |

View history per source:

```bash
pmcluster webhook deliveries <source> --limit 50     # newest first: ID/STATUS/STACK/REVISION/REPO/FILE/WHEN/ERROR
```

The same list is available in the operator console (**Webhooks → History**) and over the REST API (`GET /api/webhooks/{source}/deliveries?limit=50`).

> **Troubleshooting tip:** if CI reports a failed deploy, check `pmcluster webhook deliveries <source>` first — a `bad_request` or `unauthorized` status tells you whether to fix the payload (provenance) or the signing before touching the app. Note that a webhook deploy always mints a **new revision** even if nothing changed — CI-side deduplication is the caller's job.

---

## 2. Request format

### Headers

| Header | Required | Value |
|--------|----------|-------|
| `Content-Type` | yes | `application/json` |
| `X-Pmcluster-Timestamp` | yes | current Unix time in **seconds** (decimal) |
| `X-Pmcluster-Signature` | yes | `sha256=<lowercase hex>` |

### Body

The body is a JSON `DeployPayload`:

```json
{
  "app_name": "donation-campaign",
  "version": "v1.2.3",
  "manifest": "app: donation-campaign\nenv: production\n…",
  "repo_url": "https://github.com/acme/donation-campaign",
  "file": "deploy/donation-campaign.yaml"
}
```

| Field | Required | Notes |
|-------|----------|-------|
| `app_name` | yes | stack name; must match the manifest's `app` |
| `version` | yes | image tag to deploy |
| `manifest` | yes | the **entire manifest YAML as a string** (not a nested object) |
| `repo_url` | no | metadata, recorded with the revision |
| `file` | no | manifest path inside the source repo, recorded with the revision (provenance) |

### Signature

Compute HMAC-SHA256 over the concatenation of the **decimal timestamp string** and the **raw body bytes** — no separator:

```
signature_input = <timestamp> + <body>
signature       = HMAC-SHA256(secret, signature_input)   # hex-encode
```

The header value is `sha256=` + lowercase hex digest.

```bash
TIMESTAMP=$(date +%s)
SIG=$(printf '%s' "${TIMESTAMP}${BODY}" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print "sha256=" $2}')
```

> **Important:** sign the *exact bytes you send*. Any reformatting of `$BODY` between signing and sending (re-serializing JSON, stripping whitespace) invalidates the signature.

### Replay protection

The HMAC covers the timestamp, and the server rejects any request whose timestamp is more than **5 minutes** away from its own clock (past or future). This prevents an attacker who captures a request from replaying it later.

---

## 3. GitHub Actions example

The production pattern: build the JSON body with `jq`, sign `timestamp + body`, POST, and fail the job on a non-2xx response.

```yaml
name: Deploy

on:
  push:
    branches: [main]
    paths:
      - "deploy/donation-campaign.yaml"

jobs:
  deploy:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5

      - name: Deploy manifest via pmcluster webhook
        env:
          PMCLUSTER_WEBHOOK_URL: ${{ secrets.PMCLUSTER_WEBHOOK_URL }}
          PMCLUSTER_WEBHOOK_SECRET: ${{ secrets.PMCLUSTER_WEBHOOK_SECRET }}
        run: |
          FILE="deploy/donation-campaign.yaml"

          MANIFEST_CONTENT=$(cat "${FILE}")
          APP=$(printf '%s' "${MANIFEST_CONTENT}" | grep '^app:' | awk '{print $2}')
          VERSION=$(printf '%s' "${MANIFEST_CONTENT}" | grep '^version:' | awk '{print $2}')

          BODY=$(jq -n \
            --arg app_name "${APP}" \
            --arg version "${VERSION}" \
            --arg manifest "${MANIFEST_CONTENT}" \
            '{app_name: $app_name, version: $version, manifest: $manifest}')

          TIMESTAMP=$(date +%s)
          SIG=$(printf '%s' "${TIMESTAMP}${BODY}" \
            | openssl dgst -sha256 -hmac "$PMCLUSTER_WEBHOOK_SECRET" \
            | awk '{print "sha256=" $2}')

          HTTP_CODE=$(curl -s -o /tmp/webhook_response.txt -w "%{http_code}" \
            --connect-timeout 10 --max-time 30 \
            -X POST "$PMCLUSTER_WEBHOOK_URL" \
            -H "X-Pmcluster-Timestamp: $TIMESTAMP" \
            -H "X-Pmcluster-Signature: $SIG" \
            -H "Content-Type: application/json" \
            -d "$BODY")

          echo "HTTP ${HTTP_CODE}:"; cat /tmp/webhook_response.txt

          if [ "$HTTP_CODE" != "200" ]; then
            echo "Deploy webhook failed for ${APP}: HTTP ${HTTP_CODE}"
            exit 1
          fi
```

Configure two repository secrets:

- `PMCLUSTER_WEBHOOK_URL` — the full URL, e.g. `https://pmcluster.example.com/webhook/github-prod`
- `PMCLUSTER_WEBHOOK_SECRET` — the 64-char hex secret from `pmcluster webhook add`

> Only add `-k` to `curl` if your certificate is not yet trusted by the runner; in normal operation the cert should validate.

---

## 4. Generic CI / shell example

```bash
SOURCE="github-prod"
URL="https://pmcluster.example.com/webhook/${SOURCE}"
SECRET="$PMCLUSTER_WEBHOOK_SECRET"
MANIFEST_FILE="deploy/donation-campaign.yaml"

BODY=$(jq -n \
  --arg app_name "donation-campaign" \
  --arg version "$GIT_SHA" \
  --arg manifest "$(cat "$MANIFEST_FILE")" \
  '{app_name: $app_name, version: $version, manifest: $manifest}')

TIMESTAMP=$(date +%s)
SIG=$(printf '%s' "${TIMESTAMP}${BODY}" \
  | openssl dgst -sha256 -hmac "$SECRET" \
  | awk '{print "sha256=" $2}')

curl -fsS -X POST "$URL" \
  -H "X-Pmcluster-Timestamp: $TIMESTAMP" \
  -H "X-Pmcluster-Signature: $SIG" \
  -H "Content-Type: application/json" \
  -d "$BODY"
```

---

## 5. Responses

| Status | Meaning |
|--------|---------|
| `200 OK` | Deploy accepted — `{"stack": "<name>", "revision": <unix-ts>}` |
| `400 Bad Request` | Missing `source`, malformed JSON, or manifest validation failure |
| `401 Unauthorized` | **Any** HMAC failure — bad secret, unknown source, missing/invalid signature, stale/missing timestamp |
| `413 Payload Too Large` | Body exceeds 1 MB |
| `502 Bad Gateway` | `docker stack deploy` failed (see the error message in the body) |
| `429 Too Many Requests` | Edge rate limit exceeded — honor `Retry-After: 1` |

**All 401s are identical.** The server deliberately returns the same status and body (`{"error":"unauthorized"}`) for every authentication failure mode so an attacker cannot probe which part is wrong. Check `pmcluster webhook list` for `last_used_at` to confirm whether a request reached the source at all — that field is updated as soon as the HMAC verifies, even if the deploy then fails.

---

## 6. Testing locally

Sign and send against the daemon directly (the daemon listens on `127.0.0.1:9090`):

```bash
SECRET="<your hex secret>"
BODY='{"app_name":"demo","version":"v1","manifest":"app: demo\nenv: prod\ndomain: example.com\nservices:\n  web:\n    image: nginx\n"}'
TS=$(date +%s)
SIG=$(printf '%s' "${TS}${BODY}" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print "sha256=" $2}')

curl -sS -X POST http://127.0.0.1:9090/webhook/github-prod \
  -H "X-Pmcluster-Timestamp: $TS" \
  -H "X-Pmcluster-Signature: $SIG" \
  -H "Content-Type: application/json" \
  -d "$BODY"
```

---

## 7. Troubleshooting

**401 Unauthorized**

1. **Missing/invalid `X-Pmcluster-Timestamp`** — required; must be current Unix seconds (decimal) within ±5 min. CI runners with a skewed clock will fail — ensure NTP.
2. **Wrong HMAC input** — the signature is over `timestamp + body` with **no separator**, using the decimal timestamp exactly as sent in the header.
3. **Body mutated after signing** — sign the exact bytes you POST. Watch for `jq` re-serialization or shell trailing-newline differences (`printf '%s'` not `echo`).
4. **Wrong secret or source** — the secret is shown once; if lost, `pmcluster webhook remove <source>` and `pmcluster webhook add <source>` to get a new one (and update the CI secret).
5. **Wrong signature format** — must be `sha256=` followed by 64 lowercase hex chars.

**429 Too Many Requests**

The edge proxy is throttling this IP. Space out requests or raise `WEBHOOK_RATE`/`WEBHOOK_BURST` in the edge stack configuration.

**403 Forbidden**

The source IP has been auto-banned by the edge after repeated rate-limit trips or upstream auth/5xx failures. Bans are in-memory and expire after the configured `BAN_DURATION` (default 10 min).

**Deploy succeeded at the edge but the stack didn't change**

Check `pmcluster stack list` / `pmcluster stack show <name>`. A `200` means the payload passed validation and the deploy ran; a `502` includes the Docker error in the response body. Verify the manifest's `app` matches `app_name`.

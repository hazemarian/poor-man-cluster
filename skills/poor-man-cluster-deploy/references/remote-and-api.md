<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Remote CLI mode (run off-node)

Every data command can run against the daemon API instead of a local
`~/.pmcluster` + Docker socket. Set the API base URL and a bearer token:

```bash
export PMCLUSTER_API_URL=https://pmcluster.example.com   # or --api-url
export PMCLUSTER_API_TOKEN=pmc_<id>_<secret>                # or --api-token

# Now all of these work from any machine:
pmcluster config list
pmcluster secret list --scope service
pmcluster deploy app.yaml
pmcluster stack list
pmcluster rollback my-app 1789626082
pmcluster webhook list
pmcluster webhook deliveries github-prod
pmcluster user create ci-bot
pmcluster backup create
pmcluster backup browse <id>
pmcluster backup restore <id> [--volume <name>] [--from-s3]
pmcluster tls site show
pmcluster cluster settings
pmcluster usage
```

Restore is **local-first**: an archive still on the node restores from disk; when
it is gone the archive is fetched from the configured offsite S3 store
(`backup_s3_*` settings, R2-compatible SigV4) — `--from-s3` forces the fetch.
`--volume <name>` restores a single volume (anchored to the run's stack;
whole-disk runs accept `<app>/<volume>`). With no local archive and no S3
configured the command fails loudly and names where the archive lives.

```bash
pmcluster join --role worker --token <TOKEN> --manager <MANAGER>:2377 \
  --tailscale --tailscale-auth-key tskey-...   # opt-in tailnet advertise-addr
```

The **control loop** (leader daemon) reconciles in the background:
`reconcile_interval` (default 60s, `0` disables) + Swarm events. Each pass
re-deploys drifted platform stacks, syncs app stacks whose rendered hash
drifted, and writes a health snapshot to `stack_status` — the source for the
public **status badges** at `https://pmcluster.<domain>/api/public/badge/<stack>`
(and `/<stack>/services` for the combined per-service badge), which you can
paste into any GitHub README.

- The token comes from `pmcluster user create <name>` (or the `edge` daemon
  token from `pmcluster credentials show edge_api_token`).
- **Local-only** (need the node's SQLite + Docker socket): `init`,
  `cluster up/update/status/down`, `credentials *`, `node`, `registry`,
  `logs`, `serve`.
- Remote `tls hosts add` uploads PEM text and refreshes Traefik through the
  daemon; remote `config edit` re-stamps the current binary version, then run
  the console **Apply to swarm** (or `cluster update` on the node) to reach
  the swarm.


## REST API Deploy (Remote / CI)

When deploying from CI or a remote machine, use the REST API at the public origin:

```bash
BASE=https://pmcluster.example.com

# Deploy a manifest
curl -X POST $BASE/api/stacks \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d "{\"app_name\":\"my-app\",\"version\":\"v1.2.3\",\"manifest\": $(jq -Rs . < manifest.yaml)}"

# List stacks
curl $BASE/api/stacks -H "Authorization: Bearer <admin-token>"

# Show a stack
curl $BASE/api/stacks/my-app -H "Authorization: Bearer <admin-token>"

# Rollback
curl -X POST $BASE/api/stacks/my-app/rollback \
  -H "Authorization: Bearer <admin-token>" \
  -H "Content-Type: application/json" \
  -d '{"revision": 1719000000}'

# Remove a stack (services, named volumes, mounted secrets, record + configs/secrets)
curl -X DELETE $BASE/api/stacks/my-app -H "Authorization: Bearer <admin-token>"
```

Other endpoints: `GET /api/cluster/info`, `GET /api/nodes`, `GET /api/stacks/{name}/revisions/{rev}`, `GET|POST /api/backups`, `GET /api/backups/{id}/files`, `POST /api/backups/{id}/restore`, `GET|POST|DELETE /api/tls/hosts`, `GET|POST|DELETE /api/webhooks`, `GET /api/webhooks/{source}/deliveries`, `GET|POST /api/api_keys`, `GET /api/cluster/settings`, `PUT /api/cluster/settings`, `GET /api/usage`. Full spec in `pmcluster/docs/openapi.yaml`. The console's stacks list has a **Delete** button (with confirmation) per stack — same as `DELETE /api/stacks/{name}`.


## CI/CD Webhook Setup

Webhooks are authenticated via HMAC-SHA256, not Bearer tokens. Each webhook source has a shared secret; the CI system computes a signature over the request and pmcluster verifies it server-side. **A timestamp header is required** for replay protection.

### 1. Create a webhook source on the manager

```bash
pmcluster webhook add github-prod
# Prints a 64-char hex secret ONCE — save it.
```

### 2. Configure CI (e.g., GitHub Actions)

Two headers are required on every webhook request:

| Header                   | Value                                    |
|--------------------------|------------------------------------------|
| `X-Pmcluster-Timestamp`  | Unix seconds of the request              |
| `X-Pmcluster-Signature`  | `sha256=<hex>` HMAC over timestamp+body  |

**The HMAC input is**: `timestamp_as_decimal_string + body` (no separator). Timestamps more than 5 minutes old (or from the future) are rejected.

Production pattern — build the body with `jq`, sign `timestamp + body`, POST, and fail the job on a non-200:

```yaml
name: Deploy
on:
  push:
    branches: [main]
    paths: ["deploy/my-app.yaml"]
jobs:
  deploy:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5
      - name: Deploy via pmcluster webhook
        env:
          PMCLUSTER_WEBHOOK_URL: ${{ secrets.PMCLUSTER_WEBHOOK_URL }}
          PMCLUSTER_WEBHOOK_SECRET: ${{ secrets.PMCLUSTER_WEBHOOK_SECRET }}
        run: |
          FILE="deploy/my-app.yaml"
          MANIFEST_CONTENT=$(cat "${FILE}")
          APP=$(printf '%s' "${MANIFEST_CONTENT}" | grep '^app:' | awk '{print $2}')
          VERSION=$(printf '%s' "${MANIFEST_CONTENT}" | grep '^version:' | awk '{print $2}')

          BODY=$(jq -n --arg app_name "${APP}" --arg version "${VERSION}" \
            --arg manifest "${MANIFEST_CONTENT}" \
            '{app_name: $app_name, version: $version, manifest: $manifest}')

          TIMESTAMP=$(date +%s)
          SIG=$(printf '%s' "${TIMESTAMP}${BODY}" \
            | openssl dgst -sha256 -hmac "$PMCLUSTER_WEBHOOK_SECRET" | awk '{print "sha256=" $2}')

          HTTP_CODE=$(curl -s -o /tmp/resp.txt -w "%{http_code}" \
            --connect-timeout 10 --max-time 30 \
            -X POST "$PMCLUSTER_WEBHOOK_URL" \
            -H "X-Pmcluster-Timestamp: $TIMESTAMP" \
            -H "X-Pmcluster-Signature: $SIG" \
            -H "Content-Type: application/json" \
            -d "$BODY")

          echo "HTTP ${HTTP_CODE}:"; cat /tmp/resp.txt
          [ "$HTTP_CODE" = "200" ] || { echo "deploy failed"; exit 1; }
```

The payload is `{"app_name","version","manifest","repo_url"?}` where `manifest` is the **entire manifest YAML as a string**.

### 3. Responses

| Status | Meaning |
|--------|---------|
| `200` | Deploy accepted → `{"stack","revision"}` |
| `400` | Missing source, bad JSON, or manifest validation failure |
| `401` | Any HMAC failure (bad secret, unknown source, bad/missing timestamp, bad signature) |
| `413` | Body > 1 MB |
| `502` | `docker stack deploy` failed |
| `429` | Edge rate limit — honor `Retry-After: 1` |

All 401s are identical by design — no information leak. Use `pmcluster webhook list` to see `last_used_at`.

List webhooks: `pmcluster webhook list` · Remove: `pmcluster webhook remove github-prod`

Full guide: [`docs/webhook.md`](https://github.com/hazemarian/poor-man-cluster/blob/main/docs/webhook.md).

### CI workflows — two things that break naive watchers

- **A tag build creates no `docs` CI run.** Only a push triggers the `docs` workflow; pushing a `v*` tag runs `release.yml` alone. A release watcher that waits for a `docs=success` check on a tag will wait forever. Gate release readiness on the tag's own workflow (or the last push to `main`), not on a `docs` run that will never appear.
- **`swarm-e2e` can fail intermittently on an ordering race, not a real fault.** `docker stack deploy` attaches networks **by name** and skips creating one if a same-named network is merely *listed* — so a leftover `backup_net` still being removed asynchronously gets listed, creation is skipped, and the name is gone by the time the service is created. That surfaces as `network backup_net not found`. It is fixed by ensuring per-stack overlays before deploy, but if you see that exact string on an older run, it is the race, not a broken stack.

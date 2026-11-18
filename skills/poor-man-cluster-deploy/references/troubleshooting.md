<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Troubleshooting

### "Swarm not active"
Run `docker swarm init --advertise-addr <ip>` on the manager. Pmcluster never initializes Swarm for you.

### "Docker not reachable"
Ensure Docker engine is running and the current user has permission (`docker info`).

### "secret already exists"
`pmcluster cluster up` is init-only — it refuses to run on an already-initialised cluster. To reconcile an existing cluster, use `pmcluster cluster update` (content-aware, idempotent).

### "cluster already initialised"
`cluster up` is init-only — it refuses to run when a cluster already exists. Use `pmcluster cluster update` instead.

### "no such stack"
Run `pmcluster stack list` to see deployed stacks. Stack names come from the `app` field in manifests.

### Deploy fails
Check the Swarm service logs: `docker service logs my-app_api --tail=50`. Inspect the translated compose from the latest revision: `pmcluster stack show my-app`.

### Deploy blocked by strict backup failure
If `strict_backup: true` is set and the pre-deploy backup fails, the deploy aborts. Check `pmcluster backup list`. Fix the backup issue or remove `strict_backup: true`.

### Worker node can't pull images
Ensure `pmcluster serve` is running (it replays registry credentials). Verify with `pmcluster registry list`.

### Webhook returns 401
Common causes:
1. **Missing/incorrect `X-Pmcluster-Timestamp`** — required; current unix seconds within ±5 min. Ensure CI clocks are NTP-synced.
2. **Wrong HMAC input** — signature is over `timestamp + body` (no separator), using the timestamp exactly as sent.
3. **Body mutated after signing** — sign the exact bytes you POST (`printf '%s'`, not `echo`; avoid re-serializing JSON).
4. **Wrong webhook secret** — recreate with `pmcluster webhook remove <source> && pmcluster webhook add <source>` and update the CI secret.

### Rate limited (HTTP 429) or banned (HTTP 403)
The edge proxy throttles per real IP (`/api/*` 200/s, `/webhook/*` 40/s). Space out bursts. A 403 means the IP was auto-banned after repeated trips/failures; bans expire (default 10 min).

### Console login fails
Get the password with `pmcluster credentials show edge_admin`. If the `pmui-data` volume was wiped, the console re-reads the Swarm secret on next boot.

### After SSO-related sso-stack/edge redeploy: initial 404/403
After enabling or updating SSO, Traefik label convergence takes ~45–60 seconds. Initial 404/403 errors on `pmcluster.<domain>` or `observ.<domain>` are expected during this window. Wait and retry.

### oauth2-proxy not proxying (SSO enabled but login loop)
Ensure `OAUTH2_PROXY_COOKIE_DOMAINS` and `OAUTH2_PROXY_WHITELIST_DOMAINS` are **plural** (the singular forms are silently ignored in oauth2-proxy v7). Also verify `OAUTH2_PROXY_REVERSE_PROXY: "true"` is set and the edge container has `trusted-proxy-ip` / `TRUSTED_PROXY_CIDRS` configured for hardening.

### Edge image not updating after upgrade
The edge image tag is pinned to the release version (`ghcr.io/hazemarian/pmcluster-edge:<version>`). If the tag doesn't match, override with `PMCLUSTER_EDGE_IMAGE=<tag>`. Always use the release → binary upgrade (`hack/upgrade-nodes.sh`, or `install.sh` on a single node) → `cluster update` flow rather than `docker service update --image` directly.


## Verifying the MCP surface after a deploy

`initialize` and `tools/list` answer without touching the router, so they prove nothing about whether
reads work. Verify with a real `tools/call` for something that dispatches:

```bash
ssh root@<leader> 'bash -s' <<'REMOTE'
OUT=$(pmcluster user create mcp-check 2>&1); TOK=$(echo "$OUT" | grep -oE 'pmc_[A-Za-z0-9_.=-]+' | head -1)
H=$(mktemp); U=http://127.0.0.1:9090/api/mcp
curl -s -D "$H" -o /dev/null -X POST $U -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}'
SID=$(grep -i '^mcp-session-id:' "$H" | tr -d '\r' | awk '{print $2}' | head -1)
curl -s -o /dev/null -X POST $U -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -H "Mcp-Session-Id: $SID" -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
curl -s -X POST $U -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pmc_list_services","arguments":{}}}'
pmcluster user remove mcp-check >/dev/null 2>&1
REMOTE
```

Expect real service names in the payload and `isError` absent. `"HTTP 405: "` or `"HTTP 500"` means
the in-process dispatch is broken — see the chi trap below.


## Trap: in-process dispatch reuses the caller's chi route context

A tool call runs inside the request that matched `/api/mcp`, so its context already carries chi's
`RouteCtxKey` with `RoutePath = "/api/mcp"`. chi's `Mux.ServeHTTP` checks for exactly that key and, when
present, **delegates to its handler without routing at all**:

```go
rctx, _ := r.Context().Value(RouteCtxKey).(*Context)
if rctx != nil { mx.handler.ServeHTTP(w, r); return }
```

Re-entering the router with the inherited context therefore re-dispatches the sub-request as `/api/mcp`:
curated reads get 400/405 from the MCP handler re-entering itself, derived reads crash it (recovered
panic, logged as a bare 500 with no stack). Fix: dispatch with the same context but the key overridden to
a typed nil — `context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))` — which makes chi take its
fresh-route path while keeping cancellation and deadlines.

Whatever the transport, test the dispatch: a `tools/call` through the router built by `server.New`. A stub
router with no chi in it cannot reproduce this, and neither can `initialize`/`tools/list`.

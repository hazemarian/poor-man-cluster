<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## Authentication & API Tokens

pmcluster uses Bearer tokens for API authentication. Tokens are generated once by `pmcluster init` and printed to stdout — save them immediately; they cannot be recovered.

### Token Format

As of pmcluster v2, tokens use a structured format:

```
pmc_<hex_token_id>_<base64_secret>
```

- `pmc_` — fixed prefix identifying v2 tokens
- `hex_token_id` — 8 hex chars (4 random bytes), the public index used for fast database lookup
- `base64_secret` — 32 random bytes, base64url-encoded (~43 chars); this is the sensitive part

### Creating additional users

```bash
pmcluster user create my-user     # prints the token ONCE — save it
pmcluster user list               # id, name, created (never token material)
pmcluster user remove my-user     # revoke a token immediately (refuses "edge")
```

All API requests pass the token as a Bearer header. Against the public origin:

```bash
curl -H "Authorization: Bearer pmc_a1b2c3d4_..." https://pmcluster.example.com/api/me
# Daemon-local (e.g. on the manager host):
curl -H "Authorization: Bearer pmc_a1b2c3d4_..." http://127.0.0.1:9090/api/me
```

### Rate Limiting

The `pmcluster-edge` proxy enforces per-real-IP rate limits:

| Path prefix   | Limit                |
|---------------|----------------------|
| `/api/*`      | 200 req/s, 400 burst |
| `/webhook/*`  | 40 req/s, 60 burst   |
| `/health`     | unlimited            |

Exceeding the limit returns HTTP 429 with `Retry-After: 1`. Repeated rate-limit trips or upstream auth/5xx failures can also get the IP auto-banned (HTTP 403) for a short window (default 10 min).


## Credentials

```bash
pmcluster credentials list                 # all managed bootstrap passwords
pmcluster credentials show openobserve_admin  # show a specific one
pmcluster credentials show edge_admin      # the operator console login password
pmcluster credentials rotate openobserve_admin # generate + apply a new password
```

Managed credentials: `traefik_dashboard`, `openobserve_admin`, `edge_admin`, `edge_ui_secret`, `edge_api_token`, `sso_cookie_secret` (SSO only). Edge credentials are persisted by the console on first boot, so `rotate` refuses them.

To extract just the password value: `pmcluster credentials show edge_admin | awk -F": *" '/^password/{print $2}'`.


## Audit Logs

```bash
pmcluster logs --tail=200                  # recent JSON log lines
pmcluster logs --since=24h --follow        # stream in real time
pmcluster logs --tail=200 | jq 'select(.level=="error")'
```

Log files live at `~/.pmcluster/logs/pmcluster-YYYY-MM-DD.log` and auto-sweep after 14 days.

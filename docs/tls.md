# TLS Certificates

pmcluster manages two kinds of certificates:

1. **The cluster's own (main) certificate** — the wildcard serving
   `*.<your-domain>`. Either Let's Encrypt (ACME) or operator-supplied.
2. **Per-host certificates** — for customer domains your apps serve on top
   of the cluster (each with its own real cert).

Both share one table and one flow: the PEM bytes become **content-addressed**
Swarm secrets, the metadata (expiry, SANs, hashes) lands in the `site_certs`
DB table, and nothing is ever written to the manager's filesystem.

## Modes for the main certificate

- **Let's Encrypt (`cluster up --acme-email=…`)** — Traefik handles issuance
  and renewal automatically (HTTP-01 via the `:80` entrypoint). Certs are
  stored in the `traefik_acme` Docker volume on the managers. Nothing to
  rotate manually.
  - Caveat: each Traefik instance runs its own ACME challenge listener —
    behind a load balancer that may route the challenge to a *different*
    manager, use an operator-supplied certificate instead (see
    [`network-topology.md`](network-topology.md)).
- **Operator-supplied (`cluster up --cert=… --key=…`)** — any CA, including a
  wildcard from your existing internal/purchased PKI. The PEM pair is stored
  in the DB and materialized as content-addressed Swarm secrets
  (`cert_<sha8>` / `key_<sha8>`, mounted at their versioned paths).
- Switching an existing cluster between the modes requires
  `--force-tls-mode`.

## Renewing the main certificate in place

The main certificate rotates without a full bring-up, from any of the three
entry points — the result is identical: the pair is validated against the
cluster domain, stored in the DB, materialized as content-addressed Swarm
secrets, wired into the Traefik dynamic config, and its metadata (validity
window, SANs, hashes) recorded in the DB for expiry monitoring.

```bash
# CLI — status + upload
pmcluster tls site show                                    # domain, expiry, SANs, secret names, hashes
pmcluster tls site set --cert-file new.pem --key-file new.key.pem

# REST API
curl -X PUT https://pmcluster.<domain>/api/tls/site \
     -H "Authorization: Bearer ***" -H 'Content-Type: application/json' \
     -d "$(jq -n --rawfile c new.pem --rawfile k new.key.pem '{cert:$c,key:$k}')"
```

The operator console exposes the same flow on the **TLS** page (a *Main
certificate* card above the per-host list), including the current expiry.

Certificates within **30 days** of expiry are flagged in the console and
warned about on every `cluster up`/`cluster update` — operator certs are not
auto-renewed, so this is the reminder to rotate. (ACME-mode clusters renew
automatically and have no such row.)

## Per-host TLS (customer domains)

An app can be reachable on a pmcluster subdomain *and* on a customer's own
domain, each over HTTPS with its own real certificate. Add the extra hostname
as an `expose.aliases` entry in the manifest (that creates the Traefik
router), then supply the matching certificate:

```bash
# Store a cert+key as text (paste inline) or from files:
pmcluster tls hosts add api.customer.com --cert-file cert.pem --key-file key.pem
pmcluster tls hosts add api.customer.com --cert "$(cat cert.pem)" --key "$(cat key.pem)"

pmcluster tls hosts list                    # host, expiry, SANs
pmcluster tls hosts remove api.customer.com
```

Per-host certs use the same table and flow as the main certificate: the PEM
bytes become **content-addressed** Swarm secrets
(`hostcert-<host>_<sha8>` / `hostkey-<host>_<sha8>` — identical content
reuses the object, a rotation mints a new one) and the metadata is recorded
in the `site_certs` DB table — never written to the manager's filesystem.
Adding or removing one re-renders the Traefik dynamic config and re-deploys
the infra stack so it takes effect immediately; pass `--no-refresh` to defer
the refresh to the next `pmcluster cluster update`. The cluster's own
wildcard certificate (`cluster up --cert/--key`) is never touched by these
commands.

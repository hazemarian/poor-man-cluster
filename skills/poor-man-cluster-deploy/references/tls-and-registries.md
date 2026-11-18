<!-- moved out of the SKILL.md body on the 2026-11 docs pass; content verbatim -->

## TLS Certificates

There are two independent certificate flows — the cluster's own (main) certificate and per-host certificates for customer domains. Both validate the PEM pair, materialize versioned Swarm secrets and refresh Traefik; only the target differs.

### Main certificate (the cluster's own domain)

Rotate the cluster's own certificate in place — no full bring-up:

```bash
pmcluster tls site show                                    # domain, expiry, SANs, secret names, hashes
pmcluster tls site set --cert-file new.pem --key-file new.key.pem
# or inline: pmcluster tls site set --cert "$(cat new.pem)" --key "$(cat new.key.pem)"
```

The pair is validated against the persisted cluster domain, written to `~/.pmcluster/config/site/{cert,key}.pem` (0600), materialized as `cert_vN`/`key_vN` Swarm secrets, wired into the Traefik dynamic config, and its metadata recorded in the DB. Same flow via the console TLS page (**Main certificate** card) or `PUT /api/tls/site`.

Certificates within **30 days** of expiry are flagged in the console and warned about on every `cluster up`/`cluster update` — operator certs are not auto-renewed. (ACME-mode clusters renew automatically; no row is recorded.)

### Per-host TLS (customer domains)

Serve the same app on a customer's own domain with its own certificate. Per-host certs use the **same table and flow as the main certificate** — the PEM bytes become versioned Swarm secrets and the metadata lands in the same `site_certs` DB table (the cluster's own domain is just one row among them). The only difference is at Traefik render time: the main-domain row feeds the default-cert block, every other row becomes an extra `tls.certificates` entry referencing its own secrets (`hostcert-<host>_vNNN` / `hostkey-<host>_vNNN`). Per-host certs are **never written to the manager's filesystem**.

1. Add the hostname to the service's `expose.aliases` in the manifest (creates the Traefik router).
2. Store the matching cert+key:

```bash
pmcluster tls hosts add api.customer.com --cert-file cert.pem --key-file key.pem
# or inline text:
pmcluster tls hosts add api.customer.com --cert "$(cat cert.pem)" --key "$(cat key.pem)"

pmcluster tls hosts list                      # host, expiry, SANs, secret names
pmcluster tls hosts remove api.customer.com
```

Adding/removing re-renders Traefik and re-deploys the infra stack; use `--no-refresh` to defer to `pmcluster cluster update`. The cluster's own wildcard cert is never touched.


## Private Registries

If your images are in a private registry (e.g. `ghcr.io`):

```bash
# Interactive (prompts for password):
pmcluster registry add ghcr.io

# Scripted (no shell history leak):
echo "$GITHUB_PAT" | pmcluster registry add ghcr.io --username myuser --password-stdin

# At install time (one-shot):
curl -fsSL .../install.sh | PMCLUSTER_REGISTRY="ghcr.io=myuser=$GITHUB_PAT" bash

pmcluster registry list                   # verify
```

`pmcluster serve` auto-replays `docker login` for all persisted registries on startup, so worker nodes can pull private images even after a manager rebuild.

**Registry auth on join:** `pmcluster join` warns when the host has no Docker registry credentials (`~/.docker/config.json` missing or empty) — without them, private images silently fall back to whatever stale cached copy exists. Authenticate before deploying private images: `docker login ghcr.io --username <user>` (a copy of the manager's config is sufficient).

**Image freshness + paused updates in the console:** the services/stacks pages show a `⚠ Image N days old` pill when the node is running a stale cached image (registry auth missing is the usual cause), and an `Update paused` banner with the failing task's error when Swarm pauses a rollout — check the Tasks tab for the failing task.

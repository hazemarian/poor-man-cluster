-- 0009_site_certs.sql — main site SSL certificate metadata.
--
-- The operator uploads the cluster's own domain certificate (cert+key) via
-- the console, CLI, or API. The PEM bytes are materialized as versioned
-- Swarm secrets (cert_vN / key_vN) and wired into the Traefik dynamic config
-- exactly like the `cluster up --cert/--key` flow — this table stores only
-- METADATA (domain, validity window, hashes, secret names) so the console can
-- show the current certificate, its expiry, and warn before it lapses. The
-- DB is the single source of truth for "what's the site cert today".

CREATE TABLE site_certs (
    domain      TEXT PRIMARY KEY,      -- the cluster domain (e.g. nextrum-sy.com)
    cert_secret TEXT NOT NULL,         -- Swarm secret name holding the PEM (cert_vN)
    key_secret  TEXT NOT NULL,         -- Swarm secret name holding the key (key_vN)
    not_before  INTEGER NOT NULL,      -- unix seconds, certificate validity start
    not_after   INTEGER NOT NULL,      -- unix seconds, certificate expiry (drives warnings)
    sans        TEXT NOT NULL DEFAULT '',  -- comma-separated SANs for display
    cert_hash   TEXT NOT NULL,         -- sha256 of the cert PEM
    key_hash    TEXT NOT NULL,         -- sha256 of the key PEM
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;
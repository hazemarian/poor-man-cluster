-- 0002_credentials.sql — managed credentials for bundled stacks (Phase 2).
--
-- pmcluster generates random passwords for components it bundles
-- (Traefik dashboard, OpenObserve, edge console) on first `cluster up`,
-- stores them encrypted here, AND mirrors them to Docker Swarm secrets
-- so the services can consume them.
--
-- name              — pmcluster's internal identifier
--                     (e.g. "traefik_dashboard", "openobserve_admin")
-- kind              — short tag for the consumer ("traefik", "openobserve")
-- username          — plaintext (not sensitive on its own)
-- password_ciphertext — AES-256-GCM ciphertext (nonce || sealed)
-- swarm_secret_name — the Docker Swarm secret that holds the same value
-- created_at        — unix seconds, set on insert
-- rotated_at        — unix seconds, updated on rotate (NULL until first rotation)

CREATE TABLE managed_credentials (
    name                TEXT    PRIMARY KEY,
    kind                TEXT    NOT NULL,
    username            TEXT    NOT NULL,
    password_ciphertext BLOB    NOT NULL,
    swarm_secret_name   TEXT    NOT NULL,
    created_at          INTEGER NOT NULL,
    rotated_at          INTEGER
) STRICT;

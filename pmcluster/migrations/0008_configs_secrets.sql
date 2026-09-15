-- 0008_configs_secrets.sql — DB-backed configs & secrets.
--
-- pmcluster moves its runtime configuration and secret material into the
-- database so the control plane is reproducible from data.db alone
-- (cert source files on disk remain the only operator-owned files).
--
-- Two scopes:
--   'cluster'  — platform-owned: traefik_dynamic, otel, edge/backup stacks,
--                site cert/key, per-host certs, portainer/edge/OO creds.
--   'service'  — app-owned: user-created configs and secrets referenced in
--                the deploy DSL via env: X: config(name) / secrets(name).
--
-- configs.content is the editable template (cluster level) or arbitrary
-- config content (service level). Every successful update appends the
-- previous content to config_versions so operators can roll back. `version`
-- records the pmcluster build version that last wrote the row (the upgrade
-- rule: build version > row.version  →  re-render from the embedded template).
--
-- secrets.payload holds AES-256-GCM ciphertext (nonce || seal) encrypted with
-- ~/.pmcluster/.encryption_key; hash is sha256(plaintext) for display and
-- verification without ever revealing the value.

CREATE TABLE configs (
    id         INTEGER PRIMARY KEY,
    scope      TEXT    NOT NULL CHECK (scope IN ('cluster','service')),
    name       TEXT    NOT NULL UNIQUE,
    kind       TEXT    NOT NULL CHECK (kind IN ('template','file','env')),
    content    TEXT    NOT NULL,
    version    TEXT    NOT NULL,
    hash       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE config_versions (
    id         INTEGER PRIMARY KEY,
    config_id  INTEGER NOT NULL REFERENCES configs(id) ON DELETE CASCADE,
    content    TEXT    NOT NULL,
    hash       TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_config_versions_config_id ON config_versions(config_id);

CREATE TABLE secrets (
    id         INTEGER PRIMARY KEY,
    scope      TEXT    NOT NULL CHECK (scope IN ('cluster','service')),
    name       TEXT    NOT NULL UNIQUE,
    payload    BLOB    NOT NULL,
    hash       TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
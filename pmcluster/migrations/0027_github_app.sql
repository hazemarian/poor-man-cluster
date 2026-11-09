-- 0027_github_app.sql — GitHub App credentials and the Actions token broker.
--
-- Why an app at all: nine repos carried a personal OAuth token (GH_PAT) to run
-- `go mod download` against private modules. When that token expired, every one
-- of them broke silently. A GitHub App installation token is short-lived (one
-- hour), minted on demand by this daemon, and revocable by uninstalling the app
-- — none of which is true of a personal token copied into nine secret stores.
--
-- github_app is a singleton (id = 1): one app per cluster. The private key is
-- AES-256-GCM ciphertext like every other credential; the DERIVED installation
-- token is never written here or anywhere else on disk.
--
-- probe_repo / probe_package name what `pmcluster doctor` and the console's
-- Test button should read to prove the installation actually has
-- contents:read and packages:read. Without a target the only thing that can be
-- proven is that a token was minted, which is not the same claim.
CREATE TABLE IF NOT EXISTS github_app (
    id                     INTEGER PRIMARY KEY CHECK (id = 1),
    app_id                 INTEGER NOT NULL,
    installation_id        INTEGER NOT NULL DEFAULT 0,
    private_key_ciphertext BLOB    NOT NULL,
    probe_repo             TEXT    NOT NULL DEFAULT '',
    probe_package          TEXT    NOT NULL DEFAULT '',
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL
) STRICT;

-- One HMAC shared secret per repository, for POST /api/github/token.
--
-- A workflow must hold SOMETHING to authenticate the mint call. It cannot be a
-- cluster bearer token: that is full control-plane access in every repo. This
-- secret can only mint an installation token, is scoped to one repo name, and
-- is revoked by deleting its row — independent of the App private key.
CREATE TABLE IF NOT EXISTS github_broker_secrets (
    repo              TEXT PRIMARY KEY,
    secret_ciphertext BLOB    NOT NULL,
    description       TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL,
    last_used_at      INTEGER
) STRICT;

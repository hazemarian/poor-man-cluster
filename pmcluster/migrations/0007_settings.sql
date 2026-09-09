-- 0007_settings.sql — persistent cluster-install settings.
--
-- pmcluster records the install-time inputs it can't otherwise discover from
-- Docker (TLS mode and paths, domain, OpenObserve admin email) plus the last
-- versioned configs/certs it provisioned. `cluster up` reads this on a re-run
-- to stay idempotent; `cluster update` uses it to know what's currently
-- mounted. Pure key/value store so future settings don't need new columns.

CREATE TABLE cluster_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

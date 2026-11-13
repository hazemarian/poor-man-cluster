-- 0030_cluster_version.sql — the version a follower mirrors the cluster state by.
--
-- One row. It is bumped INSIDE the same transaction as every write to a table a
-- follower mirrors (cluster_settings, secrets, users), and the leader serves it on
-- GET /internal/api/cluster/snapshot as both the body's `version` and its ETag.
-- Two consequences worth stating where the schema is:
--
--   * an unchanged version answers 304, so a follower that is already current
--     copies no rows at all — the endpoint is a cheap poll, not a download;
--   * a row that changed without a bump is invisible to every follower until some
--     unrelated write bumps the counter (possibly never). That is why the bump
--     shares the writer's transaction instead of being a second statement: a
--     failed write must not advance the version, and a committed write must.
--
-- generated_at is stamped by the STORE, at the moment of the bump — never by the
-- HTTP handler at read time. One clock, one stamp: two requests for the same
-- version report the same generated_at, so the age a follower shows is the age of
-- the DATA rather than the age of its last poll. (Stamping at read time is how the
-- cache-report work produced reports that read as stale the instant they were
-- written.)
--
-- One row, and CHECK (id = 1) rather than a key/value table: a second counter is
-- the failure mode this replaces — two writers answering "which version is
-- current" disagree, and a follower oscillates between two answers that both look
-- authoritative.
CREATE TABLE cluster_version (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    version      INTEGER NOT NULL,
    generated_at INTEGER NOT NULL
) STRICT;

-- Seed row. A store that never takes another write still answers a version, so
-- the endpoint's first response is a normal 200 with an ETag rather than an error
-- about a row nobody created.
INSERT INTO cluster_version (id, version, generated_at)
VALUES (1, 1, CAST(strftime('%s', 'now') AS INTEGER));

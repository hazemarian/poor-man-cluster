-- 0031_sync_state.sql — the follower's mirror: when a row arrived, and which
-- version this node last applied.
--
-- Slice 2 of docs/adr/0003-cluster-state-by-pull.md. A follower manager polls
-- /internal/api/cluster/snapshot and applies the whole projection in one
-- transaction; these columns are how it can answer "is what I am serving
-- current, and when did it get here".
--
-- mirrored_at is the row's OWN arrival time, stamped by the node that applied it.
-- Two questions need two different clocks and this is the one the earlier
-- cache-report work got wrong:
--
--   * "did this row arrive, and when" — mirrored_at, this node's clock, per row.
--   * "which leader's version is this" — sync_state below, and its generated_at
--     carries the LEADER's stamp as provenance only. Age is computed from
--     applied_at (this node's clock), never by subtracting a remote timestamp
--     from a local one: the two clocks differ, and a report that mixes them reads
--     as stale the instant it is written.
--
-- A mirrored row is never (0): the apply stamps every row it writes, so a 0 means
-- "this row was created here and has never been confirmed against the leader" —
-- which is precisely the state that must not be presented as current.
ALTER TABLE cluster_settings ADD COLUMN mirrored_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE secrets          ADD COLUMN mirrored_at INTEGER NOT NULL DEFAULT 0;
-- users is mirrored too (the API-key table), so it gets the same answer. The
-- brief named settings and secrets; the projection carries three tables, and a
-- mirrored row that cannot answer "did you arrive" is the defect this column
-- exists to prevent.
ALTER TABLE users            ADD COLUMN mirrored_at INTEGER NOT NULL DEFAULT 0;

-- One row per node, never replicated: it says which leader's state THIS node has
-- applied. It has no seed row on purpose — "never synced" is a real state, and its
-- absence is how a read surface says "no age yet" instead of a zero age, which
-- would read as "extremely fresh".
--
-- `version` + `generated_at` together identify the state applied; only that pair
-- (not `applied_at`) can reveal that this node is behind — or that it is holding a
-- deletion the leader never sent. `leader` is recorded because a version number is
-- only meaningful relative to the store that produced it: after a leadership
-- change the same number can describe different content, and the ETag this node
-- sends back must name the pair it applied.
CREATE TABLE sync_state (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    leader       TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    generated_at INTEGER NOT NULL,
    applied_at   INTEGER NOT NULL
) STRICT;

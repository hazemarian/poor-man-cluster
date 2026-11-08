-- 0026_node_stats.sql
-- Per-node resource visibility for the console overview.
--
-- ONE row per node, upserted in place: the table is O(nodes) forever, which is
-- the whole point. The operator was explicit that a metrics history belongs in
-- the telemetry backend, not in the control-plane SQLite file — a row per
-- sample per node is what turns a 4-node 2GB box into a swap-thrashing one.
--
-- The columns beyond the operator's sketch (cores, docker_up) exist because two
-- of the four tint rules cannot be evaluated without them:
--   · "load > 1.5 x cores" needs the node's core count;
--   · "docker unreachable" is the node's own daemon reporting that its engine
--     socket does not answer.
-- They ride on the same row, so the one-row invariant is unchanged.
--
-- sample_ts is the telemetry timestamp (when the daemon sampled the node),
-- updated_at is when the leader's daemon wrote the row. They differ when the
-- leader is behind on reading the collector's file — which is exactly the
-- staleness the panel must show.
CREATE TABLE IF NOT EXISTS node_stats (
    hostname     TEXT PRIMARY KEY,
    role         TEXT NOT NULL DEFAULT '',
    cores        INTEGER,
    load1        REAL,
    load5        REAL,
    load15       REAL,
    mem_total_mb INTEGER,
    mem_used_mb  INTEGER,
    mem_avail_mb INTEGER,
    docker_up    INTEGER NOT NULL DEFAULT 1,
    top_rss      TEXT NOT NULL DEFAULT '[]', -- JSON [{pid,name,rss_mb,cpu_pct}], <= 5, by rss desc
    top_cpu      TEXT NOT NULL DEFAULT '[]', -- JSON [{pid,name,rss_mb,cpu_pct}], <= 5, by cpu desc
    sample_ts    INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0
) STRICT;

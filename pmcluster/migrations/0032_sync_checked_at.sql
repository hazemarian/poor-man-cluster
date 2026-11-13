-- 0032_sync_checked_at.sql — separate "when the content arrived" from "when we last
-- confirmed it is still current".
--
-- Slice 3 aged the mirror from applied_at, and deployed on a real cluster it produced a
-- false alarm within four minutes: applied_at only moves when a 200 changes rows, and the
-- steady state of a healthy cluster is a 304 on every poll. So a follower that had just
-- been told "you are current" by the leader reported "applied 3m40s ago — STALE", which is
-- exactly the warning-that-fires-when-nothing-is-wrong this platform is not allowed to
-- ship.
--
-- Two different questions, two columns:
--
--   applied_at — when the mirrored ROWS were written (content identity).
--   checked_at — when this node last got a successful answer from the leader (200 or
--                304). This is the one an age, and therefore a staleness warning, may be
--                computed from: it moves on every poll that reaches the leader and stops
--                moving only when the leader really has gone away.
ALTER TABLE sync_state ADD COLUMN checked_at INTEGER NOT NULL DEFAULT 0;

-- Existing rows were written under the old meaning: their applied_at WAS the last
-- successful answer, so it is the honest backfill for checked_at (and it is non-zero
-- only on a node that has actually applied a snapshot).
UPDATE sync_state SET checked_at = applied_at;

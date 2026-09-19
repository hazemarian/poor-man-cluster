-- 0017_users_last_used.sql — track when an API token (users row) was last
-- used to authenticate.  Defaults to 0 (never), updated best-effort on each
-- successful bearer-token lookup.

ALTER TABLE users ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0;

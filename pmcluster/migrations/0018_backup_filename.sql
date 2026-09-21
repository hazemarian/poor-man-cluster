-- 0018_backup_filename.sql
--
-- Scheduled offen backup runs (the backup-stack's own cron) never pass
-- through the daemon, so pmcluster was blind to them. This column lets
-- the daemon discover archives on disk (/var/stack/backup) and record
-- each unique tarball exactly once, giving the console + CLI the full
-- backup picture (scheduled + on-demand) with real archive paths.

ALTER TABLE backups ADD COLUMN filename TEXT NOT NULL DEFAULT '';

-- Idempotency key: one row per archive file. The partial index keeps
-- legacy rows (filename '') outside the uniqueness constraint.
CREATE UNIQUE INDEX idx_backups_filename ON backups(filename) WHERE filename != '';
-- Versioned swarm-secret rotation (BUG-007).
--
-- Docker swarm secrets are immutable and cannot be removed while referenced
-- by a running service, so `secret edit` on a live stack silently failed to
-- rotate the mirrored value. Each secret row now tracks how many times it
-- has been rotated; the CLI mirrors the value into a NEW swarm secret named
-- <name>_v<rev> on every edit (rev 1 keeps the plain name), and the compose
-- writer references the versioned external name while the container mount
-- path (/run/secrets/<name>) stays stable.
ALTER TABLE secrets ADD COLUMN swarm_rev INTEGER NOT NULL DEFAULT 1;
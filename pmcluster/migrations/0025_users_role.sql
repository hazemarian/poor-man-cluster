-- 0025_users_role.sql — daemon-side API-token roles (FIX 5).
--
-- Adds a role tier to the daemon's users table so secret-bearing surfaces
-- (rendered configs, cluster settings) can distinguish the bootstrap admin /
-- edge console token from operator-minted API keys. Existing rows default to
-- 'admin' so every pre-existing token keeps today's full access (the CLI's
-- bootstrap admin token must keep reading secrets); newly minted API keys are
-- created as 'operator' by apikeys.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin';

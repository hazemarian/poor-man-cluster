-- 0020_api_key_stack.sql — per-stack API token scoping.
--
-- API tokens (the users table) may now be bound to a single application
-- stack. A scoped token may only drive that stack's deploy/sync/rollback/
-- delete and service (list/tasks/logs/restart/exec) endpoints; every other
-- /api operation is refused with HTTP 403 {"error":"token scoped to stack <x>"}.
--
-- '' is the default and the value of every pre-existing row: an unscoped
-- token, which keeps full (backward-compatible) access.

ALTER TABLE users ADD COLUMN stack TEXT NOT NULL DEFAULT '';

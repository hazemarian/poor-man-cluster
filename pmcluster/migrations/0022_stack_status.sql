-- 0022_stack_status.sql
-- The reconcile loop's DB-backed status snapshot. Every pass writes each
-- stack's aggregate status + per-service statuses here; the public badge
-- endpoints read ONLY this table (never live Docker queries) so badges are
-- cheap, stable and reflect the control loop's latest view.
CREATE TABLE stack_status (
    stack_name TEXT PRIMARY KEY,
    status     TEXT NOT NULL,             -- healthy | in progress | degraded | error | unknown
    services   TEXT NOT NULL DEFAULT '{}', -- JSON {"service":"healthy",...}
    updated_at INTEGER NOT NULL
) STRICT;
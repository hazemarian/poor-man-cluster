-- 0028_github_runners.sql — the console's view of the self-hosted runners.
--
-- ONE row per runner NAME (pmc-<node>-org), upserted by the node that owns it on
-- every reconcile pass and DELETED when the runner is removed, killed or the
-- feature is turned off. This is a current-state table, not a history: a runner
-- killed twice is one row, and the second kill updates it rather than appending a
-- tombstone.
--
-- Rows are per node, not per org: every node writes only its own runner's row,
-- so a store shared by several nodes never has one node delete another's row.
-- The console merges these rows with GitHub's org runner list (the registration
-- is what a job actually arrives on) — this table is the half GitHub cannot tell
-- you: which local container serves that name, and when it was last observed.
--
-- `org` is the runner's scope. An org-level runner serves every repository the
-- org runner group selects, so a per-repository column would be a lie.
--
-- state is derived, never invented: `busy`/`idle`/`offline` come from GitHub's
-- runner list, `starting` from a container that exists but has no running process
-- yet, `failed` from consecutive registration failures, and `offline` for a
-- runner an operator killed on the console (it is held until Restart).
--
-- mode is `ephemeral` for every runner this platform creates today (one job,
-- then exit). `persistent` is reserved for a runner that stays registered
-- between jobs; no code path writes it yet, and the CHECK keeps a typo out of the
-- column until one does.
--
-- container_id is the local docker container id — empty on a row that describes a
-- runner living on another node.
--
-- last_job_url is the organisation's newest workflow run, captured when the
-- runner leaves `busy`. GitHub's runner API does not attribute jobs to a runner,
-- so this is "the run that just ended in the org", not a per-runner history.
CREATE TABLE IF NOT EXISTS github_runners (
    name          TEXT    PRIMARY KEY,
    org           TEXT    NOT NULL,
    node          TEXT    NOT NULL,
    container_id  TEXT    NOT NULL DEFAULT '',
    mode          TEXT    NOT NULL DEFAULT 'ephemeral'
                          CHECK (mode IN ('ephemeral', 'persistent')),
    state         TEXT    NOT NULL DEFAULT 'starting'
                          CHECK (state IN ('starting', 'idle', 'busy', 'offline', 'failed')),
    registered_at INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,
    last_job_url  TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_github_runners_org ON github_runners (org);

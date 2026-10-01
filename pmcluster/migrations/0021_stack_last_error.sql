-- 0021_stack_last_error.sql
-- The most recent deploy/apply error for a stack ("" when the last deploy
-- succeeded). Fire-and-forget deploys apply in the background, so a failure
-- is no longer visible in the HTTP response — this column lets the console
-- surface it. Cleared on the next successful apply.
ALTER TABLE stacks ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
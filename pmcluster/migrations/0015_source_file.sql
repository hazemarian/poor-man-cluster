-- 0015_source_file.sql
-- Deploy provenance: which repo + which manifest file produced each deploy.
-- repo_url already lives on `stacks`; `source_file` records the manifest path
-- inside that repo (e.g. deploy/test-lms.yaml). Both are carried through the
-- webhook payload / daemon API / CLI, so the operator can trace a revision
-- back to its source and (later) let the console fetch the DSL straight from
-- git.
ALTER TABLE stacks ADD COLUMN source_file TEXT NOT NULL DEFAULT '';
ALTER TABLE stack_revisions ADD COLUMN source_file TEXT NOT NULL DEFAULT '';
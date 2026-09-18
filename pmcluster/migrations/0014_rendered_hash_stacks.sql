-- 0014_rendered_hash_stacks.sql — rendered-content fingerprint for app-stack revisions.
--
-- The deploy pipeline stores sha256(rendered_yaml) on every revision so a
-- no-op re-deploy can be detected without talking to Docker: when the source
-- manifest is re-translated and the rendered compose hashes identically to the
-- latest stored revision, there is nothing to apply. The hash is over the
-- RENDERED compose (not the source DSL) because config()/secrets() edits in the
-- DB change the rendered output while the source stays byte-identical — and
-- that drift is exactly what a sync must re-apply.

ALTER TABLE stack_revisions ADD COLUMN rendered_hash TEXT NOT NULL DEFAULT '';
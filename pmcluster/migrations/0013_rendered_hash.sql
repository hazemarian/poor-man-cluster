-- 0013_rendered_hash.sql — track the hash of the last rendered snapshot.
--
-- Change detection for cluster update / UI sync compares the hash of the
-- freshly rendered bytes against this column. A difference means the
-- rendered output changed (template content, config names, cert names,
-- edge image, ...) and the consuming stack must be re-deployed.
ALTER TABLE configs ADD COLUMN rendered_hash TEXT NOT NULL DEFAULT '';

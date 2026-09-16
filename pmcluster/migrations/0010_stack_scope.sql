-- stack scopes configs and secrets: cluster-scope rows live in Settings and
-- are applied by `cluster update` (stack stays ''); service-scope rows belong
-- to one application stack and are resolved at deploy time via the DSL
-- config(<name>) / secrets(<name>) env references.
ALTER TABLE configs ADD COLUMN stack TEXT NOT NULL DEFAULT '';
ALTER TABLE secrets ADD COLUMN stack TEXT NOT NULL DEFAULT '';
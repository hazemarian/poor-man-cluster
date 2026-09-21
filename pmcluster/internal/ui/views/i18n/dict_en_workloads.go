// Workload dictionaries: the stacks list, one stack's inspector, a single
// revision's manifests, and a stack's config + secrets page. The rules every
// dictionary obeys are in the package comment.

package i18n

func init() {
	register(EN, map[string]string{
		// ---- stacks list -----------------------------------------------------
		"stacks.title":           "Stacks",
		"stacks.sub":             "{0}\u00a0· newest change {1}",
		"stacks.sub_unknown":     "Stack list unavailable — the cluster API did not answer",
		"stacks.list_title":      "Deployed stacks",
		"stacks.search_ph":       "Filter by name, revision or repository",
		"stacks.clear_filter":    "Clear filter",
		"stacks.showing":         "Showing {0} of {1}",
		"stacks.col_repo":        "Repository",
		"stacks.repo_unset":      "No repository",
		"stacks.state_none":      "No services",
		"stacks.stat_stacks":     "Stacks",
		"stacks.stat_stacks_f":   "Tracked by this console",
		"stacks.stat_replicas":   "Replicas",
		"stacks.stat_replicas_f": "Running / desired across every stack",
		"stacks.stat_git":        "Git-backed",
		"stacks.stat_git_f":      "Deployed from a repository",
		"stacks.stat_changed":    "Last change",
		"stacks.stat_changed_f":  "Newest stack update",
		"stacks.act_redeploy":    "Redeploy",
		"stacks.act_config":      "Config & secrets",
		"stacks.act_backups":     "Backups",

		// Destructive actions are replayed with a confirm: the row asks before
		// the page re-renders the whole index.
		"stacks.confirm_sync":   "Redeploy {0} from its repository? The daemon applies the newest manifest; the stack keeps running while it does.",
		"stacks.confirm_delete": "Delete stack {0}? Its services and revisions are removed from the daemon and this cannot be undone.",
		"stacks.msg_removed":    "Stack {0} deleted.",

		// Unknown is not empty: the list failed to load, so no stack is claimed.
		"stacks.unknown_title": "Stack list unavailable",
		"stacks.unknown_body":  "The cluster API did not answer this request, so the stacks below are unknown — not empty. Check the API URL and token in Settings.",

		"stacks.empty_title": "No stacks yet",
		"stacks.empty_body":  "A stack is a manifest the daemon deploys and tracks by revision. Deploy one, or wire a repository webhook so a push deploys it.",

		"stacks.no_match_title": "No stack matches \"{0}\"",
		"stacks.no_match_body":  "The filter matched no stack name, revision or repository. Clearing it shows every stack the cluster has.",

		// ---- one stack's inspector ------------------------------------------
		"stack.sync_hint":       "Redeploying re-applies the newest manifest from the daemon; a new revision appears only when that manifest changed.",
		"stack.backup_title":    "Last backup",
		"stack.backup_started":  "Started",
		"stack.backup_finished": "Finished",
		"stack.backup_none":     "This stack has no recorded backup yet.",
		"stack.backup_hint":     "Backup history lives on the",

		"stack.revisions_title": "Revision history",
		"stack.rev_current":     "Current",
		"stack.rev_when":        "created {0}",
		"stack.view_manifest":   "Manifests",
		"stack.rollback":        "Roll back",

		// The stack page's services section: replica health + logs/tasks.
		"stack.back_all":               "All stacks",
		"stack.about_title":            "About this stack",
		"stack.services_title":         "Services in this stack",
		"stack.services_hint":          "Replica health straight from the swarm; tasks, logs and one-shot commands open per service.",
		"stack.services_none_title":    "No services",
		"stack.services_none_body":     "The swarm is not running any service for this stack. Redeploying re-applies the manifest and schedules them.",
		"stack.services_unknown_title": "Services unavailable",
		"stack.services_unknown_body":  "The cluster API did not return this stack's services, so replica health is unknown rather than empty.",

		"stack.confirm_rollback": "Roll back {0} to revision {1}? The daemon redeploys that revision's manifest and records a new revision for it.",

		"stack.revs_empty_title": "No revisions yet",
		"stack.revs_empty_body":  "The daemon records a revision the first time a stack is deployed. Deploy this one to start its history.",

		"stack.unknown_title": "This stack could not be read",
		"stack.unknown_body":  "The stack is missing from the daemon's answer: it may have been deleted, or the name in the URL may be wrong.",

		"stack.err_bad_revision": "That revision number is not valid.",
		"revision.err_number":    "That revision number is not valid.",
		"stack.msg_synced":       "Redeployed {0} — revision {1}.",
		"stack.msg_synced_same":  "Redeployed {0} — the manifest had not changed, so no new revision was recorded.",
		"stack.msg_rolled_back":  "Rolled back {0} to revision {1} — recorded as revision {2}.",

		// ---- one revision's manifests ---------------------------------------
		"revision.title":    "Revision {0} · {1}",
		"revision.sub":      "Recorded {0}",
		"revision.source":   "Source manifest",
		"revision.rendered": "Rendered manifest",

		// ---- a stack's configs + secrets ------------------------------------
		"stackconfigs.title": "Config & secrets · {0}",
		"stackconfigs.sub":   "Service-scope configs and secrets for this stack, referenced from its manifest as config(name) and secrets(name). Values are stored encrypted, and secrets are never rendered back into this page.",

		"stackconfigs.configs_title":   "Configs",
		"stackconfigs.secrets_title":   "Secrets",
		"stackconfigs.form_new_secret": "New secret",
		"stackconfigs.reveal":          "Reveal",
		"stackconfigs.secret_hidden":   "Value hidden",

		"stackconfigs.confirm_reveal":        "Reveal the value of {0}? Its plaintext appears on screen and can be copied, screenshotted or logged.",
		"stackconfigs.confirm_config_remove": "Delete config {0} and its version history? Stacks that reference it fail to deploy until a config of that name exists again.",
		"stackconfigs.confirm_secret_remove": "Delete secret {0}? Stacks that reference it fail to deploy until a secret of that name exists again.",

		"stackconfigs.configs_unknown_title": "Configs unavailable",
		"stackconfigs.configs_unknown_body":  "The config list did not load, so this stack's configs are unknown — not empty. The secrets below were read separately and stand on their own.",

		"stackconfigs.configs_empty_title": "No configs for this stack",
		"stackconfigs.configs_empty_body":  "Add one to keep a file, template or environment block with the stack, then reference it from the manifest.",

		"stackconfigs.secrets_unknown_title": "Secrets unavailable",
		"stackconfigs.secrets_unknown_body":  "The secret list did not load, so this stack's secrets are unknown — not empty. Nothing is listed because nothing was read, not because none exist.",

		"stackconfigs.secrets_empty_title": "No secrets for this stack",
		"stackconfigs.secrets_empty_body":  "Add one to store a value the stack's services read at deploy time, without it living in the manifest.",

		"stackconfigs.err_bad_name":    "That name is not valid.",
		"stackconfigs.err_bad_version": "That version id is not valid.",

		"stackconfigs.msg_config_added":   "Config {0} created for stack {1}.",
		"stackconfigs.msg_config_edited":  "Config {0} updated — a new version was recorded.",
		"stackconfigs.msg_config_rolled":  "Config {0} rolled back to version {1}.",
		"stackconfigs.msg_config_removed": "Config {0} deleted.",
		"stackconfigs.msg_secret_added":   "Secret {0} created for stack {1}.",
		"stackconfigs.msg_secret_edited":  "Secret {0} updated.",
		"stackconfigs.msg_secret_removed": "Secret {0} deleted.",

		// ---- errors the stacks controllers raise -----------------------------
		"err.stacks":               "Stacks are unavailable.",
		"err.stack":                "This stack could not be read.",
		"err.stack_sync":           "The stack could not be redeployed.",
		"err.stack_rollback":       "The stack could not be rolled back.",
		"err.stack_remove":         "The stack could not be deleted.",
		"err.revision":             "That revision could not be read.",
		"err.stackconfigs_configs": "The stack's configs could not be read.",
		"err.stackconfigs_secrets": "The stack's secrets could not be read.",
		"err.config_remove":        "The config could not be deleted.",
		"err.config_rollback":      "The config could not be rolled back.",
		"err.secret_remove":        "The secret could not be deleted.",
		"stacks.col_file":          "File",
		"stacks.file_unset":        "None recorded",
		"revision.pipeline":        "Pipeline",
		"revision.pipeline_empty":  "This revision recorded no stages.",
		"revision.pipeline_sub":    "The stages the daemon ran, in order.",
	})
}

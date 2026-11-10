package i18n

// English — the settings page as six grouped sections.
//
// The copy here exists because three failures were discovered by failing: a
// runner switch with no org, no selected worker, and an App permission that
// covered the wrong endpoints. Every section therefore states what it needs
// BEFORE its fields, and the runners section names the exact GitHub permission
// instead of describing it.
func init() {
	register(EN, map[string]string{
		// ---------- Connection ----------
		"settings.sec_connection":       "Connection",
		"settings.sec_connection_sub":   "How this console reaches the daemon: the URL it calls and the token it presents.",
		"settings.sec_connection_needs": "Stored in this console's own store. Nothing in this section is written into the cluster.",
		"settings.save_connection":      "Save connection",

		"settings.offline_notice_title": "The daemon is not configured",
		"settings.offline_notice_body":  "Sections that read the cluster say so instead of showing a default, and nothing can be saved from them. Set the URL and the token in Connection below.",

		// ---------- GitHub App ----------
		"settings.sec_github_needs":    "Needs the App installed on the organisation with Contents and Packages: Read. Registering CI runners additionally needs Organization → Self-hosted runners: Read & write, and a permission change only takes effect after the installation token is re-minted.",
		"settings.github_status_title": "What is installed",

		// ---------- one save bar ----------
		"settings.bar_clean": "No unsaved changes",
		"settings.bar_dirty": "Unsaved changes",
		"settings.bar_scope": "Saves Identity and sign-in, CI runners, Backups and Advanced. Each node picks the values up on its next refresh; nothing is deployed.",
		"settings.dismiss":   "Dismiss",

		// ---------- the answer to a save ----------
		"settings.msg_cluster_saved_changed": "Settings saved. Changed:",
		"settings.msg_cluster_saved_none":    "Settings saved — nothing had changed.",
		"settings.err_body_lead":             "The daemon refused the save and said:",
		"settings.err_jump":                  "Go to the section",
		"settings.err_flagged":               "refused",
		"settings.err_flagged_body":          "This is the section the daemon refused — its full message is at the top of the page.",
		"settings.err_permission_hint":       "This reads as a permission failure: the App needs Organization → Self-hosted runners: Read & write, and a permission change only reaches the daemon once the installation token is re-minted — rotate the key in the GitHub App section, or re-install the App, then run Test connection.",

		// ---------- runners summary (read-only; the picker lives on /web/runners) ----------
		"settings.runners_summary_title":   "What is registered now",
		"settings.runners_summary_off":     "Off — no runner container exists and none is registered at GitHub.",
		"settings.runners_summary_nodes":   "Selected workers:",
		"settings.runners_summary_none":    "no worker selected yet.",
		"settings.runners_summary_unknown": "The daemon did not report the runner list, so this summary is unknown rather than empty.",
		"settings.runners_pick":            "Pick worker nodes",
		"settings.runners_pick_tip":        "Open the Runners page: choose the workers that may run a runner, and kill or restart one.",

		// ---------- Advanced: rendered configs ----------
		"settings.adv_rendered_title": "Rendered platform configs",
		"settings.adv_rendered_sub":   "The exact YAML sent to the swarm for the platform's own configs, after template substitution. Read-only.",
		"settings.rendered_empty":     "No rendered config is recorded yet. The first Apply to swarm writes them.",
		"settings.rendered_unknown":   "The daemon did not answer, so this list is unknown rather than empty.",
		"settings.rendered_one":       "One rendered config is recorded. Read it from Inventory → Platform, where each platform config row carries its rendered snapshot.",
		"settings.rendered_many":      "Several rendered configs are recorded. Read one from Inventory → Platform, where each platform config row carries its rendered snapshot.",

		"settings.cluster_unknown_body": "The cluster settings could not be read, so this page shows no fields that a later save could blank out. The reason is above; retry once the daemon answers.",
	})
}

// English — the cluster sections' own headings and prerequisites (the keys the
// clustersettings.* namespace owns live next to the form that uses them).
func init() {
	register(EN, map[string]string{
		"clustersettings.sect_identity":       "Identity and sign-in",
		"clustersettings.sect_identity_sub":   "The domain the cluster answers on, and who is allowed to sign in.",
		"clustersettings.sect_identity_needs": "Needs a provider the daemon knows plus a client id and secret from it; with single sign-on off, the built-in password form stays in charge.",
		"clustersettings.sect_advanced":       "Advanced",
		"clustersettings.sect_advanced_sub":   "Paths and levels that rarely change, with the platform configs as they were rendered.",
		"clustersettings.sect_advanced_needs": "Needs the admin role. These values reach every node's stack files, so they are collapsed by default.",

		"clustersettings.sect_runners_needs":     "Needs a configured GitHub App, github_runner_org, and at least one worker selected on the Runners page.",
		"clustersettings.runners_perm_title":     "The permission runner registration needs",
		"clustersettings.runners_perm_body":      "Organization → Self-hosted runners: Read & write — not Administration, which only covers repository-level endpoints, and not a repository permission. The installation token is minted from the App's current permissions, so a change on GitHub does nothing until the token is re-minted: rotate the key in the GitHub App section or re-install the App, then run Test connection.",
		"clustersettings.runners_perm_where":     "GitHub: Settings → Applications → this App → Permissions → Organization → Self-hosted runners.",
		"clustersettings.runner_org_required":    "Required when runners are enabled.",
		"clustersettings.runners_security_title": "Security notes",

		"clustersettings.sect_backups_needs": "Needs a backup store. The bucket, its region and its credentials live in the daemon's environment; this page decides when a backup runs and how long it is kept.",
		"clustersettings.backup_store_body":  "A local volume, plus the offsite bucket when one is configured. Both are set in the daemon's environment (PMCLUSTER_BACKUP_*), not on this page: a credential typed into a form the cluster only half-applies is worse than one stated as external.",

		"clustersettings.err_org_required":   "github_runner_org is empty while runners are enabled — registration is org level, so it cannot work.",
		"clustersettings.err_nodes_required": "No worker is selected for runners; choose one on the Runners page.",
	})
}

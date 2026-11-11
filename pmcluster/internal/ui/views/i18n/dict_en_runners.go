package i18n

// English dictionary — the self-hosted runners panel: the node picker that decides
// which workers may run a runner, and the table of runners with kill/restart.
//
// The wording carries the two decisions an operator has to understand: a runner is
// a local container on ONE selected worker (N nodes = N runners), and a build on a
// manager marks the node Down.
func init() {
	register(EN, map[string]string{
		"runners.title": "Runners",
		"runners.sub":   "Self-hosted GitHub Actions runners, one container per selected worker, registered at org level into a runner group.",

		"runners.enabled":  "enabled",
		"runners.disabled": "disabled",

		"runners.picker_title":   "Which workers may run a runner",
		"runners.picker_sub":     "Each selected worker runs one ephemeral runner container and registers it in the org's runner group. Managers are not selectable: they run the control plane, and a build there marks the node Down.",
		"runners.capacity":       "Capacity is the list: one runner per selected node. Each node is capped at {0} unless its row carries its own cap — a shared cap fits one machine and is permission to OOM the next. The memory column is that node's available memory right now.",
		"runners.col_select":     "Select",
		"runners.col_node":       "Node",
		"runners.col_role":       "Role",
		"runners.col_reason":     "Why not",
		"runners.col_node_state": "Runner",
		"runners.col_memory":     "Memory",
		"runners.col_cap":        "Cap",
		"runners.effective_cap":  "effective",
		"runners.cap_aria":       "Memory cap for {0} (blank uses the cluster default)",
		"runners.mb":             "MB",
		"runners.state_none":     "none",
		"runners.not_selected":   "not selected",
		"runners.no_nodes":       "The daemon returned no nodes.",
		"runners.save_nodes":     "Save selection",
		"runners.save_hint":      "Adding a node starts a runner there on the next reconcile; removing one stops and de-registers it. A cap above 70 % of a node's RAM is refused — leave the field blank to use the cluster default.",

		"runners.table_title":  "Runners on the cluster",
		"runners.col_scope":    "Scope",
		"runners.col_state":    "State",
		"runners.col_mode":     "Mode",
		"runners.col_age":      "Age",
		"runners.col_last_job": "Last job",

		"runners.state_starting":     "starting",
		"runners.state_idle":         "idle",
		"runners.state_busy":         "busy",
		"runners.state_offline":      "offline",
		"runners.state_between_jobs": "between jobs",
		"runners.state_failed":       "failed",
		"runners.mode_ephemeral":     "ephemeral",
		"runners.mode_persistent":    "persistent",

		"runners.kill":              "Kill",
		"runners.kill_tip":          "Remove this runner's container and delete its registration. It stays stopped until you restart it.",
		"runners.kill_busy_confirm": "This runner is executing a job. Killing it removes the container mid-workflow and FAILS that job — the run is cancelled and cannot be resumed. Kill runner on {0}?",
		"runners.restart":           "Restart",
		"runners.restart_tip":       "Re-register this runner and start its container again.",
		"runners.restart_all":       "Restart stopped",
		"runners.restart_all_tip":   "Restart every runner the table shows as stopped.",

		"runners.disabled_title":      "Self-hosted runners are off",
		"runners.disabled_body":       "github_runners_enabled is false, so no runner container exists and none is registered at GitHub. Nothing is left running.",
		"runners.disabled_action":     "Open cluster settings",
		"runners.disabled_action_tip": "Turn github_runners_enabled on, then pick the workers here.",
		"runners.disabled_foot":       "Turning it on needs a configured GitHub App (administration: write at the organization level), an organisation, and at least one selected worker.",

		"runners.empty_title": "No runner yet",
		"runners.empty_body":  "The feature is on but no runner has registered. A selected worker starts one within a minute — check its state in the table above.",

		"runners.not_configured_body": "The console is not connected to a daemon, so it cannot read the runner settings or the runner list.",
		"runners.unknown_title":       "The runner list could not be loaded",
		"runners.unknown_body":        "The console could not read the runner list from the daemon. This is not an empty list — nothing was changed.",

		"runners.foot": "A killed runner is held: no reconcile pass re-registers it until Restart. The last job is the org's newest workflow run, because GitHub does not attribute a job to the runner that ran it.",

		"settings.runners_title": "Self-hosted runners",
		"settings.runners_body":  "One runner container per selected worker, registered at org level into a runner group. Choose the workers, watch each runner's state, and kill or restart one from the Runners panel.",
		"settings.runners_open":  "Open runners",

		"runners.err_list":               "The runner list could not be read",
		"runners.err_settings":           "The runner settings could not be read",
		"runners.err_nodes":              "The node list could not be read",
		"runners.err_bad_name":           "That runner name is not valid.",
		"runners.err_manager":            "That node cannot run a runner",
		"runners.err_save":               "The node selection could not be saved",
		"runners.err_kill":               "The runner could not be killed",
		"runners.err_kill_deregister":    "The container was removed, but GitHub did not confirm the de-registration",
		"runners.err_kill_register":      "The kill could not complete",
		"runners.err_restart":            "The runner could not be restarted",
		"runners.err_restart_deregister": "The container was removed, but GitHub did not confirm the de-registration",
		"runners.err_restart_register":   "The runner was killed but not registered again",
		"runners.err_restart_partial":    "Only some runners were restarted",
		"runners.err_not_found":          "No runner with that name (it may have been removed by a reconcile pass)",
		"runners.err_busy":               "That runner is busy: killing it would fail the job it is running",
		"runners.err_unknown_node":       "That node is not in the cluster, so no cap was saved for it",
		"runners.err_memory":             "That memory cap is not a Docker quantity (e.g. 1200m or 3g)",

		"runners.msg_killed":        "Killed runner {0}.",
		"runners.msg_restarted":     "Restarted runner {0}.",
		"runners.msg_restarted_all": "Restarted {0} stopped runner(s).",
		"runners.msg_saved":         "Node selection saved: {0}.",
		"runners.msg_saved_none":    "Node selection cleared: no runner will run until a worker is selected.",
	})

	registerPlural(EN, "plural.runner_nodes", PluralForms{One: "{n} node selected", Other: "{n} nodes selected"})
	registerPlural(EN, "plural.runners", PluralForms{One: "{n} runner", Other: "{n} runners"})
}

package i18n

// English dictionary — the CI board.
//
// Two pieces of wording carry decisions rather than labels:
//
//   - "superseded" is marked INFERRED. GitHub does not report who cancelled a run,
//     and today a newer commit, an operator and a competing dispatcher all arrive as
//     the same word "cancelled". Presenting the guess as fact would be exactly the
//     kind of confident wrong answer this panel exists to stop.
//   - "not readable" is a ROW, not a missing row. Hiding a repo the App cannot see
//     makes "no information" look like "everything is green".
func init() {
	register(EN, map[string]string{
		"nav.ci": "CI",

		"ci.title": "CI",
		"ci.sub":   "Every repository's latest workflow run, newest activity first.",

		"ci.queued":       "Queued",
		"ci.running":      "Running",
		"ci.failed":       "Failed",
		"ci.read":         "Data age",
		"ci.cached":       "cached",
		"ci.col_repo":     "Repository",
		"ci.col_state":    "State",
		"ci.col_workflow": "Workflow",
		"ci.col_sha":      "Commit",
		"ci.col_updated":  "Updated",
		"ci.col_detail":   "Detail",

		"ci.state_queued":     "queued",
		"ci.state_running":    "running",
		"ci.state_ok":         "ok",
		"ci.state_failed":     "failed",
		"ci.state_cancelled":  "cancelled",
		"ci.state_superseded": "superseded",
		"ci.state_unreadable": "not readable",
		"ci.state_none":       "no runs",
		"ci.state_other":      "—",

		"ci.open":    "open",
		"ci.no_rows": "No repository has reported a workflow run yet.",

		"ci.not_configured":      "The CI board needs the GitHub App.",
		"ci.not_configured_hint": "Without an installation token the platform cannot read Actions, and an empty board would read as \"everything is green\".",

		"ci.inferred_tip": "Inferred, not reported: GitHub does not say who cancelled a run. A newer run of the same workflow exists, so a later commit most likely replaced it.",

		"ci.footnote": "* inferred — GitHub does not report why a run was cancelled. Everything else is read from GitHub's Actions API and cached for a minute.",

		"ci.err_load":    "Could not read the CI board.",
		"ci.err_partial": "Showing the last successful read — the repository list could not be refreshed.",
	})
}

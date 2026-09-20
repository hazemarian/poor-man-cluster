package i18n

// English strings for the delivery pages: Deploy (frag_deploy.html) and
// Webhooks (frag_webhooks.html). Both pages are one namespace because they are
// two halves of one flow — the manifest the console submits and the sources CI
// submits with.
//
// Technical tokens (endpoint paths, header names, JSON keys) stay verbatim in
// English inside the templates: they are code the operator pastes, not copy.
// Everything a human reads here is translated.
func init() {
	register(EN, map[string]string{
		// --- Deploy: header ---------------------------------------------------
		"deploy.title":              "Deploy a stack",
		"deploy.sub":                "Post a manifest to the cluster API. The daemon rewrites the stack and stores a new revision.",
		"deploy.sub_not_configured": "The cluster API is not configured yet, so nothing can be submitted from this page.",
		"deploy.open_stacks":        "Open stacks",

		// --- Deploy: stat strip ----------------------------------------------
		"deploy.st_target":            "Target stack",
		"deploy.st_target_foot":       "The app name in this manifest",
		"deploy.no_target":            "Not named",
		"deploy.st_named_foot":        "The daemon reads the name from the manifest's app field",
		"deploy.st_revision":          "Current revision",
		"deploy.st_revision_foot":     "Live revision of the target stack",
		"deploy.st_revision_new":      "New stack",
		"deploy.st_revision_new_foot": "No stack has that name yet — the daemon creates it",
		"deploy.st_updated":           "Last revision stored",
		"deploy.st_updated_foot":      "When the daemon stored that revision",
		"deploy.st_stacks":            "Known stacks",
		"deploy.st_stacks_foot":       "Stacks the daemon reports",
		"deploy.st_unknown_foot":      "Unknown — the stack list could not be read",

		// --- Deploy: the manifest form ---------------------------------------
		"deploy.form_title":      "Manifest",
		"deploy.f_app":           "App name",
		"deploy.ph_app":          "bookfair",
		"deploy.f_app_hint":      "Names the stack to rewrite. Leave it empty to let the daemon read the name from the manifest.",
		"deploy.f_version":       "Version",
		"deploy.ph_version":      "v1.4.0",
		"deploy.f_version_hint":  "Optional. Overrides the version the manifest declares — that is what CI bumps per release.",
		"deploy.f_repo":          "Repository",
		"deploy.ph_repo":         "https://github.com/acme/bookfair",
		"deploy.f_repo_hint":     "Optional. Recorded on the stack so the next operator knows where this came from.",
		"deploy.f_manifest":      "Manifest (DSL)",
		"deploy.ph_manifest":     "app: bookfair",
		"deploy.f_manifest_hint": "Paste the DSL manifest. The daemon parses, validates and translates it before anything is deployed.",
		"deploy.submit":          "Deploy",
		"deploy.submit_named":    "Deploy to {0}",
		"deploy.confirm_new":     "Deploy this manifest as a new stack? The daemon creates it and stores the first revision.",
		"deploy.confirm_named":   "Deploy this manifest to {0}? Its current running revision is replaced.",
		"deploy.submit_hint":     "Sent as JSON to the daemon. Nothing is written until the daemon answers.",

		// --- Deploy: result ---------------------------------------------------
		"deploy.result_title":   "Deploy accepted",
		"deploy.result_body":    "Deployed {0} as revision {1}.",
		"deploy.result_changed": "The daemon reported a new revision for this stack.",
		"deploy.result_hint":    "Task convergence appears under Services once the daemon finishes rolling the stack out.",
		"deploy.open_stack":     "Open stack",
		"deploy.view_revision":  "View revision",

		// --- Deploy: request preview -----------------------------------------
		"deploy.preview_title":       "Request body",
		"deploy.preview_sub":         "Exactly what this page sends to the daemon, before you submit it.",
		"deploy.preview_endpoint":    "Endpoint",
		"deploy.preview_method":      "Method",
		"deploy.preview_size":        "Body size",
		"deploy.preview_empty_title": "Nothing to send yet",
		"deploy.preview_empty_body":  "Paste a manifest above and the exact request body appears here first, so you can check it before the daemon sees it.",
		"deploy.preview_goto":        "Go to the manifest form",

		// --- Deploy: what the daemon does ------------------------------------
		"deploy.pipeline_title":      "What the daemon does",
		"deploy.pipeline_sub":        "One request runs five stages. A failure stops the run and leaves the stored revision untouched.",
		"deploy.step_parse":          "Parse the manifest",
		"deploy.step_parse_note":     "The DSL is read as YAML and checked for structure.",
		"deploy.step_interp":         "Interpolate and validate",
		"deploy.step_interp_note":    "Variables are resolved and required fields are verified.",
		"deploy.step_translate":      "Translate to Compose",
		"deploy.step_translate_note": "configs() and secrets() are resolved against the store.",
		"deploy.step_record":         "Store the revision",
		"deploy.step_record_note":    "Source and rendered YAML are kept under the new revision number.",
		"deploy.step_apply":          "Roll out the stack",
		"deploy.step_apply_note":     "Swarm converges the services on the new Compose file.",
		"deploy.pipeline_foot":       "The stages are the daemon's fixed order; the console is not told how far a given run got, so none of them is marked as started.",

		// --- Deploy: the stack list ------------------------------------------
		"deploy.stacks_title":         "Stacks",
		"deploy.stacks_empty_title":   "No stacks yet",
		"deploy.stacks_empty_body":    "Nothing has been deployed. Paste a manifest above and submit it to create the first stack.",
		"deploy.stacks_unknown_title": "Stack list unavailable",
		"deploy.stacks_unknown_body":  "The daemon did not answer this request, so the stacks below are unknown — not empty.",
		"deploy.col_stack":            "Stack",
		"deploy.row_deploy":           "Deploy to this stack",
		"deploy.copy_name":            "Copy stack name",
		"deploy.stacks_foot":          "Two deploys can run at once; the daemon serializes revisions per stack, not across stacks.",

		// --- Deploy: errors ---------------------------------------------------
		"deploy.err_manifest": "A manifest is required before the daemon can deploy anything",
		"deploy.err_deploy":   "The daemon rejected this deploy",
		"deploy.err_stacks":   "The stack list could not be read from the daemon",

		// --- Webhooks: header -------------------------------------------------
		"webhooks.title": "Webhooks",
		"webhooks.sub":   "Sources that may deploy by posting a signed manifest to the receiver. Each source has its own shared secret.",

		// --- Webhooks: stat strip ---------------------------------------------
		"webhooks.st_sources":               "Sources",
		"webhooks.st_sources_foot":          "Webhook sources the daemon reports",
		"webhooks.st_unknown_foot":          "Unknown — the source list could not be read",
		"webhooks.st_endpoint":              "Receiver URL",
		"webhooks.st_endpoint_foot":         "Where the daemon accepts signed deliveries",
		"webhooks.st_endpoint_unknown_foot": "No public domain is configured for this console",
		"webhooks.st_sig_foot":              "sha256=<hex> over the timestamp and the raw body",
		"webhooks.st_skew_foot":             "Older deliveries are refused as replays",

		// --- Webhooks: one-time secret ---------------------------------------
		"webhooks.secret_title":            "Shared secret for {0}",
		"webhooks.secret_pill":             "Shown once",
		"webhooks.secret_body":             "This is the only time the daemon returns it. Store it in your CI secret store before you leave this page.",
		"webhooks.secret_copy":             "Copy secret",
		"webhooks.secret_endpoint":         "Endpoint",
		"webhooks.secret_endpoint_unknown": "Unknown — this console has no public domain configured",
		"webhooks.secret_usage":            "CI signs every delivery with this secret; the daemon refuses anything that does not match.",

		// --- Webhooks: delivery contract -------------------------------------
		"webhooks.delivery_title": "Signing a delivery",
		"webhooks.delivery_sub":   "The receiver is the only machine way in: it verifies the signature against the source's secret before it reads the manifest.",
		"webhooks.d_method":       "Method",
		"webhooks.d_path":         "Path",
		"webhooks.d_sig":          "Signature header",
		"webhooks.d_ts":           "Timestamp header",
		"webhooks.d_skew":         "Timestamp window",
		"webhooks.d_skew_v":       "5 minutes",
		"webhooks.d_note":         "Sign the timestamp and the raw body together as HMAC-SHA256, hex-encode the result and prefix it with sha256=. The timestamp is in Unix seconds.",
		"webhooks.d_body":         "Body",
		"webhooks.d_body_v":       "The deploy payload as JSON — the same fields as the Deploy page, and the same validation.",
		"webhooks.d_snippet":      "Signing it in CI",

		// --- Webhooks: add a source ------------------------------------------
		"webhooks.add_title":      "Add a source",
		"webhooks.add_sub":        "A source is a name your CI signs with. The daemon generates the shared secret and shows it once, here.",
		"webhooks.f_source":       "Source",
		"webhooks.ph_source":      "github-actions",
		"webhooks.f_source_hint":  "Used verbatim in the endpoint path and in the signature, and it cannot be renamed later.",
		"webhooks.f_desc":         "Description",
		"webhooks.ph_desc":        "bookfair ci",
		"webhooks.f_desc_hint":    "Optional. What deploys from this source, so the next operator recognises it.",
		"webhooks.create_action":  "Create source",
		"webhooks.create_missing": "Fill in a source name to get a secret.",

		// --- Webhooks: the source list ---------------------------------------
		"webhooks.keys_title":     "Webhook Keys",
		"webhooks.col_source":     "Source",
		"webhooks.col_last":       "Last delivery",
		"webhooks.never_used":     "Never used",
		"webhooks.copy_source":    "Copy source name",
		"webhooks.copy_endpoint":  "Copy endpoint",
		"webhooks.revoke":         "Revoke",
		"webhooks.revoke_confirm": "Revoke {0}? Its shared secret stops being accepted immediately and the source is removed.",
		"webhooks.keys_foot":      "A source cannot be renamed. Revoke it and add a new one — the old secret dies with it.",
		"webhooks.empty_title":    "No webhook sources",
		"webhooks.empty_body":     "Nothing can deploy by webhook yet. Add a source, then put its shared secret in your CI.",
		"webhooks.empty_action":   "Add a source",
		"webhooks.unknown_title":  "Source list unavailable",
		"webhooks.unknown_body":   "The daemon did not answer this request, so the sources below are unknown — not none.",

		// --- Webhooks: outcomes and errors -----------------------------------
		"webhooks.msg_created":                  "Webhook source {0} created. Copy the shared secret now — it is shown once.",
		"webhooks.msg_removed":                  "Removed webhook source {0}. Deliveries signed with its secret are rejected from now on.",
		"webhooks.not_configured_body":          "Webhook sources live on the daemon, so the console needs the cluster API before it can list them.",
		"webhooks.err_source":                   "A source name is required before a secret can be generated",
		"webhooks.err_list":                     "The webhook source list could not be read from the daemon",
		"webhooks.err_create":                   "The daemon could not create this webhook source",
		"webhooks.err_remove":                   "The daemon could not remove this webhook source",
		"deploy.f_file":                         "Manifest file",
		"deploy.f_file_hint":                    "Optional. Recorded with the revision as provenance — where this manifest came from.",
		"deploy.ph_file":                        "deploy/my-app.yaml",
		"webhooks.act_history":                  "Deliveries",
		"webhookdeliveries.title":               "Deliveries for {0}",
		"webhookdeliveries.sub":                 "Attempts the receiver recorded, newest first.",
		"webhookdeliveries.sub_unknown":         "The delivery log could not be read, so this history is unknown rather than empty.",
		"webhookdeliveries.hist_title":          "Delivery attempts",
		"webhookdeliveries.foot":                "The most recent {0} attempts are listed.",
		"webhookdeliveries.col_when":            "When",
		"webhookdeliveries.col_status":          "Result",
		"webhookdeliveries.col_stack":           "Stack",
		"webhookdeliveries.col_revision":        "Revision",
		"webhookdeliveries.col_file":            "File",
		"webhookdeliveries.col_error":           "Error",
		"webhookdeliveries.status_accepted":     "Accepted",
		"webhookdeliveries.status_unauthorized": "Rejected — signature",
		"webhookdeliveries.status_error":        "The receiver errored",
		"webhookdeliveries.empty_title":         "Nothing has been delivered to this source",
		"webhookdeliveries.empty_body":          "The receiver has logged no attempts for this source — nothing has called it yet.",
		"webhookdeliveries.unknown_title":       "The delivery log is unavailable",
		"webhookdeliveries.unknown_body":        "The receiver did not return its log, so no attempt can be shown either way.",
		"err.webhook_deliveries":                "The delivery history could not be read",
	})

	registerPlural(EN, "plural.sources", PluralForms{One: "{n} source", Other: "{n} sources"})
}

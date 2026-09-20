package i18n

// English dictionary — application shell, navigation, shared vocabulary.
// Part files register into the language maps from init() so each domain can be
// reviewed on its own.

func init() {
	register(EN, map[string]string{
		// product
		"brand.sub":        "operator console",
		"app.title":        "pmcluster",
		"nav.menu":         "Menu",
		"nav.close":        "Close menu",
		"nav.language":     "Language",
		"nav.theme":        "Theme",
		"nav.logout":       "Log out",
		"nav.signed_in_as": "Signed in as {0}",

		// navigation groups
		"nav.group.cluster":  "Cluster",
		"nav.group.delivery": "Delivery",
		"nav.group.trust":    "Trust",
		"nav.group.access":   "Access",
		"nav.group.system":   "System",
		"nav.group.external": "External",

		// navigation items
		"nav.overview": "Overview",
		"nav.stacks":   "Stacks",
		"nav.services": "Services",
		"nav.deploy":   "Deploy",
		"nav.webhooks": "Webhooks",
		"nav.tls":      "Certificates",
		"nav.backups":  "Backups",
		"nav.users":    "Users",
		"nav.apikeys":  "API keys",
		"nav.settings": "Settings",
		"nav.observ":   "OpenObserve",
		"nav.traefik":  "Traefik",
		"nav.more":     "More",

		// shared verbs — buttons take the verbal noun (المصدر)
		"common.save":      "Save",
		"common.cancel":    "Cancel",
		"common.close":     "Close",
		"common.add":       "Add",
		"common.edit":      "Edit",
		"common.remove":    "Remove",
		"common.delete":    "Delete",
		"common.confirm":   "Confirm",
		"common.copy":      "Copy",
		"common.copied":    "Copied to the clipboard",
		"common.refresh":   "Refresh",
		"common.retry":     "Retry",
		"common.search":    "Search",
		"common.filter":    "Filter",
		"common.apply":     "Apply",
		"common.restore":   "Restore",
		"common.download":  "Download",
		"common.upload":    "Upload",
		"common.back":      "Back",
		"common.open":      "Open",
		"common.view_all":  "View all",
		"common.details":   "Details",
		"common.show_more": "Show more",
		"common.show_less": "Show less",
		"common.dismiss":   "Dismiss",

		// shared nouns / labels
		"common.actions":     "Actions",
		"common.name":        "Name",
		"common.id":          "ID",
		"common.type":        "Type",
		"common.status":      "Status",
		"common.source":      "Source",
		"common.target":      "Target",
		"common.host":        "Host",
		"common.node":        "Node",
		"common.image":       "Image",
		"common.created":     "Created",
		"common.updated":     "Updated",
		"common.never":       "Never run",
		"common.size":        "Size",
		"common.version":     "Version",
		"common.revision":    "Revision",
		"common.role":        "Role",
		"common.username":    "Username",
		"common.password":    "Password",
		"common.email":       "Email",
		"common.optional":    "optional",
		"common.required":    "required",
		"common.none":        "None",
		"common.unknown":     "Unknown",
		"common.unavailable": "Unavailable",
		"common.all":         "All",
		"common.yes":         "Yes",
		"common.no":          "No",
		"common.loading":     "Loading…",
		"common.saving":      "Saving…",
		"common.working":     "Working…",
		"common.last_run":    "Last run",
		"common.duration":    "Duration",
		"common.target_name": "Target",
		"common.warning":     "Warning",
		"common.error":       "Error",
		"common.reason":      "Reason",
		"common.raw":         "Raw response",
		"common.expires":     "Expires",
		"common.issuer":      "Issuer",
		"common.domains":     "Domains",
		"common.secret":      "Secret",
		"common.value":       "Value",
		"common.description": "Description",
		"common.category":    "Category",
		"common.summary":     "Summary",
		"common.automatic":   "Automatic",
		"common.manual":      "Manual",

		// statuses — اسم الفاعل / المبني للمجهول
		"st.running":    "Running",
		"st.ready":      "Ready",
		"st.active":     "Active",
		"st.inactive":   "Inactive",
		"st.pending":    "Pending",
		"st.queued":     "Queued",
		"st.failed":     "Failed",
		"st.done":       "Done",
		"st.degraded":   "Degraded",
		"st.draining":   "Draining",
		"st.valid":      "Valid",
		"st.expired":    "Expired",
		"st.verified":   "Verified",
		"st.incomplete": "Incomplete",
		"st.missed":     "Missed",
		"st.ok":         "OK",
		"st.leader":     "Leader",
		"st.manager":    "Manager",
		"st.worker":     "Worker",
		"st.admin":      "Admin",
		"st.operator":   "Operator",
		"st.viewer":     "Viewer",
		"st.sso_gated":  "SSO-gated",
		"st.unknown":    "Unknown",

		// errors — تعذّر / لم نتمكن من, never a failure calque
		// Reached only on a failed call, so these two are the difference between an
		// operator seeing a sentence and seeing a raw key.
		"err.backups_coverage": "Backup coverage could not be read",
		"err.backup_create":    "The backup could not be created",
		"err.unreachable":      "The pmcluster API could not be reached",
		"err.upstream":         "This data could not be loaded",
		"err.not_manager":      "This host is not a swarm manager",
		"err.swarm_inactive":   "No swarm is active on this host",
		"err.copy_failed":      "The value could not be copied",
		"err.logout_failed":    "Signing out failed",
		"err.show_details":     "Show details",
		"err.hide_details":     "Hide details",
		"err.retry_hint":       "Try again in a moment.",

		// topbar
		"topbar.refresh":      "Refresh this page",
		"topbar.theme_toggle": "Switch theme",
		"topbar.lang_toggle":  "Switch language",

		// swarm / cluster vocabulary
		"cluster.state":            "Cluster status",
		"cluster.quorum":           "Quorum {0}/{1}",
		"cluster.nodes":            "Nodes",
		"cluster.managers":         "Managers",
		"cluster.services":         "Services",
		"cluster.stacks":           "Stacks",
		"cluster.cpus":             "CPUs",
		"cluster.memory":           "Memory",
		"cluster.engine":           "Engine",
		"cluster.daemon":           "Daemon",
		"cluster.docker":           "Container runtime",
		"cluster.capacity":         "Capacity",
		"cluster.unreachable_hint": "Cluster state is unavailable because the container runtime did not answer.",
		"cluster.unknown_hint":     "This value is not known yet — it is not zero.",
	})

	// counted phrases — six Arabic categories, two English
	registerPlural(EN, "plural.stacks", PluralForms{One: "{n} stack", Other: "{n} stacks"})
	registerPlural(EN, "plural.services", PluralForms{One: "{n} service", Other: "{n} services"})
	registerPlural(EN, "plural.nodes", PluralForms{One: "{n} node", Other: "{n} nodes"})
	registerPlural(EN, "plural.revisions", PluralForms{One: "{n} revision", Other: "{n} revisions"})
	registerPlural(EN, "plural.backups", PluralForms{One: "{n} backup", Other: "{n} backups"})
	registerPlural(EN, "plural.certificates", PluralForms{One: "{n} certificate", Other: "{n} certificates"})
	registerPlural(EN, "plural.users", PluralForms{One: "{n} user", Other: "{n} users"})
	registerPlural(EN, "plural.webhooks", PluralForms{One: "{n} webhook", Other: "{n} webhooks"})
	registerPlural(EN, "plural.secrets", PluralForms{One: "{n} secret", Other: "{n} secrets"})
	registerPlural(EN, "plural.configs", PluralForms{One: "{n} config", Other: "{n} configs"})
	registerPlural(EN, "plural.keys", PluralForms{One: "{n} key", Other: "{n} keys"})
	registerPlural(EN, "plural.tasks", PluralForms{One: "{n} task", Other: "{n} tasks"})
	registerPlural(EN, "plural.lines", PluralForms{One: "{n} line", Other: "{n} lines"})
	registerPlural(EN, "plural.cores", PluralForms{One: "{n} core", Other: "{n} cores"})
	registerPlural(EN, "plural.seconds", PluralForms{One: "{n} second", Other: "{n} seconds"})
	registerPlural(EN, "plural.minutes", PluralForms{One: "{n} minute", Other: "{n} minutes"})
	registerPlural(EN, "plural.hours", PluralForms{One: "{n} hour", Other: "{n} hours"})
	registerPlural(EN, "plural.days", PluralForms{One: "{n} day", Other: "{n} days"})
}

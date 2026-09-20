package i18n

// English — cluster overview and the cluster vocabulary shared by other pages.

func init() {
	register(EN, map[string]string{
		"overview.title":               "Cluster overview",
		"overview.sub":                 "{0}\u00a0· Docker {1}\u00a0· {2}\u00a0· {3}",
		"overview.sub_unknown":         "Cluster details unavailable",
		"overview.nodes_title":         "Nodes",
		"overview.nodes_unknown_title": "Node list unavailable",
		"overview.nodes_unknown_body":  "Unable to load nodes. Refresh the page or check the connection in Settings.",
		"overview.nodes_empty_title":   "No nodes reported",
		"overview.nodes_empty_body":    "Check the API address and token in Settings.",
		"overview.not_manager_body":    "This host is not a manager, so cluster metrics are unavailable. You can still manage applications, certificates, and backups.",

		"cluster.state_foot":    "Current cluster status",
		"cluster.nodes_foot":    "In this cluster",
		"cluster.managers_foot": "Manage the cluster",
		"cluster.host_cores":    "On this host",
		"cluster.memory_foot":   "Total RAM",

		"common.availability":  "Availability",
		"common.copy_hostname": "Copy hostname",

		"st.error":        "Error",
		"st.down":         "Down",
		"st.avail_active": "Schedulable",
		"st.avail_pause":  "Paused",
		"st.avail_drain":  "Draining",

		"err.banner_body":        "Try again, or check the API address and token in Settings.",
		"err.api_not_configured": "The cluster API is not configured",
		"err.api_unreachable":    "The cluster API did not answer",
		"err.cluster_info":       "Cluster details could not be read",
		"err.nodes":              "The node list could not be read",
	})

	registerPlural(EN, "plural.nodes", PluralForms{One: "{n} node", Other: "{n} nodes"})
}

// Usage page: the config/secret reference graph (nav.usage + /web/usage).
func init() {
	register(EN, map[string]string{
		"usage.sub":         "Which stacks reference each config and secret. The list is computed from every stack's latest rendered compose, so a name that no stack references is safe to delete.",
		"usage.sub_unknown": "Which stacks reference each config and secret. The daemon did not answer, so this list is unknown rather than empty.",

		"usage.configs_title": "Configs",
		"usage.configs_sub":   "Each config and the stacks that mount it.",
		"usage.secrets_title": "Secrets",
		"usage.secrets_sub":   "Each secret and the stacks that mount it. Only the names are read; the values never leave the daemon.",

		"usage.col_name":   "Name",
		"usage.col_stacks": "Referenced by",
		"usage.col_count":  "Stacks",

		"usage.unused_pill": "unused",
		"usage.no_stacks":   "no stack references it",

		"usage.empty_title": "Nothing is referenced yet",
		"usage.empty_body":  "Configs and secrets appear here as soon as a stack's rendered compose mounts one.",

		"usage.unknown_title": "Usage graph unavailable",
		"usage.unknown_body":  "The daemon did not answer the usage request, so this page cannot say which names are still in use. An empty list here would read as \"nothing is used\", which is not what happened.",

		"usage.stat_configs":     "Configs",
		"usage.stat_secrets":     "Secrets",
		"usage.stat_unused":      "Unused",
		"usage.stat_unused_foot": "{0} of them are referenced by no stack.",
		"usage.stat_unused_body": "Names no stack references; deleting one changes nothing that is deployed.",

		"usage.foot": "Computed from the latest rendered compose of every stack. A stack that has never been deployed has no rendered compose, so the references it would make are not counted.",

		"err.usage":                                "Could not read the config and secret usage graph",
		"clustersettings.title":                    "Cluster settings",
		"clustersettings.sub":                      "Settings the daemon stores for every node. A save is applied by each node on its next refresh.",
		"clustersettings.empty_title":              "No settings to edit",
		"clustersettings.empty_body":               "The daemon reported none of the editable settings. Nothing was changed.",
		"clustersettings.sect_core":                "Core",
		"clustersettings.sect_core_sub":            "Where the cluster keeps its state, and the name it answers on.",
		"clustersettings.volume_root":              "Volume root",
		"clustersettings.volume_root_hint":         "Absolute path every node uses for stack volumes and secrets, for example /srv/pmcluster.",
		"clustersettings.domain":                   "Domain",
		"clustersettings.domain_hint":              "Domain the edge serves, for example example.com. Leave it empty to keep the daemon's own default.",
		"clustersettings.sect_backup":              "Backups",
		"clustersettings.backup_all_nodes":         "Back up every node",
		"clustersettings.backup_all_nodes_hint":    "When on, a requested backup runs on all nodes instead of only the node that took the request.",
		"clustersettings.sect_sso":                 "Single sign-on",
		"clustersettings.sect_sso_sub":             "Leave it off to keep the daemon's own login. Turning it on hands sign-in to the provider below.",
		"clustersettings.sso_enabled":              "Enable single sign-on",
		"clustersettings.sso_enabled_hint":         "When on, sign-in is delegated to the OIDC provider and the built-in password form is not used.",
		"clustersettings.sso_provider":             "Provider",
		"clustersettings.sso_provider_hint":        "Provider name the daemon knows, for example github or google.",
		"clustersettings.sso_client_id":            "Client ID",
		"clustersettings.sso_client_id_hint":       "OAuth client identifier issued by the provider.",
		"clustersettings.sso_client_secret":        "Client secret",
		"clustersettings.sso_secret_set":           "A secret is stored \u2014 type to replace it",
		"clustersettings.sso_secret_none":          "No secret stored",
		"clustersettings.sso_secret_hint":          "Leave it empty to keep the stored secret. It is never shown again.",
		"clustersettings.sso_github_org":           "GitHub organisation",
		"clustersettings.sso_github_org_hint":      "Only members of this organisation may sign in. Leave it empty to accept any GitHub account.",
		"clustersettings.sso_cookie_expire":        "Session lifetime",
		"clustersettings.sso_cookie_expire_hint":   "How long one sign-in lasts, for example 24h or 720h.",
		"clustersettings.sect_edge":                "Edge and built-in login",
		"clustersettings.edge_login_disabled":      "Disable the built-in login at the edge",
		"clustersettings.edge_login_disabled_hint": "When on, the edge stops serving its own login page. Make sure SSO or another door works first.",
		"clustersettings.traefik_admin_user":       "Traefik admin user",
		"clustersettings.traefik_admin_user_hint":  "User name for the edge's own dashboard, where it is exposed.",
		"clustersettings.oo_admin_email":           "Admin email",
		"clustersettings.oo_admin_email_hint":      "Address the daemon uses for its own notices.",
		"clustersettings.save":                     "Save settings",
		"clustersettings.foot":                     "Only the keys on this page can be edited from the console. Everything else is set in the daemon's environment or configuration file.",
	})
}

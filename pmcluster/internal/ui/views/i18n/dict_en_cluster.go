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

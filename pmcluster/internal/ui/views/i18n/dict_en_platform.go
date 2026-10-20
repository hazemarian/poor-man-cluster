package i18n

// English dictionary — platform surfaces that used to be reachable only from a
// shell: Docker registry credentials, the bootstrap credentials the platform
// mints for its bundled components, the daemon's own logs, and secret
// verification.
//
// Keys are prefixed by the surface that owns them (registries.*, credentials.*,
// logs.*, inventory.verify_*). Strings registered elsewhere (common.*, nav.*,
// err.*, settings.conn_missing_body) are reused, never repeated here.
func init() {
	register(EN, map[string]string{
		"nav.logs": "Control plane logs",

		"settings.platform_card_title":       "Platform credentials, registries and logs",
		"settings.platform_card_registries":  "Registries",
		"settings.platform_card_credentials": "Bootstrap credentials",
		"settings.platform_card_logs":        "Control plane logs",
		"settings.platform_card_body":        "Log in to a private registry so workers can pull its images, rotate the passwords the platform generated for its own components, or read what the daemon logged.",

		"registries.title":          "Registries",
		"registries.sub":            "Credentials for private image registries. A password is sent once and never read back.",
		"registries.back":           "Settings",
		"registries.list_title":     "Configured registries",
		"registries.col_host":       "Host",
		"registries.col_user":       "Username",
		"registries.col_configured": "Added",
		"registries.remove_confirm": "Remove the credentials for {0} and log this host out? Workers will no longer be able to pull its images.",
		"registries.empty_title":    "No registries configured",
		"registries.empty_body":     "Add one to pull images from a private registry. Until then a deploy that needs one fails with an image pull error.",
		"registries.add_title":      "Add or replace a registry",
		"registries.add_sub":        "This logs in on the manager and stores the credential encrypted. Workers receive it on the next deploy, so re-deploy the stack afterwards.",
		"registries.field_host":     "Registry host",
		"registries.field_user":     "Username",
		"registries.field_password": "Password or access token",
		"registries.hint_host":      "For example ghcr.io, docker.io or registry.example.com.",
		"registries.hint_password":  "Used once for docker login and stored encrypted; it is never shown again.",
		"registries.action_add":     "Save and log in",
		"registries.foot":           "The password is write-only: it is used for docker login and stored encrypted, and no page can read it back.",
		"registries.err_add":        "Could not add the registry",
		"registries.err_remove":     "Could not remove the registry",
		"registries.err_list":       "Could not read the registry list",
		"registries.msg_added":      "Registry {0} saved and logged in.",
		"registries.msg_removed":    "Registry {0} removed.",

		"credentials.title":                 "Bootstrap credentials",
		"credentials.sub":                   "Passwords the platform generated for its bundled components. They can be rotated here but not read back.",
		"credentials.back":                  "Settings",
		"credentials.list_title":            "Managed credentials",
		"credentials.col_name":              "Name",
		"credentials.col_kind":              "Kind",
		"credentials.col_user":              "Username",
		"credentials.col_secret":            "Swarm secret",
		"credentials.col_password":          "New password",
		"credentials.col_rotated":           "Last rotated",
		"credentials.never_rotated":         "Never",
		"credentials.action_rotate":         "Rotate",
		"credentials.rotate_confirm":        "Rotate the password for {0}? The service that consumes it will be restarted.",
		"credentials.empty_title":           "No managed credentials",
		"credentials.empty_body":            "They are minted the first time the cluster is brought up.",
		"credentials.foot":                  "Rotation generates a new password, updates the Swarm secret and restarts the consuming service. The new password is shown once and cannot be read back.",
		"credentials.once_title":            "Copy this password now",
		"credentials.once_body":             "It is shown once. Neither this console nor the CLI can print it again.",
		"credentials.once_username_changed": "The username changed as well.",
		"credentials.err_rotate":            "Could not rotate the credential",
		"credentials.err_list":              "Could not read the credentials",
		"credentials.msg_rotated":           "Credential {0} rotated.",

		"logs.title":         "Control plane logs",
		"logs.sub":           "What the daemon itself logged. A service's own output lives on that service's page.",
		"logs.panel_title":   "Daemon log",
		"logs.tail_label":    "Lines",
		"logs.since_label":   "Period",
		"logs.since_any":     "All",
		"logs.since_week":    "Last 7 days",
		"logs.all_files":     "Include older daily files",
		"logs.foot":          "{0} lines shown",
		"logs.truncated":     "older lines omitted",
		"logs.empty_title":   "No log entries",
		"logs.empty_body":    "the daemon has not written any in this period",
		"logs.level_unknown": "log",
		"logs.err_read":      "Could not read the control plane log",

		"inventory.verify_title":         "Verify the value of {0}",
		"inventory.verify_sub":           "The plaintext is never stored, so a value can be checked but not shown.",
		"inventory.verify_field":         "Candidate value",
		"inventory.verify_hint":          "Only whether it matches comes back.",
		"inventory.verify_action":        "Check",
		"inventory.verify_match":         "Match",
		"inventory.verify_match_body":    "This is the value stored under that name.",
		"inventory.verify_no_match":      "No match",
		"inventory.verify_no_match_body": "This is not the value stored under that name.",
		"inventory.err_verify":           "Could not verify the secret",
	})

	// Counts go through registerPlural (never a bare "{0} <noun>"): Arabic needs all six CLDR
	// categories, registered on the AR side. See docs/console-i18n-contract.md.
	registerPlural(EN, "plural.registries", PluralForms{One: "{n} registry", Other: "{n} registries"})
	registerPlural(EN, "plural.credentials", PluralForms{One: "{n} credential", Other: "{n} credentials"})
}

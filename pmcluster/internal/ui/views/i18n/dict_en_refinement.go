package i18n

func init() {
	register(EN, map[string]string{
		"preferences.label":          "Language, theme and palette",
		"preferences.light":          "Light",
		"preferences.dark":           "Dark",
		"preferences.to_light":       "Switch to light mode",
		"preferences.to_dark":        "Switch to dark mode",
		"preferences.english":        "Switch to English",
		"preferences.arabic":         "Switch to Arabic",
		"preferences.palette":        "Colour palette",
		"preferences.palette_orange": "Orange",
		"preferences.palette_purple": "Purple",
		"preferences.palette_blue":   "Blue",
		"preferences.palette_red":    "Red",
		"preferences.palette_green":  "Green",
		"preferences.palette_teal":   "Teal",
		"nav.skip_content":           "Skip to content",
		"overview.workspace":         "Quick access",
		"overview.workspace_sub":     "Common cluster tasks.",
		"overview.delivery_title":    "Deploy an application",
		"overview.delivery_body":     "Add your manifest, review the settings, and deploy.",
		"overview.stacks_hint":       "Applications, revisions, and rollbacks",
		"overview.services_hint":     "Replica health, tasks, and live logs",
		"overview.backups_hint":      "Volume snapshots and backup history",
		"auth.welcome":               "Welcome back",
		"auth.welcome_body":          "Sign in to manage your cluster.",
		"auth.intro_title":           "Your infrastructure.\nUnder your control.",
		"auth.intro_body":            "Deploy applications, inspect services, and manage your cluster from one focused workspace.",
		"auth.intro_foot":            "A small stack. A capable control plane.",
	})
}

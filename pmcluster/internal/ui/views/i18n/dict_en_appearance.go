package i18n

// English dictionary — the Appearance panel in Settings, where an operator picks
// the console's accent color. Keys are prefixed appearance.*.
func init() {
	register(EN, map[string]string{
		"appearance.title":         "Appearance",
		"appearance.sub":           "Pick the accent color for buttons, the active menu item and focus rings. Status colors stay the same. Your choice is saved in this browser.",
		"appearance.accent":        "Accent color",
		"appearance.accent_orange": "Orange",
		"appearance.accent_blue":   "Blue",
		"appearance.accent_violet": "Violet",
		"appearance.accent_rose":   "Rose",
		"appearance.accent_cyan":   "Cyan",
	})
}

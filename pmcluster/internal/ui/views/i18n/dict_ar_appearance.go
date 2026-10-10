package i18n

// Arabic dictionary — the Appearance panel in Settings, where an operator picks
// the console's accent color.
//
// Terminology: «لون التمييز» for the accent color, and «ألوان الحالة» for the
// green/amber/red status colors, which the accent never changes.
func init() {
	register(AR, map[string]string{
		"appearance.title":         "المظهر",
		"appearance.sub":           "اختر لون التمييز للأزرار والعنصر النشط في القائمة وحلقات التركيز. تبقى ألوان الحالة كما هي. يُحفظ اختيارك في هذا المتصفح.",
		"appearance.accent":        "لون التمييز",
		"appearance.accent_orange": "برتقالي",
		"appearance.accent_blue":   "أزرق",
		"appearance.accent_violet": "بنفسجي",
		"appearance.accent_rose":   "وردي",
		"appearance.accent_cyan":   "سماوي",
	})
}

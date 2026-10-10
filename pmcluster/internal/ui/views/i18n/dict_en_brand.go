package i18n

// English dictionary — the two-part wordmark. The first part renders in the
// text color and the second in the accent color, so they are separate keys.
func init() {
	register(EN, map[string]string{
		"brand.name_lead":   "Poor Man's",
		"brand.name_accent": "Cluster",
	})
}

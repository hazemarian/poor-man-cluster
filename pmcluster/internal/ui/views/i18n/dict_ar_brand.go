package i18n

// Arabic dictionary — the two-part wordmark. The product name is a proper noun
// and stays in Latin script in both languages; the lockup is set left-to-right
// inside an RTL page so the apostrophe and word order survive.
func init() {
	register(AR, map[string]string{
		"brand.name_lead":   "Poor Man's",
		"brand.name_accent": "Cluster",
	})
}

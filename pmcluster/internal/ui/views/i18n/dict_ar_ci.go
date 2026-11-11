package i18n

// Arabic dictionary — لوحة الـ CI. Mirrors dict_en_ci.go key for key: a missing key
// renders as the key itself, which on this panel would look like a broken row.
func init() {
	register(AR, map[string]string{
		"nav.ci": "التكامل المستمر",

		"ci.title": "التكامل المستمر",
		"ci.sub":   "أحدث تشغيل لكل مستودع، الأحدث نشاطًا أولًا.",

		"ci.queued":       "في الانتظار",
		"ci.running":      "قيد التشغيل",
		"ci.failed":       "فشل",
		"ci.read":         "عمر البيانات",
		"ci.cached":       "من الذاكرة",
		"ci.col_repo":     "المستودع",
		"ci.col_state":    "الحالة",
		"ci.col_workflow": "سير العمل",
		"ci.col_sha":      "الإيداع",
		"ci.col_updated":  "آخر تحديث",
		"ci.col_detail":   "التفصيل",

		"ci.state_queued":     "في الانتظار",
		"ci.state_running":    "قيد التشغيل",
		"ci.state_ok":         "ناجح",
		"ci.state_failed":     "فاشل",
		"ci.state_cancelled":  "ملغى",
		"ci.state_superseded": "استُبدل",
		"ci.state_unreadable": "غير مقروء",
		"ci.state_none":       "لا تشغيلات",
		"ci.state_other":      "—",

		"ci.open":    "فتح",
		"ci.no_rows": "لا يوجد مستودع أبلغ عن تشغيل بعد.",

		"ci.not_configured":      "تحتاج لوحة التكامل المستمر إلى تطبيق GitHub.",
		"ci.not_configured_hint": "بدون رمز التركيب لا يمكن قراءة Actions، ولوحة فارغة تُقرأ كأن كل شيء ناجح.",

		"ci.inferred_tip": "استنتاج لا تقرير: GitHub لا يذكر من ألغى التشغيل. يوجد تشغيل أحدث لنفس سير العمل، فالأرجح أن إيداعًا لاحقًا استبدله.",

		"ci.footnote": "* استنتاج — GitHub لا يذكر سبب إلغاء التشغيل. وما عداه مقروء من واجهة Actions ومحفوظ دقيقة واحدة.",

		"ci.err_load":    "تعذّرت قراءة لوحة التكامل المستمر.",
		"ci.err_partial": "تُعرض آخر قراءة ناجحة — تعذّر تحديث قائمة المستودعات.",
	})
}

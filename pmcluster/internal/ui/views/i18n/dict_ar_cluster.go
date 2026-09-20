package i18n

// Arabic — cluster overview and shared cluster vocabulary.
//
// Notes on this domain's wording:
//   · الخادم (daemon) is the pmcluster/docker daemon on a host; استخدام
//     "الخدمة" here would collide with الخدمة = swarm service.
//   · Node/manager/worker follow Docker's Arabic glosses: عقدة / مدير / عامل.
//   · "Schedulable" is rendered as قابلية الجدولة, the term used by schedulers,
//     rather than a literal "active".

func init() {
	register(AR, map[string]string{
		"overview.title":               "نظرة عامة على العنقود",
		"overview.sub":                 "{0}\u00a0· Docker {1}\u00a0· {2}\u00a0· {3}",
		"overview.sub_unknown":         "تفاصيل العنقود غير متاحة",
		"overview.nodes_title":         "العقد",
		"overview.nodes_unknown_title": "قائمة العقد غير متاحة",
		"overview.nodes_unknown_body":  "تعذّر تحميل العقد. حدّث الصفحة أو تحقّق من الاتصال في الإعدادات.",
		"overview.nodes_empty_title":   "لا توجد عقد",
		"overview.nodes_empty_body":    "تحقّق من عنوان API والرمز في الإعدادات.",
		"overview.not_manager_body":    "هذا الخادم ليس مديرًا، لذا مقاييس العنقود غير متاحة. يمكنك إدارة التطبيقات والشهادات والنسخ الاحتياطية.",

		"cluster.state_foot":    "حالة العنقود الحالية",
		"cluster.nodes_foot":    "ضمن هذا العنقود",
		"cluster.managers_foot": "لإدارة العنقود",
		"cluster.host_cores":    "على هذا الخادم",
		"cluster.memory_foot":   "إجمالي الذاكرة",

		"common.availability":  "قابلية الجدولة",
		"common.copy_hostname": "نسخ اسم الخادم",

		"st.error":        "خطأ",
		"st.down":         "متوقفة",
		"st.avail_active": "قابلة للجدولة",
		"st.avail_pause":  "متوقفة مؤقتًا",
		"st.avail_drain":  "قيد الإخلاء",

		"err.banner_body":        "أعد المحاولة، أو تحقّق من عنوان API والرمز في الإعدادات.",
		"err.api_not_configured": "واجهة API للعنقود غير مُهيّأة",
		"err.api_unreachable":    "لم تستجب واجهة API للعنقود",
		"err.cluster_info":       "تعذّرت قراءة تفاصيل العنقود",
		"err.nodes":              "تعذّرت قراءة قائمة العقد",
	})

	registerPlural(AR, "plural.nodes", PluralForms{
		Zero:   "لا توجد عقد",
		One:    "عقدة واحدة",
		TwoNom: "عقدتان",
		TwoObl: "عقدتين",
		Few:    "{n} عقد",
		Many:   "{n} عقدةً",
		Other:  "{n} عقدة",
	})
}

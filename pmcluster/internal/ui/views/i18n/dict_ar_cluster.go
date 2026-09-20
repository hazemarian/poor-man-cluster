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

// صفحة الاستخدام: مخطط استشهاد التهيئات والأسرار (nav.usage و/web/usage).
//
// Terms follow the pages that already name these objects: الحزمة for stack,
// التهيئة for config, السر for secret, and الاستشهاد for the reference a stack
// makes. "غير مستشهَد بها" is the passive that marks a name nothing mounts.
func init() {
	register(AR, map[string]string{
		"usage.sub":         "الحزم التي تستشهد بكل تهيئة وسرّ. تُحسب القائمة من أحدث ملف Compose معروض لكل حزمة، لذا الاسم الذي لا حزمة تستشهد به آمن للحذف.",
		"usage.sub_unknown": "الحزم التي تستشهد بكل تهيئة وسرّ. لم يجِب الخادم، فهذه القائمة غير معروفة لا فارغة.",

		"usage.configs_title": "التهيئات",
		"usage.configs_sub":   "كل تهيئة والحزم التي تركّبها.",
		"usage.secrets_title": "الأسرار",
		"usage.secrets_sub":   "كل سرّ والحزم التي تركّبه. تُقرأ الأسماء فقط، ولا تخرج القيم من الخادم.",

		"usage.col_name":   "الاسم",
		"usage.col_stacks": "تستشهد بها",
		"usage.col_count":  "الحزم",

		"usage.unused_pill": "غير مستشهَد بها",
		"usage.no_stacks":   "لا حزمة تستشهد بها",

		"usage.empty_title": "لا شيء مستشهَد به بعد",
		"usage.empty_body":  "تظهر التهيئات والأسرار هنا بمجرد أن تركّبها حزمة في ملف Compose المعروض.",

		"usage.unknown_title": "مخطط الاستخدام غير متاح",
		"usage.unknown_body":  "لم يجِب الخادم عن طلب الاستخدام، فلا تستطيع هذه الصفحة بيان الأسماء التي لا تزال قيد الاستخدام. القائمة الفارغة هنا كانت ستُقرأ بمعنى «لا شيء مستخدَم»، وهذا ما لم يحدث.",

		"usage.stat_configs":     "التهيئات",
		"usage.stat_secrets":     "الأسرار",
		"usage.stat_unused":      "غير مستشهَد بها",
		"usage.stat_unused_foot": "لا حزمة تستشهد بـ{0} منها.",
		"usage.stat_unused_body": "أسماء لا تستشهد بها أي حزمة؛ حذف أحدها لا يغيّر شيئًا منشورًا.",

		"usage.foot": "يُحسب من أحدث ملف Compose معروض لكل حزمة. الحزمة التي لم تُطلق قط ليس لها ملف معروض، فلا تُحسب الاستشهادات التي كانت ستُحدثها.",

		"err.usage":                                "تعذّرت قراءة مخطط استخدام التهيئات والأسرار",
		"clustersettings.title":                    "إعدادات العنقود",
		"clustersettings.sub":                      "إعدادات يحفظها الخادم لكل العقد. تطبّقها كل عقدة عند تحديثها التالي.",
		"clustersettings.empty_title":              "لا توجد إعدادات للتعديل",
		"clustersettings.empty_body":               "لم يُرجِع الخادم أي إعداد قابل للتعديل. لم يتغيّر شيء.",
		"clustersettings.sect_core":                "الأساسيات",
		"clustersettings.sect_core_sub":            "أين يحفظ العنقود حالته، وبأي اسم يستجيب.",
		"clustersettings.volume_root":              "جذر الأحجام",
		"clustersettings.volume_root_hint":         "مسار مطلق تستخدمه كل عقدة لأحجام الحزم وأسرارها، مثل /srv/pmcluster.",
		"clustersettings.domain":                   "النطاق",
		"clustersettings.domain_hint":              "النطاق الذي تخدمه الحافة، مثل example.com. اتركه فارغًا للإبقاء على افتراضي الخادم.",
		"clustersettings.sect_backup":              "النسخ الاحتياطي",
		"clustersettings.backup_all_nodes":         "النسخ من كل العقد",
		"clustersettings.backup_all_nodes_hint":    "عند التشغيل، يجري النسخ المطلوب على كل العقد بدل العقدة التي تلقّت الطلب وحدها.",
		"clustersettings.sect_sso":                 "الدخول الموحّد",
		"clustersettings.sect_sso_sub":             "اتركه متوقفًا للإبقاء على دخول الخادم المدمج. تشغيله يسلّم الدخول إلى المزوّد أدناه.",
		"clustersettings.sso_enabled":              "تشغيل الدخول الموحّد",
		"clustersettings.sso_enabled_hint":         "عند التشغيل، يُفوَّض الدخول إلى مزوّد OIDC ولا يُستخدم نموذج كلمة المرور المدمج.",
		"clustersettings.sso_provider":             "المزوّد",
		"clustersettings.sso_provider_hint":        "اسم المزوّد الذي يعرفه الخادم، مثل github أو google.",
		"clustersettings.sso_client_id":            "معرّف العميل",
		"clustersettings.sso_client_id_hint":       "معرّف عميل OAuth الصادر من المزوّد.",
		"clustersettings.sso_client_secret":        "سرّ العميل",
		"clustersettings.sso_secret_set":           "يوجد سرّ محفوظ — اكتب لاستبداله",
		"clustersettings.sso_secret_none":          "لا يوجد سرّ محفوظ",
		"clustersettings.sso_secret_hint":          "اتركه فارغًا للإبقاء على السرّ المحفوظ. ولا يُعرض مرة أخرى.",
		"clustersettings.sso_github_org":           "مؤسسة GitHub",
		"clustersettings.sso_github_org_hint":      "لا يسجّل الدخول إلا أعضاء هذه المؤسسة. اتركه فارغًا لقبول أي حساب GitHub.",
		"clustersettings.sso_cookie_expire":        "مدة الجلسة",
		"clustersettings.sso_cookie_expire_hint":   "كم يدوم تسجيل الدخول الواحد، مثل 24 ساعة أو 720 ساعة.",
		"clustersettings.sect_edge":                "الحافة والدخول المدمج",
		"clustersettings.edge_login_disabled":      "تعطيل الدخول المدمج في الحافة",
		"clustersettings.edge_login_disabled_hint": "عند التشغيل، تكفّ الحافة عن تقديم صفحة الدخول الخاصة بها. تأكد أولًا أن الدخول الموحّد أو بابًا آخر يعمل.",
		"clustersettings.traefik_admin_user":       "مستخدم مشرف Traefik",
		"clustersettings.traefik_admin_user_hint":  "اسم المستخدم للوحة الحافة الخاصة بها، حيث تكون مكشوفة.",
		"clustersettings.oo_admin_email":           "بريد المسؤول",
		"clustersettings.oo_admin_email_hint":      "العنوان الذي يستخدمه الخادم لإشعاراته.",
		"clustersettings.save":                     "حفظ الإعدادات",
		"clustersettings.foot":                     "لا يمكن تعديل من لوحة التحكم سوى المفاتيح المعروضة في هذه الصفحة. وما عداها يُضبط في بيئة الخادم أو ملف تهيئته.",
	})
}

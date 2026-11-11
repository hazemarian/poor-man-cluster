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
		"overview.apply_warnings_title": "آخر تطبيق للمنصة لم يكمل {0} خطوة",
		"overview.apply_warnings_body":  "لا شيء متوقف، لكن المنصة تعمل بأقل مما طُلب منها — هذه هي الخطوات التي تراجعت واستمرت:",
		"overview.apply_warnings_when":  "مسجّل عند {0} بواسطة {1}",
		"overview.version_skew_title":   "{0} مديراً يشغّل إصدار pmcluster مختلفاً",
		"overview.version_skew_body":    "كل مدير ينشر إصداره عند بدء التشغيل وكل عشر دقائق. اختلاف الإصدارات يعني أن عقدة ستتصرف بشكل مختلف عن التي تقرأ منها — رقِّ المتأخرة:",
		"overview.title":                "نظرة عامة على العنقود",
		"overview.sub":                  "{0}\u00a0· Docker {1}\u00a0· {2}\u00a0· {3}",
		"overview.sub_unknown":          "تفاصيل العنقود غير متاحة",
		"overview.nodes_title":          "العقد",
		"overview.nodes_unknown_title":  "قائمة العقد غير متاحة",
		"overview.nodes_unknown_body":   "تعذّر تحميل العقد. حدّث الصفحة أو تحقّق من الاتصال في الإعدادات.",
		"overview.nodes_empty_title":    "لا توجد عقد",
		"overview.nodes_empty_body":     "تحقّق من عنوان API والرمز في الإعدادات.",
		"overview.not_manager_body":     "هذا الخادم ليس مديرًا، لذا مقاييس العنقود غير متاحة. يمكنك إدارة التطبيقات والشهادات والنسخ الاحتياطية.",

		// لوحة موارد العقد في النظرة العامة.
		"nodestat.title":            "موارد العقد",
		"nodestat.empty_title":      "لا توجد إحصاءات عقد",
		"nodestat.empty_body":       "يُبلّغ خادم كل عقدة عن حِملها وذاكرتها وأكثر عملياتها استهلاكًا. لم يصل شيء بعد — قد يكون المجمِّع قيد النشر، أو تنتظر اللوحة العيّنة التالية.",
		"nodestat.load":             "الحِمل (دقيقة)",
		"nodestat.memory":           "الذاكرة",
		"nodestat.top_rss":          "الأعلى بالذاكرة",
		"nodestat.top_cpu":          "الأعلى بالمعالج",
		"nodestat.sample_age":       "العيّنة",
		"nodestat.cores":            "أنوية",
		"nodestat.free":             "متاحة",
		"nodestat.no_procs":         "لا شيء مُبلَّغ",
		"nodestat.stale":            "قديمة",
		"nodestat.overloaded":       "حِمل مرتفع",
		"nodestat.overloaded_hint":  "متوسط الحِمل يتجاوز 1.5 ضعف أنوية هذه العقدة: العمل ينتظر بالفعل.",
		"nodestat.mem_low":          "ذاكرة منخفضة",
		"nodestat.mem_low_hint":     "أقل من 15% من الذاكرة متاحة؛ هذه العقدة قريبة من التبديل إلى القرص.",
		"nodestat.docker_down":      "دوكر لا يستجيب",
		"nodestat.docker_down_hint": "لم يستجب محرّك دوكر على هذه العقدة لخادمها، فلا يستطيع تشغيل أو إيقاف أي شيء.",

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

		"err.usage":                                      "تعذّرت قراءة مخطط استخدام التهيئات والأسرار",
		"clustersettings.title":                          "إعدادات العنقود",
		"clustersettings.sub":                            "إعدادات يحفظها الخادم لكل العقد. تطبّقها كل عقدة عند تحديثها التالي.",
		"clustersettings.empty_title":                    "لا توجد إعدادات للتعديل",
		"clustersettings.empty_body":                     "لم يُرجِع الخادم أي إعداد قابل للتعديل. لم يتغيّر شيء.",
		"clustersettings.sect_core":                      "الأساسيات",
		"clustersettings.sect_core_sub":                  "أين يحفظ العنقود حالته، وبأي اسم يستجيب.",
		"clustersettings.volume_root":                    "جذر الأحجام",
		"clustersettings.volume_root_hint":               "مسار مطلق تستخدمه كل عقدة لأحجام الحزم وأسرارها، مثل /srv/pmcluster.",
		"clustersettings.domain":                         "النطاق",
		"clustersettings.domain_hint":                    "النطاق الذي تخدمه الحافة، مثل example.com. اتركه فارغًا للإبقاء على افتراضي الخادم.",
		"clustersettings.sect_backup":                    "النسخ الاحتياطي",
		"clustersettings.backup_enabled":                 "تفعيل النسخ الاحتياطي لهذا العنقود",
		"clustersettings.backup_enabled_hint":            "مؤقّت الساعة لا يقرّر شيئًا: يسأل فقط هل حان موعد النسخ. الجدول أدناه هو ما يقرّر. إيقاف هذا يُبقي المؤقّت يعمل دون أن يفعل شيئًا.",
		"clustersettings.backup_cron":                    "موعد التشغيل (صيغة cron بخمسة حقول)",
		"clustersettings.backup_cron_hint":               "مثال: 0 * * * * كل ساعة، أو 0 2 * * * الساعة الثانية فجرًا. لا تُقبل الخطوات أو النطاقات. لا تعدّل مؤقّت systemd فهو نبضة وليس الجدول.",
		"clustersettings.backup_retention_days":          "مدة الاحتفاظ بالنسخ (أيام)",
		"clustersettings.backup_retention_days_hint":     "كل نسخة من الساعات الثماني والأربعين الأخيرة تُحفظ ساعةً بساعة، ثم نسخة واحدة يوميًا حتى هذا العمر. الافتراضي ٣. كل عقدة تقرأ هذا في تشغيلها التالي؛ والمدة الأطول تستهلك مساحة أكبر في كل حاوية تخزين.",
		"clustersettings.sect_sso":                       "الدخول الموحّد",
		"clustersettings.sect_sso_sub":                   "اتركه متوقفًا للإبقاء على دخول الخادم المدمج. تشغيله يسلّم الدخول إلى المزوّد أدناه.",
		"clustersettings.sso_enabled":                    "تشغيل الدخول الموحّد",
		"clustersettings.sso_enabled_hint":               "عند التشغيل، يُفوَّض الدخول إلى مزوّد OIDC ولا يُستخدم نموذج كلمة المرور المدمج.",
		"clustersettings.sso_provider":                   "المزوّد",
		"clustersettings.sso_provider_hint":              "اسم المزوّد الذي يعرفه الخادم، مثل github أو google.",
		"clustersettings.sso_client_id":                  "معرّف العميل",
		"clustersettings.sso_client_id_hint":             "معرّف عميل OAuth الصادر من المزوّد.",
		"clustersettings.sso_client_secret":              "سرّ العميل",
		"clustersettings.sso_secret_set":                 "يوجد سرّ محفوظ — اكتب لاستبداله",
		"clustersettings.sso_secret_none":                "لا يوجد سرّ محفوظ",
		"clustersettings.sso_secret_hint":                "اتركه فارغًا للإبقاء على السرّ المحفوظ. ولا يُعرض مرة أخرى.",
		"clustersettings.sso_github_org":                 "مؤسسة GitHub",
		"clustersettings.sso_github_org_hint":            "لا يسجّل الدخول إلا أعضاء هذه المؤسسة. اتركه فارغًا لقبول أي حساب GitHub.",
		"clustersettings.sso_github_repos":               "مستودعات GitHub",
		"clustersettings.sso_github_repos_hint":          "قائمة مفصولة بفواصل بصيغة org/repo (مثال acme/app,acme/web). لا يسجّل الدخول إلا المستخدمون المخوّلون بهذه المستودعات. اتركه فارغًا لقبول جميع أعضاء المؤسسة.",
		"clustersettings.sso_cookie_expire":              "مدة الجلسة",
		"clustersettings.sso_cookie_expire_hint":         "كم يدوم تسجيل الدخول الواحد، مثل 24 ساعة أو 720 ساعة.",
		"clustersettings.sect_edge":                      "الحافة والدخول المدمج",
		"clustersettings.edge_login_disabled":            "تعطيل الدخول المدمج في الحافة",
		"clustersettings.edge_login_disabled_hint":       "عند التشغيل، تكفّ الحافة عن تقديم صفحة الدخول الخاصة بها. تأكد أولًا أن الدخول الموحّد أو بابًا آخر يعمل.",
		"clustersettings.traefik_admin_user":             "مستخدم مشرف Traefik",
		"clustersettings.traefik_admin_user_hint":        "اسم المستخدم للوحة الحافة الخاصة بها، حيث تكون مكشوفة.",
		"clustersettings.observability_admin_email":      "بريد المسؤول",
		"clustersettings.observability_admin_email_hint": "العنوان الذي يستخدمه الخادم لإشعاراته.",
		"clustersettings.log_level":                      "مستوى السجلات",
		"clustersettings.log_level_hint":                 "كم يكتب الخادم في ملف سجلاته وفي Parseable. يضيف debug ملف compose المُقدَّم الكامل لكل مستوى نشر؛ بينما يسجّل info كل خطوة نشر مع أسماء الخدمات.",
		"clustersettings.sect_runners":                   "مشغّلات CI مستضافة ذاتيًا",
		"clustersettings.sect_runners_sub":               "يمكن للمنصّة تشغيل حاوية مشغّل GitHub Actions واحدة لكل عقدة عاملة مختارة — البديل عندما تنفد دقائق GitHub المستضافة. تُسجَّل المشغّلات على مستوى المؤسسة برمز تثبيت تطبيق GitHub وتحتاج صلاحية Self-hosted runners: Read & write على مستوى المؤسسة في التطبيق.",
		"clustersettings.runners_enabled":                "تشغيل المشغّلات المستضافة ذاتيًا",
		"clustersettings.runners_enabled_hint":           "معطّلة افتراضيًا. تشغيلها يتطلب تطبيق GitHub مهيّأً؛ وعند إطفائها توقف حلقة المطابقة كل مشغّل وتزيله.",
		"clustersettings.runner_org":                     "المؤسسة",
		"clustersettings.runner_org_hint":                "مؤسسة GitHub التي يُسجَّل فيها المشغّل. التسجيل على مستوى المؤسسة، فمشغّل واحد يخدم كل مستودع خاص تختاره مجموعة المشغّلات.",
		"clustersettings.runner_group":                   "مجموعة المشغّلات",
		"clustersettings.runner_group_hint":              "تُنشأ بظهور 'selected' عند غيابها ولا تُغيَّر إلى 'all' أبدًا: المجموعة الظاهرة للجميع تشغّل كود سير العمل من أي مستودع في المؤسسة — بما فيها مستودع يستطيع مهاجم الدفع إليه.",
		"clustersettings.runner_socket_path":             "مقبس docker المفلتر",
		"clustersettings.runner_socket_path_hint":        "مسار مطلق على كل عقدة. يستمع الخادم هنا ويمرّر فقط المسارات التي يحتاجها البناء؛ والمشغّل يربط هذا المقبس، لا /var/run/docker.sock.",
		"clustersettings.runner_socket_proxy":            "توجيه وصول المشغّل إلى docker عبر المقبس المفلتر",
		"clustersettings.runner_socket_proxy_hint":       "مفعّل افتراضيًا، وهو النمط الوحيد المدعوم لسير عمل لا تتحكّم فيه. إطفاؤه يربط مقبس docker الخام للمضيف في المشغّل: صلاحية جذر على تلك العقدة.",
		"clustersettings.runner_nodes_note":              "العقد العاملة المسموح لها بتشغيل مشغّل تُختار من لوحة المشغّلات، حيث يظهر دور كل عقدة وذاكرتها المتاحة. لا يمكن اختيار عقد الإدارة. يبقى الاختيار محفوظًا عند حفظ هذا النموذج.",
		"clustersettings.runner_memory":                  "الذاكرة لكل مشغّل",
		"clustersettings.runner_memory_hint":             "كمية Docker، مثل 2560m أو 3g. القيمة الافتراضية 2560m هي التي أكملت تشغيل govulncheck؛ وعند 1500m أُوقف المشغّل لضيق الذاكرة في منتصف المهمة.",
		"clustersettings.runner_allow_leader":            "السماح بالمشغّلات على القائد",
		"clustersettings.runner_allow_leader_hint":       "معطّلة افتراضيًا: المشغّل يبني الصور وينافس لوحة التحكّم. شغّلها فقط في عنقود أحادي العقدة حيث لا يوجد مضيف آخر.",
		"clustersettings.runners_security":               "أمان: المشغّل يبني عبر مقبس docker المفلتر في الخادم، الذي يسمح بالحاويات والصور والأحجام والشبكات — ويرفض الخدمات والعقد وswarm والأسرار والإعدادات وexec. هذا تقييد لنطاق الأثر لا حدّ أمني: المقبس ما زال أمام خادم يعمل بصلاحية الجذر، فلا تضع مشغّلًا على عقدة إدارة، ولا تسمح لسير عمل من مستودع لا تثق به بالعمل عليه. إطفاء الإعداد يزيل الحاويات ويلغي تسجيل المشغّلات.",
		"clustersettings.save":                           "حفظ الإعدادات",
		"clustersettings.foot":                           "لا يمكن تعديل من لوحة التحكم سوى المفاتيح المعروضة في هذه الصفحة. وما عداها يُضبط في بيئة الخادم أو ملف تهيئته.",
	})
}

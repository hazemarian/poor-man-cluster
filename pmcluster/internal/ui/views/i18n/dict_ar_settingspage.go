package i18n

// Arabic — صفحة الإعدادات في ستة أقسام.
//
// النصوص هنا موجودة لأن ثلاث حالات فشل اكتُشفت بالفشل نفسه: مفتاح مشغّلات بلا
// مؤسسة، ولا عقدة عاملة مختارة، وصلاحية تطبيق تغطي نقاط نهاية خاطئة. لذلك يذكر
// كل قسم ما يحتاجه قبل حقوله، ويسمّي قسم المشغّلات صلاحية GitHub بالضبط.
func init() {
	register(AR, map[string]string{
		// ---------- الاتصال ----------
		"settings.sec_connection":       "الاتصال",
		"settings.sec_connection_sub":   "كيف تصل لوحة التحكم إلى الخادم: العنوان الذي تناديه والرمز الذي تقدّمه.",
		"settings.sec_connection_needs": "محفوظ في مخزن لوحة التحكم هذه. ولا يُكتب أي شيء من هذا القسم في العنقود.",
		"settings.save_connection":      "حفظ الاتصال",

		"settings.offline_notice_title": "الخادم غير مهيّأ",
		"settings.offline_notice_body":  "الأقسام التي تقرأ من العنقود تقول ذلك بدل عرض قيمة افتراضية، ولا يمكن الحفظ منها. اضبط العنوان والرمز في قسم الاتصال أدناه.",

		// ---------- تطبيق GitHub ----------
		"settings.sec_github_needs":    "يتطلب تثبيت التطبيق على المؤسسة بصلاحية Contents وPackages: Read. وتسجيل مشغّلات CI يحتاج إضافةً إلى ذلك Organization → Self-hosted runners: Read & write، ولا يسري تغيير الصلاحية إلا بعد إصدار رمز تركيب جديد.",
		"settings.github_status_title": "المثبَّت الآن",

		// ---------- شريط الحفظ الواحد ----------
		"settings.bar_clean": "لا تغييرات غير محفوظة",
		"settings.bar_dirty": "تغييرات غير محفوظة",
		"settings.bar_scope": "يحفظ الهوية والدخول، ومشغّلات CI، والنسخ الاحتياطي، والإعدادات المتقدّمة. وكل عقدة تأخذ القيم في تحديثها التالي؛ ولا يُنشر أي شيء.",
		"settings.dismiss":   "إغلاق",

		// ---------- جواب الحفظ ----------
		"settings.msg_cluster_saved_changed": "حُفظت الإعدادات. المتغيّر:",
		"settings.msg_cluster_saved_none":    "حُفظت الإعدادات — لم يتغيّر شيء.",
		"settings.err_body_lead":             "رفض الخادم الحفظ وقال:",
		"settings.err_jump":                  "الانتقال إلى القسم",
		"settings.err_flagged":               "مرفوض",
		"settings.err_flagged_body":          "هذا هو القسم الذي رفضه الخادم — ورسالته الكاملة في أعلى الصفحة.",
		"settings.err_permission_hint":       "يبدو هذا فشل صلاحيات: يحتاج التطبيق Organization → Self-hosted runners: Read & write، ولا يصل تغيير الصلاحية إلى الخادم إلا بعد إصدار رمز تركيب جديد — دوّر المفتاح في قسم تطبيق GitHub أو أعد تثبيت التطبيق، ثم شغّل «اختبار الاتصال».",

		// ---------- ملخّص المشغّلات (للقراءة فقط؛ الاختيار في /web/runners) ----------
		"settings.runners_summary_title":   "المسجَّل الآن",
		"settings.runners_summary_off":     "متوقّفة — لا حاوية مشغّل موجودة ولا شيء مسجَّل على GitHub.",
		"settings.runners_summary_nodes":   "العقد العاملة المختارة:",
		"settings.runners_summary_none":    "لم تُختَر أي عقدة عاملة بعد.",
		"settings.runners_summary_unknown": "لم يُبلغ الخادم عن قائمة المشغّلات، فهذا الملخّص غير معروف وليس فارغًا.",
		"settings.runners_pick":            "اختيار العقد العاملة",
		"settings.runners_pick_tip":        "افتح صفحة المشغّلات: اختر العقد التي يجوز أن تشغّل مشغّلًا، وأوقف مشغّلًا أو أعد تشغيله.",

		// ---------- المتقدّم: التهيئات المُصاغة ----------
		"settings.adv_rendered_title": "تهيئات المنصّة المُصاغة",
		"settings.adv_rendered_sub":   "نص YAML نفسه الذي يُرسَل إلى السرب لتهيئات المنصّة، بعد استبدال القوالب. للقراءة فقط.",
		"settings.rendered_empty":     "لا توجد تهيئة مُصاغة مسجّلة بعد. أول «تطبيق على السرب» يكتبها.",
		"settings.rendered_unknown":   "لم يجب الخادم، فهذه القائمة غير معروفة وليست فارغة.",
		"settings.rendered_one":       "هناك تهيئة مُصاغة واحدة مسجّلة. اقرأها من Inventory → Platform، حيث يحمل كل صف تهيئة منصّة نسخته المُصاغة.",
		"settings.rendered_many":      "هناك عدة تهيئات مُصاغة مسجّلة. اقرأ واحدة من Inventory → Platform، حيث يحمل كل صف تهيئة منصّة نسخته المُصاغة.",

		"settings.cluster_unknown_body": "تعذّرت قراءة إعدادات العنقود، فلا تعرض هذه الصفحة حقولًا يمكن أن يمحوها حفظ لاحق. السبب مذكور أعلاه؛ أعد المحاولة عندما يستجيب الخادم.",
	})
}

// Arabic — عناوين أقسام العنقود ومتطلباتها.
func init() {
	register(AR, map[string]string{
		"clustersettings.sect_identity":       "الهوية والدخول",
		"clustersettings.sect_identity_sub":   "النطاق الذي يجيب عليه العنقود، ومن يُسمح له بالدخول.",
		"clustersettings.sect_identity_needs": "يتطلب مزوّدًا يعرفه الخادم مع معرّف عميل وسرّ منه؛ ومع إيقاف الدخول الموحّد تبقى كلمة المرور المدمجة هي الحاكم.",
		"clustersettings.sect_advanced":       "متقدّم",
		"clustersettings.sect_advanced_sub":   "مسارات ومستويات نادرًا ما تتغيّر، مع تهيئات المنصّة كما صيغت.",
		"clustersettings.sect_advanced_needs": "يتطلب دور المدير. هذه القيم تصل إلى ملفات الحزم على كل عقدة، لذا فهي مطويّة افتراضيًا.",

		"clustersettings.sect_runners_needs":     "يتطلب تطبيق GitHub مهيّأً، وgithub_runner_org، وعقدة عاملة واحدة على الأقل مختارة في صفحة المشغّلات.",
		"clustersettings.runners_perm_title":     "الصلاحية التي يحتاجها تسجيل المشغّلات",
		"clustersettings.runners_perm_body":      "Organization → Self-hosted runners: Read & write — وليست Administration التي تغطي نقاط نهاية على مستوى المستودع فقط، وليست صلاحية مستودع. يُصدَر رمز التركيب من صلاحيات التطبيق الحالية، لذا لا يفعل تغيير الصلاحية على GitHub شيئًا قبل إعادة إصدار الرمز: دوّر المفتاح في قسم تطبيق GitHub أو أعد تثبيت التطبيق، ثم شغّل «اختبار الاتصال».",
		"clustersettings.runners_perm_where":     "GitHub: Settings → Applications → هذا التطبيق → Permissions → Organization → Self-hosted runners.",
		"clustersettings.runner_org_required":    "مطلوب عند تشغيل المشغّلات.",
		"clustersettings.runners_security_title": "ملاحظات أمنية",

		"clustersettings.sect_backups_needs": "يتطلب مخزن نسخ احتياطي. الدلو ومنطقته واعتماده في بيئة الخادم؛ وهذه الصفحة تحدّد موعد التشغيل ومدة الاحتفاظ.",
		"clustersettings.backup_store_body":  "حجم محلي، ودلو خارج الموقع عند ضبطه. يُضبَطان في بيئة الخادم (PMCLUSTER_BACKUP_*) لا في هذه الصفحة: اعتماد يُكتب في نموذج يطبّقه العنقود نصف تطبيق أسوأ من اعتماد يُعلَن أنه خارجي.",

		"clustersettings.err_org_required":   "github_runner_org فارغ والمشغّلات مشغّلة — والتسجيل على مستوى المؤسسة، فلا يمكن أن يعمل.",
		"clustersettings.err_nodes_required": "لم تُختَر أي عقدة عاملة للمشغّلات؛ اختر واحدة في صفحة المشغّلات.",
	})
}

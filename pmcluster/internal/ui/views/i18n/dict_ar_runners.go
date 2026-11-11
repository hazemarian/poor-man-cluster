package i18n

// Arabic dictionary — لوحة المشغّلات: منتقي العقد التي يجوز أن تعمل عليها مشغّلات
// GitHub Actions المستضافة ذاتيًا، وجدول المشغّلات مع الإيقاف وإعادة التشغيل.
func init() {
	register(AR, map[string]string{
		"runners.title": "المشغّلات",
		"runners.sub":   "مشغّلات GitHub Actions مستضافة ذاتيًا: حاوية واحدة لكل عقدة عاملة مختارة، تُسجَّل على مستوى المؤسسة داخل مجموعة مشغّلات.",

		"runners.enabled":  "مفعّل",
		"runners.disabled": "متوقّف",

		"runners.picker_title":   "أي العقد العاملة يجوز أن تشغّل مشغّلًا",
		"runners.picker_sub":     "كل عقدة عاملة مختارة تشغّل حاوية مشغّل مؤقتة واحدة وتسجّلها في مجموعة مشغّلات المؤسسة. عقد الإدارة غير قابلة للاختيار: هي تشغّل مستوى التحكّم، والبناء عليها يجعل حالة العقدة Down ويُسقط واجهة العنقود.",
		"runners.col_select":     "اختيار",
		"runners.col_node":       "العقدة",
		"runners.col_role":       "الدور",
		"runners.col_reason":     "سبب المنع",
		"runners.col_node_state": "المشغّل",
		"runners.col_memory":     "الذاكرة",
		"runners.col_cap":        "الحد",
		"runners.effective_cap":  "الفعّال",
		"runners.cap_aria":       "حد الذاكرة للعقدة {0} (الفراغ يعني الافتراضي على مستوى العنقود)",
		"runners.capacity":       "السعة هي القائمة: مشغّل واحد لكل عقدة مختارة. كل عقدة محدودة بـ {0} ما لم يكن لصفّها حدّ خاص — الحد الواحد يناسب جهازًا واحدًا ويكون إذنًا بإسقاط التالي. عمود الذاكرة هو المتاح على تلك العقدة الآن.",
		"runners.mb":             "ميغابايت",
		"runners.state_none":     "لا يوجد",
		"runners.not_selected":   "غير مختارة",
		"runners.no_nodes":       "لم يُرجع الخادم أي عقدة.",
		"runners.save_nodes":     "حفظ الاختيار",
		"runners.save_hint":      "إضافة عقدة تبدأ مشغّلًا عليها في الدورة التالية؛ وإزالتها توقف مشغّلها وتلغي تسجيله. الحد الذي يتجاوز 70٪ من ذاكرة العقدة مرفوض — اتركه فارغًا لاستخدام الافتراضي على مستوى العنقود.",

		"runners.table_title":  "المشغّلات على العنقود",
		"runners.col_scope":    "النطاق",
		"runners.col_state":    "الحالة",
		"runners.col_mode":     "النمط",
		"runners.col_age":      "العمر",
		"runners.col_last_job": "آخر مهمة",

		"runners.state_starting":     "قيد البدء",
		"runners.state_idle":         "متاح",
		"runners.state_busy":         "مشغول",
		"runners.state_offline":      "غير متصل",
		"runners.state_between_jobs": "بين المهام",
		"runners.state_failed":       "فاشل",
		"runners.mode_ephemeral":     "مؤقت",
		"runners.mode_persistent":    "دائم",

		"runners.kill":              "إيقاف",
		"runners.kill_tip":          "إزالة حاوية هذا المشغّل وحذف تسجيله. يبقى متوقّفًا حتى تعيد تشغيله.",
		"runners.kill_busy_confirm": "هذا المشغّل ينفّذ مهمة الآن. إيقافه يزيل الحاوية في منتصف سير العمل ويُفشل تلك المهمة — يُلغى التشغيل ولا يمكن استئنافه. هل توقف المشغّل على {0}؟",
		"runners.restart":           "إعادة تشغيل",
		"runners.restart_tip":       "إعادة تسجيل هذا المشغّل وتشغيل حاويته من جديد.",
		"runners.restart_all":       "إعادة تشغيل المتوقّفة",
		"runners.restart_all_tip":   "إعادة تشغيل كل مشغّل يظهر في الجدول كمتوقّف.",

		"runners.disabled_title":      "المشغّلات المستضافة ذاتيًا متوقّفة",
		"runners.disabled_body":       "\u200fgithub_runners_enabled يساوي false، لذا لا توجد حاوية مشغّل ولا تسجيل على GitHub. لا شيء باقٍ يعمل.",
		"runners.disabled_action":     "فتح إعدادات العنقود",
		"runners.disabled_action_tip": "فعّل github_runners_enabled ثم اختر العقد العاملة من هنا.",
		"runners.disabled_foot":       "التفعيل يحتاج تطبيق GitHub مُهيّأً (صلاحية administration: write على مستوى المؤسسة)، ومؤسسة، وعقدة عاملة واحدة على الأقل.",

		"runners.empty_title": "لا يوجد مشغّل بعد",
		"runners.empty_body":  "الميزة مفعّلة لكن لم يُسجَّل أي مشغّل. العقدة المختارة تبدأ واحدًا خلال دقيقة — تحقّق من حالتها في الجدول أعلاه.",

		"runners.not_configured_body": "لوحة التحكّم غير موصولة بخادم، لذا لا تستطيع قراءة إعدادات المشغّلات ولا قائمتها.",
		"runners.unknown_title":       "تعذّر تحميل قائمة المشغّلات",
		"runners.unknown_body":        "لم تستطع اللوحة قراءة قائمة المشغّلات من الخادم. هذه ليست قائمة فارغة — ولم يتغيّر شيء.",

		"runners.foot": "المشغّل الموقوف يبقى موقوفًا: لا دورة مصالحة تعيد تسجيله حتى تضغط إعادة التشغيل. آخر مهمة هي أحدث تشغيل في المؤسسة، لأن GitHub لا ينسب المهمة إلى المشغّل الذي نفّذها.",

		"settings.runners_title": "المشغّلات المستضافة ذاتيًا",
		"settings.runners_body":  "حاوية مشغّل واحدة لكل عقدة عاملة مختارة، تُسجَّل على مستوى المؤسسة داخل مجموعة مشغّلات. اختر العقد، وتابع حالة كل مشغّل، وأوقفه أو أعد تشغيله من لوحة المشغّلات.",
		"settings.runners_open":  "فتح المشغّلات",

		"runners.err_list":               "تعذّرت قراءة قائمة المشغّلات",
		"runners.err_settings":           "تعذّرت قراءة إعدادات المشغّلات",
		"runners.err_nodes":              "تعذّرت قراءة قائمة العقد",
		"runners.err_bad_name":           "اسم المشغّل غير صالح.",
		"runners.err_manager":            "تلك العقدة لا يمكنها تشغيل مشغّل",
		"runners.err_save":               "تعذّر حفظ اختيار العقد",
		"runners.err_kill":               "تعذّر إيقاف المشغّل",
		"runners.err_kill_deregister":    "أُزيلت الحاوية، لكن GitHub لم يؤكّد إلغاء التسجيل",
		"runners.err_kill_register":      "لم يكتمل الإيقاف",
		"runners.err_restart":            "تعذّرت إعادة تشغيل المشغّل",
		"runners.err_restart_deregister": "أُزيلت الحاوية، لكن GitHub لم يؤكّد إلغاء التسجيل",
		"runners.err_restart_register":   "أُوقف المشغّل لكن لم يُسجَّل من جديد",
		"runners.err_restart_partial":    "أُعيد تشغيل بعض المشغّلات فقط",
		"runners.err_not_found":          "لا يوجد مشغّل بهذا الاسم (ربما أزالته دورة مصالحة)",
		"runners.err_busy":               "هذا المشغّل مشغول: إيقافه يُفشل المهمة التي ينفّذها",
		"runners.err_unknown_node":       "تلك العقدة ليست في العنقود، فلم يُحفظ لها حدّ",
		"runners.err_memory":             "حد الذاكرة هذا ليس كمية Docker صالحة (مثل 1200m أو 3g)",

		"runners.msg_killed":        "أُوقف المشغّل {0}.",
		"runners.msg_restarted":     "أُعيد تشغيل المشغّل {0}.",
		"runners.msg_restarted_all": "أُعيد تشغيل {0} من المشغّلات المتوقّفة.",
		"runners.msg_saved":         "حُفظ اختيار العقد: {0}.",
		"runners.msg_saved_none":    "أُلغي اختيار العقد: لن يعمل أي مشغّل حتى تُختار عقدة عاملة.",
	})

	registerPlural(AR, "plural.runner_nodes", PluralForms{
		Zero: "لا عقد مختارة", One: "عقدة واحدة مختارة",
		TwoNom: "عقدتان مختارتان", TwoObl: "عقدتين مختارتين",
		Few: "{n} عقد مختارة", Many: "{n} عقدة مختارة", Other: "{n} عقدة مختارة",
	})
	registerPlural(AR, "plural.runners", PluralForms{
		Zero: "لا مشغّلات", One: "مشغّل واحد",
		TwoNom: "مشغّلان", TwoObl: "مشغّلين",
		Few: "{n} مشغّلات", Many: "{n} مشغّلًا", Other: "{n} مشغّل",
	})
}

package i18n

// Arabic dictionary — the platform surfaces that used to be reachable only from
// a shell: Docker registry credentials, the bootstrap credentials, the daemon's
// own logs, and secret verification.
//
// Terminology follows the rest of the console: العنقود for the Swarm/cluster,
// أسرار العنقود for Swarm secrets, بيانات الاعتماد for credentials, مستودع
// الصور for an image registry, and السجلات for logs.
func init() {
	register(AR, map[string]string{
		"nav.logs": "سجلات لوحة التحكم",

		"settings.platform_card_title":       "بيانات اعتماد المنصة والمستودعات والسجلات",
		"settings.platform_card_registries":  "مستودعات الصور",
		"settings.platform_card_credentials": "بيانات اعتماد التهيئة",
		"settings.platform_card_logs":        "سجلات لوحة التحكم",
		"settings.platform_card_body":        "سجّل الدخول إلى مستودع صور خاص ليتمكن العمال من سحب صوره، أو دوّر كلمات المرور التي أنشأتها المنصة لمكوّناتها، أو اقرأ ما سجّله الخادم.",

		"registries.title":          "مستودعات الصور",
		"registries.sub":            "بيانات الاعتماد لمستودعات الصور الخاصة. تُرسل كلمة المرور مرة واحدة ولا تُقرأ بعدها.",
		"registries.back":           "الإعدادات",
		"registries.list_title":     "المستودعات المُهيّأة",
		"registries.col_host":       "المضيف",
		"registries.col_user":       "اسم المستخدم",
		"registries.col_configured": "تاريخ الإضافة",
		"registries.remove_confirm": "إزالة بيانات اعتماد {0} وتسجيل خروج هذا المضيف؟ لن يتمكن العمال بعدها من سحب صوره.",
		"registries.empty_title":    "لا توجد مستودعات مُهيّأة",
		"registries.empty_body":     "أضف مستودعًا لسحب الصور من مستودع خاص. حتى ذلك الحين، سيتوقف أي إطلاق يحتاجه بخطأ في سحب الصورة.",
		"registries.add_title":      "إضافة مستودع أو استبداله",
		"registries.add_sub":        "يسجّل الدخول على المدير ويخزّن بيانات الاعتماد مشفّرة. يتلقاها العمال عند الإطلاق التالي، لذا أعد إطلاق الحزمة بعد ذلك.",
		"registries.field_host":     "مضيف المستودع",
		"registries.field_user":     "اسم المستخدم",
		"registries.field_password": "كلمة المرور أو رمز الوصول",
		"registries.hint_host":      "مثل ghcr.io أو docker.io أو registry.example.com.",
		"registries.hint_password":  "تُستخدم مرة واحدة لأمر docker login وتُخزَّن مشفّرة، ولا تُعرض مرة أخرى.",
		"registries.action_add":     "حفظ وتسجيل الدخول",
		"registries.foot":           "كلمة المرور تُكتب ولا تُقرأ: تُستخدم لأمر docker login وتُخزَّن مشفّرة، ولا تستطيع أي صفحة قراءتها لاحقًا.",
		"registries.err_add":        "تعذّرت إضافة المستودع",
		"registries.err_remove":     "تعذّرت إزالة المستودع",
		"registries.err_list":       "تعذّرت قراءة قائمة المستودعات",
		"registries.msg_added":      "حُفظ المستودع {0} وسُجّل الدخول إليه.",
		"registries.msg_removed":    "أُزيل المستودع {0}.",

		"credentials.title":                 "بيانات اعتماد التهيئة",
		"credentials.sub":                   "كلمات مرور أنشأتها المنصة لمكوّناتها المضمّنة. يمكن تدويرها هنا، لكن لا يمكن قراءتها.",
		"credentials.back":                  "الإعدادات",
		"credentials.list_title":            "بيانات الاعتماد المُدارة",
		"credentials.col_name":              "الاسم",
		"credentials.col_kind":              "النوع",
		"credentials.col_user":              "اسم المستخدم",
		"credentials.col_secret":            "سر العنقود",
		"credentials.col_password":          "كلمة المرور الجديدة",
		"credentials.col_rotated":           "آخر تدوير",
		"credentials.never_rotated":         "لم يُدوَّر",
		"credentials.action_rotate":         "تدوير",
		"credentials.rotate_confirm":        "تدوير كلمة مرور {0}؟ ستُعاد تشغيل الخدمة التي تستخدمها.",
		"credentials.empty_title":           "لا توجد بيانات اعتماد مُدارة",
		"credentials.empty_body":            "تُنشأ عند أول تشغيل للعنقود.",
		"credentials.foot":                  "يُنشئ التدوير كلمة مرور جديدة، ويحدّث سر العنقود، ويعيد تشغيل الخدمة التي تستخدمه. تُعرض كلمة المرور الجديدة مرة واحدة ولا يمكن قراءتها بعدها.",
		"credentials.once_title":            "انسخ كلمة المرور الآن",
		"credentials.once_body":             "تُعرض مرة واحدة. لا تستطيع لوحة التحكم هذه ولا سطر الأوامر طباعتها مرة أخرى.",
		"credentials.once_username_changed": "تغيّر اسم المستخدم أيضًا.",
		"credentials.err_rotate":            "تعذّر تدوير بيانات الاعتماد",
		"credentials.err_list":              "تعذّرت قراءة بيانات الاعتماد",
		"credentials.msg_rotated":           "دُوّرت بيانات الاعتماد {0}.",

		"logs.title":         "سجلات لوحة التحكم",
		"logs.sub":           "ما سجّله الخادم نفسه. ناتج كل خدمة موجود في صفحتها.",
		"logs.panel_title":   "سجل الخادم",
		"logs.tail_label":    "الأسطر",
		"logs.since_label":   "المدة",
		"logs.since_any":     "الكل",
		"logs.since_week":    "آخر 7 أيام",
		"logs.all_files":     "تضمين الملفات اليومية الأقدم",
		"logs.foot":          "عدد الأسطر المعروضة: {0}",
		"logs.truncated":     "أُسقطت أسطر أقدم",
		"logs.empty_title":   "لا توجد مدخلات في السجل",
		"logs.empty_body":    "لم يكتب الخادم أي مدخلات في هذه المدة",
		"logs.level_unknown": "سجل",
		"logs.err_read":      "تعذّرت قراءة سجل لوحة التحكم",

		"inventory.verify_title":         "تحقّق من قيمة {0}",
		"inventory.verify_sub":           "لا يُخزَّن النصّ الصريح، لذا يمكن التحقق من القيمة دون عرضها.",
		"inventory.verify_field":         "القيمة المرشّحة",
		"inventory.verify_hint":          "لا يعود سوى نتيجة المطابقة.",
		"inventory.verify_action":        "تحقّق",
		"inventory.verify_match":         "مطابقة",
		"inventory.verify_match_body":    "هذه هي القيمة المخزّنة تحت ذلك الاسم.",
		"inventory.verify_no_match":      "غير مطابقة",
		"inventory.verify_no_match_body": "ليست هذه القيمة المخزّنة تحت ذلك الاسم.",
		"inventory.err_verify":           "تعذّر التحقق من السر",
	})

	// Counted nouns are registered here, never written as a bare "{0} <noun>": a count is
	// ungrammatical in Arabic outside the 11-99 range, and registerPlural carries all six CLDR
	// categories. See docs/console-i18n-contract.md.
	registerPlural(AR, "plural.registries", PluralForms{
		Zero: "لا توجد مستودعات مُهيّأة", One: "مستودع واحد مُهيّأ",
		TwoNom: "مستودعان مُهيّآن", TwoObl: "مستودعين مُهيّأين",
		Few: "{n} مستودعات مُهيّأة", Many: "{n} مستودعًا مُهيّأً", Other: "{n} مستودع مُهيّأ",
	})
	registerPlural(AR, "plural.credentials", PluralForms{
		Zero: "لا توجد اعتمادات مُدارة", One: "اعتماد واحد مُدار",
		TwoNom: "اعتمادان مُداران", TwoObl: "اعتمادين مُدارَين",
		Few: "{n} اعتمادات مُدارة", Many: "{n} اعتمادًا مُدارًا", Other: "{n} اعتماد مُدار",
	})
}

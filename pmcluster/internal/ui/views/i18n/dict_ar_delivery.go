package i18n

// Arabic dictionary — the delivery pages: Deploy (frag_deploy.html) and
// Webhooks (frag_webhooks.html). Both pages share one namespace because they
// are two halves of one flow: the manifest the console submits and the sources
// CI signs with.
//
// Voice rules, as in dict_ar_shell.go: buttons and links take المصدر ("إطلاق",
// "فتح الحزمة", "إلغاء"); hints and placeholders take فعل الأمر ("الصق
// ملف تعريف…", "خزّنه…"); completed system actions use المبني للمجهول ("أُطلقت {0}
// بالمراجعة {1}") and never "تم + مصدر"; failures use تعذّر ("تعذّرت قراءة
// قائمة الحزم"); possession uses كاف الخطاب ("سرّه"), never "الخاص بك";
// counts carry all six CLDR categories with a case-inflected dual, as in
// plural.sources below.
//
// Technical tokens stay Latin because the operator pastes them: DSL, YAML,
// JSON, HMAC-SHA256, sha256=, app, configs(), secrets(), CI, Swarm, Compose,
// API. "Manifest" has no glossary entry; it is rendered ملف التعريف throughout,
// matching dict_ar_workloads.go, rather than بيان (which reads as "statement").
func init() {
	register(AR, map[string]string{
		// --- Deploy: header ---
		"deploy.title":              "إطلاق حزمة",
		"deploy.sub":                "أرسل ملف تعريف إلى واجهة API للعنقود؛ يعيد الخادم كتابة الحزمة ويحفظ مراجعة جديدة.",
		"deploy.sub_not_configured": "واجهة API للعنقود غير مُهيّأة بعد، لذا لا يمكن إرسال أي شيء من هذه الصفحة.",
		"deploy.open_stacks":        "فتح الحزم",

		// --- Deploy: stat strip ---
		"deploy.st_target":            "الحزمة الهدف",
		"deploy.st_target_foot":       "اسم التطبيق في ملف التعريف",
		"deploy.no_target":            "بلا اسم",
		"deploy.st_named_foot":        "يقرأ الخادم الاسم من حقل app في ملف التعريف",
		"deploy.st_revision":          "المراجعة الحالية",
		"deploy.st_revision_foot":     "المراجعة العاملة للحزمة الهدف",
		"deploy.st_revision_new":      "حزمة جديدة",
		"deploy.st_revision_new_foot": "لا توجد حزمة بهذا الاسم بعد — ينشئها الخادم",
		"deploy.st_updated":           "آخر مراجعة محفوظة",
		"deploy.st_updated_foot":      "وقت حفظ الخادم لتلك المراجعة",
		"deploy.st_stacks":            "الحزم المعروفة",
		"deploy.st_stacks_foot":       "الحزم التي يعرضها الخادم",
		"deploy.st_unknown_foot":      "غير معروفة — تعذّرت قراءة قائمة الحزم",

		// --- Deploy: the manifest form ---
		"deploy.form_title":      "ملف التعريف",
		"deploy.f_app":           "اسم التطبيق",
		"deploy.ph_app":          "أدخل اسم التطبيق، مثل bookfair",
		"deploy.f_app_hint":      "أدخل اسم الحزمة المطلوب تحديثها. اتركه فارغًا ليقرأ الخادم الاسم من ملف التعريف.",
		"deploy.f_version":       "الإصدار",
		"deploy.ph_version":      "أدخل الإصدار، مثل v1.4.0",
		"deploy.f_version_hint":  "أدخل رقم إصدار اختياريًا ليحلّ محلّ الرقم المحدّد في ملف التعريف؛ وهو الرقم الذي يزيده CI مع كل إصدار.",
		"deploy.f_repo":          "المستودع",
		"deploy.ph_repo":         "https://github.com/acme/bookfair",
		"deploy.f_repo_hint":     "أدخل المستودع اختياريًا؛ يُسجَّل ضمن بيانات الحزمة ليعرف المشغّل التالي مصدرها.",
		"deploy.f_manifest":      "ملف التعريف (DSL)",
		"deploy.ph_manifest":     "الصق ملف التعريف، مثل app: bookfair",
		"deploy.f_manifest_hint": "الصق ملف تعريف بصيغة DSL؛ يحلّله الخادم ويتحقّق من صحته ويترجمه قبل إطلاق أي شيء.",
		"deploy.submit":          "إطلاق",
		"deploy.submit_named":    "الإطلاق إلى {0}",
		"deploy.confirm_new":     "إطلاق ملف التعريف هذا حزمةً جديدة؟ ينشئها الخادم ويحفظ المراجعة الأولى.",
		"deploy.confirm_named":   "إطلاق ملف التعريف هذا إلى {0}؟ ستُستبدل مراجعتها العاملة الحالية.",
		"deploy.submit_hint":     "يُرسَل بصيغة JSON إلى الخادم، ولا يُكتب شيء حتى يردّ الخادم.",

		// --- Deploy: result ---
		"deploy.result_title":   "قُبل الإطلاق",
		"deploy.result_body":    "أُطلقت {0} بالمراجعة {1}.",
		"deploy.result_changed": "أبلغ الخادم عن مراجعة جديدة لهذه الحزمة.",
		"deploy.result_hint":    "تابع وصول المهام إلى حالتها المطلوبة في صفحة الخدمات بعد أن ينتهي الخادم من إطلاق الحزمة.",
		"deploy.open_stack":     "فتح الحزمة",
		"deploy.view_revision":  "عرض المراجعة",

		// --- Deploy: request preview ---
		"deploy.preview_title":       "جسم الطلب",
		"deploy.preview_sub":         "ما ترسله هذه الصفحة إلى الخادم بالضبط، قبل أن ترسله.",
		"deploy.preview_endpoint":    "نقطة النهاية",
		"deploy.preview_method":      "الطريقة",
		"deploy.preview_size":        "حجم المتن",
		"deploy.preview_empty_title": "لا يوجد ما يُرسَل بعد",
		"deploy.preview_empty_body":  "الصق ملف تعريف في الأعلى ليظهر هنا جسم الطلب بالضبط، فتتحقّق منه قبل أن يراه الخادم.",
		"deploy.preview_goto":        "الانتقال إلى نموذج ملف التعريف",

		// --- Deploy: what the daemon does ---
		"deploy.pipeline_title":      "ما يفعله الخادم",
		"deploy.pipeline_sub":        "طلب واحد يمرّ بخمس مراحل. أي إخفاق يوقف التنفيذ ويترك المراجعة المحفوظة كما هي.",
		"deploy.step_parse":          "تحليل ملف التعريف",
		"deploy.step_parse_note":     "يُقرأ DSL بصيغة YAML وتُفحَص بنيته.",
		"deploy.step_interp":         "الاستبدال والتحقّق",
		"deploy.step_interp_note":    "تُستبدل المتغيّرات بقيمها ويُتحقّق من الحقول المطلوبة.",
		"deploy.step_translate":      "الترجمة إلى Compose",
		"deploy.step_translate_note": "تُحلّ الدالتان configs() وsecrets() بالرجوع إلى المخزن.",
		"deploy.step_record":         "حفظ المراجعة",
		"deploy.step_record_note":    "يُحفظ المصدر وYAML الناتج تحت رقم المراجعة الجديد.",
		"deploy.step_apply":          "إطلاق الحزمة",
		"deploy.step_apply_note":     "يعيد Swarm مواءمة الخدمات مع ملف Compose الجديد.",
		"deploy.pipeline_foot":       "المراحل هي الترتيب الثابت لدى الخادم؛ ولا تُبلَّغ لوحة التحكم بمدى تقدّم أي تنفيذ، فلا تُعلَّم أي مرحلة بأنها بدأت.",

		// --- Deploy: the stack list ---
		"deploy.stacks_title":         "الحزم",
		"deploy.stacks_empty_title":   "لا توجد حزم بعد",
		"deploy.stacks_empty_body":    "لم يُطلَق شيء بعد. الصق ملف تعريف في الأعلى وأرسله لإنشاء أول حزمة.",
		"deploy.stacks_unknown_title": "قائمة الحزم غير متاحة",
		"deploy.stacks_unknown_body":  "لم يستجب الخادم لهذا الطلب، لذا الحزم أدناه غير معروفة — وليست فارغة.",
		"deploy.col_stack":            "الحزمة",
		"deploy.row_deploy":           "الإطلاق إلى هذه الحزمة",
		"deploy.copy_name":            "نسخ اسم الحزمة",
		"deploy.stacks_foot":          "يمكن تنفيذ عمليتَي إطلاق في وقت واحد؛ يتابع الخادم المراجعات بالتسلسل داخل الحزمة، لا بين الحزم.",

		// --- Deploy: errors ---
		"deploy.err_manifest": "يلزم ملف تعريف قبل أن يستطيع الخادم إطلاق أي شيء",
		"deploy.err_deploy":   "رُفض طلب الإطلاق من الخادم.",
		"deploy.err_stacks":   "تعذّرت قراءة قائمة الحزم من الخادم",

		// --- Webhooks: header ---
		"webhooks.title": "خطافات الويب",
		"webhooks.sub":   "مصادر يمكنها الإطلاق بإرسال ملف تعريف موقَّع إلى المستقبِل، ولكل مصدر سرّه المشترك على حدة.",

		// --- Webhooks: stat strip ---
		"webhooks.st_sources":               "المصادر",
		"webhooks.st_sources_foot":          "مصادر خطافات الويب التي يعرضها الخادم",
		"webhooks.st_unknown_foot":          "غير معروفة — تعذّرت قراءة قائمة المصادر",
		"webhooks.st_endpoint":              "رابط المستقبِل",
		"webhooks.st_endpoint_foot":         "حيث يستقبل الخادم عمليات التسليم الموقّعة",
		"webhooks.st_endpoint_unknown_foot": "لا يوجد نطاق عام مُهيّأ للوحة التحكم",
		"webhooks.st_sig_foot":              "\u2068sha256=<hex>\u2069 على الطابع الزمني والمتن الخام",
		"webhooks.st_skew_foot":             "تُرفض عمليات التسليم الأقدم لأنها إعادة إرسال",

		// --- Webhooks: one-time secret ---
		"webhooks.secret_title":            "السر المشترك لـ {0}",
		"webhooks.secret_pill":             "يُعرض مرة واحدة",
		"webhooks.secret_body":             "هذه المرة الوحيدة التي يعيده فيها الخادم. خزّنه في مخزن أسرار CI قبل مغادرة هذه الصفحة.",
		"webhooks.secret_copy":             "نسخ السر",
		"webhooks.secret_endpoint":         "نقطة النهاية",
		"webhooks.secret_endpoint_unknown": "غير معروف — لا يوجد نطاق عام مُهيّأ للوحة التحكم",
		"webhooks.secret_usage":            "يوقّع CI كل عملية تسليم بهذا السر، ويرفض الخادم كل ما لا يطابقه.",

		// --- Webhooks: delivery contract ---
		"webhooks.delivery_title": "توقيع التسليم",
		"webhooks.delivery_sub":   "المستقبِل هو نقطة الدخول الوحيدة للطلبات الآلية: يتحقّق من التوقيع باستخدام سرّ المصدر قبل قراءة ملف التعريف.",
		"webhooks.d_method":       "الطريقة",
		"webhooks.d_path":         "المسار",
		"webhooks.d_sig":          "ترويسة التوقيع",
		"webhooks.d_ts":           "ترويسة الطابع الزمني",
		"webhooks.d_skew":         "نافذة الطابع الزمني",
		"webhooks.d_skew_v":       "5 دقائق",
		"webhooks.d_note":         "وقّع الطابع الزمني والمتن الخام معًا بصيغة \u2068HMAC-SHA256\u2069، ثم رمّز الناتج بصيغة hex وأضف قبله \u2068sha256=\u2069. والطابع الزمني بالثواني وفق نظام Unix.",
		"webhooks.d_body":         "المتن",
		"webhooks.d_body_v":       "حمولة الإطلاق بصيغة JSON — الحقول نفسها الموجودة في صفحة الإطلاق، والتحقّق نفسه.",
		"webhooks.d_snippet":      "توقيعه في CI",

		// --- Webhooks: add a source ---
		"webhooks.add_title":      "إضافة مصدر",
		"webhooks.add_sub":        "المصدر اسم يوقّع به CI. يولّد الخادم السر المشترك ويعرضه هنا مرة واحدة.",
		"webhooks.f_source":       "المصدر",
		"webhooks.ph_source":      "أدخل اسم المصدر، مثل github-actions",
		"webhooks.f_source_hint":  "اختر اسم المصدر بعناية؛ يُستخدم حرفيًا في مسار نقطة النهاية وفي التوقيع، ولا يمكن تغييره لاحقًا.",
		"webhooks.f_desc":         "الوصف",
		"webhooks.ph_desc":        "اكتب وصفًا مختصرًا، مثل bookfair ci",
		"webhooks.f_desc_hint":    "اكتب، اختياريًا، ما يُطلَق من هذا المصدر ليتعرّف عليه المشغّل التالي.",
		"webhooks.create_action":  "إنشاء مصدر",
		"webhooks.create_missing": "أدخل اسم مصدر للحصول على سرّ.",

		// --- Webhooks: the source list ---
		"webhooks.keys_title":     "مفاتيح خطافات الويب",
		"webhooks.col_source":     "المصدر",
		"webhooks.col_last":       "آخر تسليم",
		"webhooks.never_used":     "لم يُستخدم بعد",
		"webhooks.copy_source":    "نسخ اسم المصدر",
		"webhooks.copy_endpoint":  "نسخ نقطة النهاية",
		"webhooks.revoke":         "إلغاء",
		"webhooks.revoke_confirm": "إلغاء {0}؟ يتوقّف قبول سرّه المشترك فورًا ويُزال المصدر.",
		"webhooks.keys_foot":      "لا يمكن إعادة تسمية المصدر. ألغِه وأضف مصدرًا جديدًا — ويُلغى السر القديم معه.",
		"webhooks.empty_title":    "لا توجد مصادر خطافات الويب",
		"webhooks.empty_body":     "لا يمكن الإطلاق من خطافات الويب بعد. أضف مصدرًا ثم ضع سرّه المشترك في CI.",
		"webhooks.empty_action":   "إضافة مصدر",
		"webhooks.unknown_title":  "قائمة المصادر غير متاحة",
		"webhooks.unknown_body":   "لم يستجب الخادم لهذا الطلب، لذا المصادر أدناه غير معروفة — وليست قائمة فارغة.",

		// --- Webhooks: outcomes and errors ---
		"webhooks.msg_created":                  "أُنشئ مصدر خطاف الويب {0}. انسخ السر المشترك الآن — فهو يُعرض مرة واحدة.",
		"webhooks.msg_removed":                  "أُزيل مصدر خطاف الويب {0}. وتُرفض من الآن فصاعدًا عمليات التسليم الموقّعة بسرّه.",
		"webhooks.not_configured_body":          "مصادر خطافات الويب موجودة على الخادم، لذا تحتاج لوحة التحكم إلى واجهة API للعنقود قبل أن تسردها.",
		"webhooks.err_source":                   "اسم المصدر مطلوب قبل توليد السر",
		"webhooks.err_list":                     "تعذّرت قراءة قائمة مصادر خطافات الويب من الخادم",
		"webhooks.err_create":                   "تعذّر على الخادم إنشاء مصدر خطاف الويب هذا",
		"webhooks.err_remove":                   "تعذّر على الخادم إزالة مصدر خطاف الويب هذا",
		"deploy.f_file":                         "ملف البيان",
		"deploy.f_file_hint":                    "اختياري. يُسجَّل مع المراجعة للدلالة على مصدر هذا البيان.",
		"deploy.ph_file":                        "deploy/my-app.yaml",
		"webhooks.act_history":                  "سجل التسليم",
		"webhookdeliveries.title":               "التسليمات إلى {0}",
		"webhookdeliveries.sub":                 "المحاولات التي سجّلها المستلم، الأحدث أولًا.",
		"webhookdeliveries.sub_unknown":         "تعذّرت قراءة سجل التسليم، فهذا السجل مجهول لا فارغ.",
		"webhookdeliveries.hist_title":          "محاولات التسليم",
		"webhookdeliveries.foot":                "تُعرض أحدث {0} من المحاولات.",
		"webhookdeliveries.col_when":            "الوقت",
		"webhookdeliveries.col_status":          "النتيجة",
		"webhookdeliveries.col_stack":           "الحزمة",
		"webhookdeliveries.col_revision":        "المراجعة",
		"webhookdeliveries.col_file":            "الملف",
		"webhookdeliveries.col_error":           "الخطأ",
		"webhookdeliveries.status_accepted":     "مقبولة",
		"webhookdeliveries.status_unauthorized": "مرفوضة — التوقيع",
		"webhookdeliveries.status_error":        "خطأ من المستلم",
		"webhookdeliveries.empty_title":         "لم يُسلَّم شيء إلى هذا المصدر",
		"webhookdeliveries.empty_body":          "لم يسجّل المستلم أي محاولة لهذا المصدر — لم ينادِه شيء بعد.",
		"webhookdeliveries.unknown_title":       "سجل التسليم غير متاح",
		"webhookdeliveries.unknown_body":        "لم يُرجِع المستلم سجله، فلا يمكن عرض أي محاولة.",
		"err.webhook_deliveries":                "تعذّرت قراءة سجل التسليم",
	})

	registerPlural(AR, "plural.sources", PluralForms{
		Zero: "لا توجد مصادر", One: "مصدر واحد",
		TwoNom: "مصدران", TwoObl: "مصدرين",
		Few: "{n} مصادر", Many: "{n} مصدرًا", Other: "{n} مصدر",
	})
}

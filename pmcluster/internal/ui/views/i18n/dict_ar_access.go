package i18n

// القاموس العربي — واجهات الوصول: حسابات لوحة التحكم (المستخدمون)، ومفاتيح API،
// والإعدادات مع تهيئات العنقود وأسراره.
//
// مفاتيح هذا الملف مسبوقة بالصفحة التي تملكها (users.*، apikeys.*، settings.*)
// مع نطاقَي النوافذ المشتركة بين صفحات الحزم (configs.*، secrets.*).
// المفردات المسجَّلة في dict_ar_shell.go / dict_ar_cluster.go (common.*، nav.*،
// st.*، err.*) تُستعمل كما هي ولا تُكرَّر هنا.
//
// قواعد الصوت المطبَّقة (وفق دليل arabic-ui):
//   - الأزرار تأخذ المصدر: «حفظ التغييرات»، «إلغاء»، «إنشاء مفتاح»، «إظهار».
//   - التلميحات وصناديق الإدخال تأخذ فعل الأمر: «اترك كلمة المرور فارغة…»، «أدخل…».
//   - الحالات تأخذ اسم الفاعل/المفعول: «مضبوط»، «غير مضبوط»، «غير قابل للاسترجاع».
//   - الأفعال المكتملة تُصاغ بالمبني للمجهول: «أُنشئ المستخدم»، «حُفظت الإعدادات»،
//     «أُزيل مفتاح API»، «حُذفت التهيئة» — و«تعذّر» بدل «فشل في».
//   - الملكية بالكاف: «مفتاحك»… وحيث تعذّر ذلك يُضاف المالك باسمه لا بـ «الخاص بـ».
//   - «السرّ» للقيم المشفّرة، و«الرمز» لمفتاح API، و«المفتاح» لمفتاح API نفسه —
//     فإلغاء المفتاح ≠ حذفه، وتعطيل شيء ≠ إزالته.
//
// ملاحظة على جمع «مفتاح API»: المثنى في الإضافة «مفتاحا API» (رفع) و«مفتاحي API»
// (جر)، وكثرة 11–99 تحتاج صيغة معربة فلا تصلح الإضافة مع التنوين، لذا جاءت
// «{n} مفتاحًا لـ API».

func init() {
	register(AR, map[string]string{
		// ---------- المستخدمون: حسابات لوحة التحكم ----------
		"users.title":    "المستخدمون",
		"users.sub":      "حسابات لوحة التحكم والدور الذي يحمله كل حساب. لا يرى هذه القائمة أو يعدّلها إلا مديرو النظام.",
		"users.accounts": "الحسابات",
		"users.new":      "إضافة مستخدم",
		"users.you":      "أنت",
		"users.roles_help": "مدير النظام يدير الحسابات والتهيئات والأسرار — المشغّل يُطلق الحزم ويشغّلها — " +
			"المُشاهد يقرأ فقط.",
		"users.empty_title":    "لا توجد حسابات لعرضها",
		"users.empty_body":     "لم يُعِد مخزن الحسابات أي حساب. أنشئ أول حساب لمشاركة الوصول.",
		"users.unknown_title":  "تعذّر تحميل قائمة الحسابات",
		"users.unknown_body":   "تعذّرت قراءة مخزن المستخدمين في لوحة التحكم. لم يُغيَّر شيء — هذه ليست قائمة فارغة.",
		"users.login_disabled": "تسجيل الدخول مُعطّل في لوحة التحكم هذه",
		"users.login_disabled_body": "المتغيّر EDGE_LOGIN_DISABLED مُضبوط، لذا تُعامَل كل طلب كطلب مدير نظام. " +
			"وتُهمَل الحسابات ما دام مُفعّلًا.",

		// المستخدمون: نموذج الإنشاء والتعديل
		"users.form_new_title":  "إضافة مستخدم",
		"users.form_edit_title": "تعديل المستخدم",
		"users.form_new_sub":    "يسجّل الحساب الدخول إلى لوحة التحكم هذه بكلمة المرور التي تضبطها هنا.",
		"users.form_edit_sub":   "غيّر الدور، أو عيّن كلمة مرور جديدة. اترك كلمة المرور فارغة للإبقاء على الحالية.",
		"users.password_new":    "كلمة مرور جديدة (اختياري)",
		"users.role_locked": "يحتفظ حسابك بدور مدير النظام. امنح حسابًا آخر دور مدير النظام أولًا " +
			"إذا أردت التنازل عن دورك.",
		"users.form_create":        "إنشاء مستخدم",
		"users.form_save":          "حفظ التغييرات",
		"users.hint_password":      "أدخل كلمة مرور من 8 أحرف على الأقل.",
		"users.hint_password_keep": "اتركه فارغًا للإبقاء على كلمة المرور الحالية.",
		"users.delete_confirm":     "حذف المستخدم {0}؟ ينتهي وصوله فورًا وتتوقف أي جلسة مفتوحة عن العمل.",
		"users.err_admins_unknown": "تعذّر التحقق من عدد مديري النظام — يبقى تغيير الدور محظورًا حتى يستجيب مخزن الحسابات.",

		// المستخدمون: ضوابط السلامة — تعرضها الواجهة قبل الطلب، ويتحقق منها
		// المتحكّم مرة أخرى على الخادم.
		"users.self_delete_blocked": "لا يمكنك حذف حسابك",
		"users.last_admin_blocked":  "لا يمكن حذف آخر مدير نظام",
		"users.only_admin_hint":     "احتفظ بمدير نظام واحد على الأقل: لا تخفّض دور مدير النظام أو تحذف حسابه إلا إذا بقي مدير نظام آخر.",

		// المستخدمون: الرسائل والأخطاء
		"users.msg_created":        "أُنشئ المستخدم {0} ({1}).",
		"users.msg_updated":        "حُدِّث المستخدم {0}.",
		"users.msg_deleted":        "حُذف المستخدم {0}.",
		"users.err_store":          "تعذّرت قراءة مخزن الحسابات أو الكتابة فيه",
		"users.err_save":           "تعذّر حفظ الحساب",
		"users.err_delete":         "تعذّر حذف الحساب",
		"users.err_bad_id":         "معرّف المستخدم غير صالح.",
		"users.err_not_found":      "هذا الحساب غير موجود.",
		"users.err_username":       "اسم المستخدم مطلوب.",
		"users.err_password_short": "يجب أن تتكوّن كلمة المرور من 8 أحرف على الأقل.",
		"users.err_role":           "هذا الدور غير موجود.",
		"users.err_self_demote":    "لا يمكنك إزالة دور مدير النظام عن حسابك.",
		"users.err_self_delete":    "لا يمكنك حذف حسابك.",
		"users.err_last_admin":     "لا يمكن حذف آخر مدير نظام.",
		"users.err_admin_required": "لا تخفّض دور مدير النظام أو تحذف حسابه إلا إذا بقي مدير نظام آخر.",

		// ---------- مفاتيح API ----------
		"apikeys.title": "مفاتيح API",
		"apikeys.sub": "رموز Bearer للأتمتة التي تخاطب الخادم. لا يحفظ الخادم سوى تجزئة الرمز — " +
			"أما الرمز نفسه فيُعرض مرة واحدة عند إنشائه.",
		"apikeys.keys_title":      "المفاتيح الصادرة",
		"apikeys.new":             "إنشاء مفتاح",
		"apikeys.create_title":    "إنشاء مفتاح API",
		"apikeys.create_sub":      "سمّ العميل الذي سيستخدم الرمز — مثل ci أو proxmox أو مهمة نسخ احتياطي.",
		"apikeys.create_action":   "إنشاء مفتاح",
		"apikeys.col_token":       "الرمز",
		"apikeys.token_title":     "رمز {0}",
		"apikeys.token_body":      "انسخ هذا الرمز الآن — يُعرض مرة واحدة ولا يمكن استرجاعه لاحقًا.",
		"apikeys.token_prefix":    "البادئة",
		"apikeys.copy_token":      "نسخ الرمز",
		"apikeys.token_usage":     "أرسله في كل طلب بوصفه رمز Bearer:",
		"apikeys.token_for":       "هذا هو المفتاح الذي أنشأته للتوّ (المعرّف {0}).",
		"apikeys.token_gone":      "غير قابل للاسترجاع",
		"apikeys.token_gone_hint": "لا يحفظ الخادم لهذا الرمز سوى تجزئة، لذا لا يمكن عرض قيمته كاملة مرة أخرى.",
		"apikeys.revoke":          "إلغاء",
		"apikeys.revoke_confirm":  "إلغاء مفتاح \u2068API {0}\u2069؟ ستفشل الأتمتة التي تستخدمه فورًا.",
		"apikeys.empty_title":     "لا توجد مفاتيح API بعد",
		"apikeys.empty_body": "أنشئ مفتاحًا واحدًا لكل عميل يخاطب الخادم، حتى يمكن إلغاء أحدها " +
			"دون المساس بالبقية.",
		"apikeys.unknown_title":       "تعذّر تحميل قائمة المفاتيح",
		"apikeys.unknown_body":        "لم يستجب الخادم لطلب القائمة. لم يُغيَّر أي مفتاح.",
		"apikeys.not_configured_body": "وجّه لوحة التحكم إلى خادم pmcluster من الإعدادات، ثم عد لإصدار الرموز.",
		"apikeys.msg_created":         "أُنشئ مفتاح API للعميل {0}. يُعرض مرة واحدة — انسخه قبل مغادرة هذه الصفحة.",
		"apikeys.msg_removed":         "أُزيل مفتاح API {0}.",
		"apikeys.err_name":            "الاسم مطلوب.",
		"apikeys.err_bad_id":          "معرّف المفتاح غير صالح.",
		"apikeys.err_list":            "تعذّر عرض مفاتيح API",
		"apikeys.err_create":          "تعذّر إنشاء المفتاح",
		"apikeys.err_revoke":          "تعذّر إلغاء المفتاح",
		"apikeys.once_only":           "يُعرض مرة واحدة",
		"apikeys.just_created":        "أُنشئ للتوّ",
		"apikeys.name_placeholder":    "أدخل الاسم، مثل ci",

		// ---------- الإعدادات: الاتصال بالخادم ----------
		"settings.title": "الإعدادات",
		"settings.sub": "كيف تصل لوحة التحكم هذه إلى خادم pmcluster، مع التهيئات والأسرار " +
			"على مستوى العنقود التي تُطلق منها المنصة.",
		"settings.daemon_title":          "الاتصال بالخادم",
		"settings.api_url":               "عنوان API",
		"settings.api_url_hint":          "الوجهة التي ترسل إليها لوحة التحكم طلباتها. الافتراضي من البيئة: {0}",
		"settings.api_url_hint_none":     "لا يوجد افتراضي مضبوط في بيئة لوحة التحكم.",
		"settings.api_token":             "رمز API",
		"settings.api_token_hint":        "اتركه فارغًا للإبقاء على الرمز المحفوظ. ولا يُعرض مرة أخرى أبدًا.",
		"settings.api_token_set":         "يوجد رمز محفوظ في لوحة التحكم هذه",
		"settings.api_token_env":         "الرمز مصدره البيئة",
		"settings.api_token_none":        "لا يوجد رمز محفوظ",
		"settings.api_token_clear":       "إزالة الرمز المحفوظ عند الحفظ",
		"settings.conn_state":            "الاتصال",
		"settings.conn_ok":               "مُهيّأ",
		"settings.conn_missing":          "غير مُهيّأ",
		"settings.conn_missing_body":     "تبقى كل صفحة تقرأ من الخادم فارغة حتى يُضبط عنوان API هنا.",
		"settings.save":                  "حفظ الإعدادات",
		"settings.about_title":           "عن لوحة التحكم هذه",
		"settings.about_version":         "إصدار لوحة التحكم",
		"settings.about_daemon":          "الخادم",
		"settings.daemon_sub":            "الوجهة التي يُرسَل إليها كل طلب عنقود من لوحة التحكم هذه.",
		"settings.about_signed":          "مسجّل الدخول باسم {0}",
		"settings.secret_hidden":         "القيمة مخفية",
		"settings.delete_config_confirm": "حذف التهيئة {0}؟ ستفشل الحزم التي تشير إليها عند التطبيق التالي.",
		"settings.delete_secret_confirm": "حذف السرّ {0}؟ ستفشل الحزم التي تحتاجه عند التطبيق التالي.",

		// الإعدادات: التطبيق على العنقود
		"settings.apply_title":   "التطبيق على العنقود",
		"settings.apply_sub":     "إعادة توليد حزم المنصة من التهيئات والأسرار المحفوظة، ثم تحديث العنقود.",
		"settings.apply_hint":    "تسري تغييرات التهيئات والأسرار عند التطبيق التالي — لا يُدفع شيء أثناء الكتابة.",
		"settings.apply_action":  "التطبيق على العنقود",
		"settings.apply_summary": "طُبّق تحديث العنقود — \u2068otel: {0}\u2069، \u2068traefik: {1}\u2069، \u2068cert: {2}\u2069، \u2068edge: {3}\u2069، \u2068stacks: {4}\u2069",
		"settings.apply_last":    "آخر تطبيق",

		// الإعدادات: تهيئات العنقود وأسراره
		"settings.configs_title":       "تهيئات العنقود",
		"settings.configs_sub":         "القوالب والملفات التي تولّد منها حزم المنصة.",
		"settings.secrets_title":       "أسرار العنقود",
		"settings.secrets_sub":         "قيم مشفّرة تُسلَّم إلى حزم المنصة. لا يُعرض هنا سوى وجودها.",
		"settings.add_config":          "إضافة تهيئة",
		"settings.add_secret":          "إضافة سرّ",
		"settings.config_kind":         "النوع",
		"settings.col_version":         "الإصدار",
		"settings.col_updated":         "آخر تحديث",
		"settings.config_masked":       "••••••••",
		"settings.reveal":              "إظهار",
		"settings.reveal_confirm":      "إظهار السرّ {0}؟ ستُعرض قيمته على الشاشة.",
		"settings.rendered":            "المولَّدة",
		"settings.rendered_title":      "التهيئة المولَّدة",
		"settings.rendered_sub":        "ما سيسلّمه الخادم إلى العنقود بعد استبدال متغيّرات القالب — للقراءة فقط.",
		"settings.rendered_copy":       "نسخ YAML",
		"settings.configs_empty_title": "لا توجد تهيئات للعنقود بعد",
		"settings.configs_empty_body": "تأتي المنصة بقوالبها الخاصة. أضف تهيئة فقط لتجاوز أحدها، " +
			"أو للاحتفاظ بقيمة إلى جانب الأسرار.",
		"settings.configs_unknown_title": "تهيئات العنقود غير متاحة",
		"settings.configs_unknown_body":  "لم يستجب الخادم لطلب التهيئات. هذه ليست قائمة فارغة.",
		"settings.secrets_empty_title":   "لا توجد أسرار للعنقود بعد",
		"settings.secrets_empty_body": "تحتوي أسرار العنقود على بيانات اعتماد المنصة مثل مفتاح TLS " +
			"أو رمز API أو كلمة مرور سجل الصور.",
		"settings.secrets_unknown_title": "أسرار العنقود غير متاحة",
		"settings.secrets_unknown_body":  "لم يستجب الخادم لطلب الأسرار. هذه ليست قائمة فارغة.",
		"settings.secrets_hash_hint":     "لا تُسرد القيم أبدًا — فقط أسماؤها ووقت حفظها.",

		// الإعدادات: الرسائل والأخطاء
		"settings.msg_saved":              "حُفظت الإعدادات.",
		"settings.msg_config_created":     "أُنشئت التهيئة {0}. وتُطبَّق عند تحديث العنقود التالي.",
		"settings.msg_config_updated":     "حُدِّثت التهيئة {0}. استخدم «التطبيق على العنقود» لإعادة توليد المنصة.",
		"settings.msg_config_rolled_back": "أُعيدت التهيئة {0} إلى الإصدار {1}.",
		"settings.msg_config_removed":     "حُذفت التهيئة {0}.",
		"settings.msg_secret_created":     "أُنشئ السرّ {0}. تُحفظ القيمة مشفّرة؛ ولا تُعرض سوى تجزئتها.",
		"settings.msg_secret_updated":     "حُدِّث السرّ {0}.",
		"settings.msg_secret_removed":     "حُذف السرّ {0}.",
		"settings.err_save":               "تعذّر حفظ الإعدادات",
		"settings.err_config_list":        "تعذّر تحميل تهيئات العنقود",
		"settings.err_secret_list":        "تعذّر تحميل أسرار العنقود",
		"settings.err_config_action":      "تعذّر تغيير التهيئة",
		"settings.err_secret_action":      "تعذّر تغيير السرّ",
		"settings.err_bad_config_name":    "اسم التهيئة غير صالح.",
		"settings.err_bad_secret_name":    "اسم السرّ غير صالح.",
		"settings.err_bad_version":        "معرّف الإصدار غير صالح.",
		"settings.err_apply":              "تعذّر تفعيل تحديث العنقود",
		"settings.err_rendered":           "تعذّر تحميل التهيئة المولَّدة",
		"settings.err_rendered_missing":   "لا توجد لقطة مولَّدة بعد — شغّل «التطبيق على العنقود» أولًا.",
		"settings.err_rendered_not_found": "لا توجد لقطة مولَّدة لهذه التهيئة.",

		// ---------- التهيئات: نافذة الإنشاء والتعديل المشتركة ----------
		"configs.form_scope":        "النطاق",
		"configs.scope_cluster":     "على مستوى العنقود",
		"configs.form_new":          "إضافة تهيئة",
		"configs.form_edit":         "تعديل التهيئة",
		"configs.form_versions":     "سجل الإصدارات",
		"configs.form_sub_new":      "التهيئة متنٌ مُسمّى يُنشئه الإطلاق. سمّها بحسب الغرض منها.",
		"configs.form_sub_edit":     "يحتفظ الحفظ بالمتن القديم كإصدار، فيبقى التراجع ممكنًا دائمًا.",
		"configs.form_name":         "الاسم",
		"configs.form_name_hint":    "الاسم هو مفتاح البحث — لا تدعم هذه الصفحة إعادة التسمية.",
		"configs.form_kind":         "النوع",
		"configs.form_content":      "المحتوى",
		"configs.form_content_hint": "أدخل المحتوى كما هو أو اكتب قالبًا. تُستبدل المتغيّرات عند إطلاق الحزمة.",
		"configs.form_create":       "إنشاء تهيئة",
		"configs.form_save_version": "حفظ إصدار جديد",
		"configs.form_hash":         "التجزئة",
		"configs.form_restore":      "تراجع",
		"configs.restore_confirm":   "إرجاع {0} إلى الإصدار {1}؟ يُحفظ المتن الحالي كإصدار، فلا يُفقد شيء.",
		"configs.restore_hint":      "ينشئ التراجع إصدارًا جديدًا.",
		"configs.versions_empty":    "لا توجد إصدارات سابقة بعد. يسجّل أول حفظ الإصدار 1.",
		"configs.meta":              "النطاق {0}\u00a0· النوع {1}",
		"configs.meta_stack":        "الحزمة {0}",
		"configs.kind_file":         "file — متن حرفي",
		"configs.kind_template":     "template — قالب Go مع متغيّرات",
		"configs.kind_env":          "env — تعيينات متغيّرات البيئة",
		"configs.err_action":        "تعذّر حفظ التهيئة",
		"configs.err_bad_name":      "اسم التهيئة غير صالح.",
		"configs.err_name_required": "الاسم مطلوب.",

		// ---------- الأسرار: الإنشاء والتعديل والإظهار ----------
		"secrets.form_new":        "إضافة سرّ",
		"secrets.form_edit":       "تعديل السرّ",
		"secrets.editing":         "جارٍ التعديل",
		"secrets.form_sub_new":    "تُشفَّر القيمة على الخادم. ولا تستطيع لوحة التحكم قراءتها للتعديل.",
		"secrets.form_edit_help":  "القيمة الجديدة تحلّ محلّ المحفوظة. ولا تُعرض القيمة الحالية أبدًا.",
		"secrets.form_name":       "الاسم",
		"secrets.form_value":      "القيمة",
		"secrets.form_value_hint": "أدخل القيمة مرة واحدة؛ تُحفظ مشفّرة ولا تُعرض لاحقًا في القوائم.",
		"secrets.form_value_edit": "القيمة الجديدة تحلّ محلّ المحفوظة.",
		"secrets.form_create":     "حفظ السرّ",
		"secrets.copy_value":      "نسخ القيمة",
		"secrets.reveal_title":    "السرّ",
		"secrets.reveal_sub":      "تُعرض القيمة بعد فك التشفير أدناه، ولا تُعرض في أي مكان آخر في لوحة التحكم.",
		"secrets.reveal_warning": "تعامل معها ككلمة مرور: فكل من يرى هذه الشاشة يستطيع قراءتها. " +
			"أغلق النافذة عند الانتهاء.",
		"secrets.err_action":           "تعذّر حفظ السرّ",
		"secrets.err_reveal":           "تعذّر فك تشفير السرّ",
		"secrets.err_name_required":    "الاسم مطلوب.",
		"secrets.err_bad_name":         "اسم السرّ غير صالح.",
		"secrets.err_value_required":   "القيمة مطلوبة.",
		"apikeys.col_last_used":        "آخر استخدام",
		"apikeys.never_used":           "لم يُستخدم بعد",
		"settings.cluster_card_title":  "إعدادات العنقود",
		"settings.cluster_card_action": "فتح إعدادات العنقود",
		"settings.cluster_card_body":   "إعدادات يحفظها الخادم للعنقود بأكمله: جذر الأحجام، ونطاق النسخ الاحتياطي، ودخول الحافة، والدخول الموحّد. وهي محفوظة على العنقود لا في هذا المتصفح.",
		"settings.cluster_configs":     "تهيئات العنقود",
		"settings.cluster_secrets":     "أسرار العنقود",
		"settings.cluster_card_foot":   "تعديلها يحتاج دور المدير.",
		"settings.err_cluster_read":    "تعذّرت قراءة إعدادات العنقود",
		"settings.err_cluster_save":    "تعذّر حفظ إعدادات العنقود",
		"settings.msg_cluster_saved":   "حُفظت إعدادات العنقود.",
	})

	// العبارات المعدودة التي يملكها هذا النطاق. plural.users / plural.keys /
	// plural.configs / plural.secrets مسجَّلة أصلًا في قاموس الواجهة.
	registerPlural(AR, "plural.api_keys", PluralForms{
		Zero: "لا توجد مفاتيح API", One: "مفتاح API واحد",
		TwoNom: "مفتاحا API", TwoObl: "مفتاحي API",
		Few: "{n} مفاتيح API", Many: "{n} مفتاح API", Other: "{n} مفتاح API",
	})
	registerPlural(AR, "plural.versions", PluralForms{
		Zero: "لا توجد إصدارات", One: "إصدار واحد",
		TwoNom: "إصداران", TwoObl: "إصدارين",
		Few: "{n} إصدارات", Many: "{n} إصدارًا", Other: "{n} إصدار",
	})
	registerPlural(AR, "plural.accounts", PluralForms{
		Zero: "لا توجد حسابات", One: "حساب واحد",
		TwoNom: "حسابان", TwoObl: "حسابين",
		Few: "{n} حسابات", Many: "{n} حسابًا", Other: "{n} حساب",
	})
}

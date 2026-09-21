package i18n

// Arabic dictionary — application shell, navigation, shared vocabulary.
//
// Voice rules from the arabic-ui skill applied throughout this file:
//   · buttons/links take المصدر ("حفظ", "تسجيل الخروج"), not فعل مضارع
//   · placeholders take فعل الأمر ("أدخل…", "أعد المحاولة بعد قليل.")
//   · statuses take اسم الفاعل / اسم المفعول ("يعمل", "منتهية الصلاحية")
//   · completed actions use المبني للمجهول ("نُسخ إلى الحافظة"), never "تم + مصدر"
//   · errors use تعذّر / لم نتمكن من, never "فشل في + مصدر"
//   · possession uses كاف الخطاب ("مظهرك"), never "الخاص بك"
//   · counts carry all six categories with a case-inflected dual (see plural.go)

func init() {
	register(AR, map[string]string{
		// product
		"brand.sub":        "لوحة التحكم للمشغّل",
		"app.title":        "pmcluster",
		"nav.menu":         "القائمة",
		"nav.close":        "إغلاق القائمة",
		"nav.language":     "اللغة",
		"nav.theme":        "المظهر",
		"nav.logout":       "تسجيل الخروج",
		"nav.signed_in_as": "مسجّل الدخول باسم {0}",

		// navigation groups
		"nav.group.cluster":  "العنقود",
		"nav.group.delivery": "التسليم",
		"nav.group.trust":    "الموثوقية",
		"nav.group.access":   "الوصول",
		"nav.group.system":   "النظام",
		"nav.group.external": "خدمات خارجية",

		// navigation items
		"nav.overview": "نظرة عامة",
		"nav.stacks":   "الحزم",
		"nav.services": "الخدمات",
		"nav.deploy":   "الإطلاق",
		"nav.webhooks": "خطافات الويب",
		"nav.tls":      "الشهادات",
		"nav.backups":  "النسخ الاحتياطية",
		"nav.users":    "المستخدمون",
		"nav.apikeys":  "مفاتيح API",
		"nav.settings": "الإعدادات",
		"nav.observ":   "OpenObserve",
		"nav.traefik":  "Traefik",
		"nav.more":     "المزيد",

		// shared verbs — المصدر
		"common.save":      "حفظ",
		"common.cancel":    "إلغاء",
		"common.close":     "إغلاق",
		"common.add":       "إضافة",
		"common.edit":      "تعديل",
		"common.remove":    "إزالة",
		"common.delete":    "حذف",
		"common.confirm":   "تأكيد",
		"common.copy":      "نسخ",
		"common.copied":    "نُسخ إلى الحافظة",
		"common.refresh":   "تحديث",
		"common.retry":     "إعادة المحاولة",
		"common.search":    "بحث",
		"common.filter":    "تصفية",
		"common.apply":     "تطبيق",
		"common.restore":   "استعادة",
		"common.download":  "تنزيل",
		"common.upload":    "رفع",
		"common.back":      "رجوع",
		"common.open":      "فتح",
		"common.view_all":  "عرض الكل",
		"common.details":   "التفاصيل",
		"common.show_more": "عرض المزيد",
		"common.show_less": "عرض أقل",
		"common.dismiss":   "إخفاء",

		// shared nouns / labels
		"common.actions":     "إجراءات",
		"common.name":        "الاسم",
		"common.id":          "المعرّف",
		"common.type":        "النوع",
		"common.status":      "الحالة",
		"common.source":      "المصدر",
		"common.target":      "الهدف",
		"common.host":        "الخادم",
		"common.node":        "العقدة",
		"common.image":       "الصورة",
		"common.created":     "تاريخ الإنشاء",
		"common.updated":     "تاريخ التحديث",
		"common.never":       "لم يُنفَّذ بعد",
		"common.size":        "الحجم",
		"common.version":     "الإصدار",
		"common.revision":    "المراجعة",
		"common.role":        "الدور",
		"common.username":    "اسم المستخدم",
		"common.password":    "كلمة المرور",
		"common.email":       "البريد الإلكتروني",
		"common.optional":    "اختياري",
		"common.required":    "مطلوب",
		"common.none":        "لا شيء",
		"common.unknown":     "غير معروف",
		"common.unavailable": "غير متاح",
		"common.all":         "الكل",
		"common.yes":         "نعم",
		"common.no":          "لا",
		"common.loading":     "جارٍ التحميل…",
		"common.saving":      "جارٍ الحفظ…",
		"common.working":     "جارٍ التنفيذ…",
		"common.last_run":    "آخر تشغيل",
		"common.duration":    "المدة",
		"common.target_name": "الهدف",
		"common.warning":     "تنبيه",
		"common.error":       "خطأ",
		"common.reason":      "السبب",
		"common.raw":         "الاستجابة الخام",
		"common.expires":     "تاريخ الانتهاء",
		"common.issuer":      "الجهة المُصدِرة",
		"common.domains":     "النطاقات",
		"common.secret":      "السر",
		"common.value":       "القيمة",
		"common.description": "الوصف",
		"common.category":    "التصنيف",
		"common.summary":     "الملخّص",
		"common.automatic":   "تلقائي",
		"common.manual":      "يدوي",

		// statuses — اسم الفاعل / اسم المفعول
		"st.running":    "يعمل",
		"st.ready":      "جاهز",
		"st.active":     "نشط",
		"st.inactive":   "غير نشط",
		"st.pending":    "معلّق",
		"st.queued":     "في الانتظار",
		"st.failed":     "فاشل",
		"st.done":       "مكتمل",
		"st.degraded":   "أداء متردٍّ",
		"st.draining":   "جارٍ الإخلاء",
		"st.valid":      "صالحة",
		"st.expired":    "منتهية الصلاحية",
		"st.verified":   "موثّقة",
		"st.incomplete": "غير مكتملة",
		"st.missed":     "فائتة",
		"st.ok":         "سليم",
		"st.leader":     "القائد",
		"st.manager":    "مدير",
		"st.worker":     "عامل",
		"st.admin":      "مدير النظام",
		"st.operator":   "مشغّل",
		"st.viewer":     "مُشاهد",
		"st.sso_gated":  "محمي بالدخول الموحّد",
		"st.unknown":    "غير معروف",

		// errors — تعذّر
		"err.unreachable":      "تعذّر الوصول إلى واجهة API لـ pmcluster",
		"err.upstream":         "تعذّر تحميل هذه البيانات",
		"err.not_manager":      "هذا الخادم ليس مدير عنقود",
		"err.swarm_inactive":   "لا يوجد عنقود نشط على هذا الخادم",
		"err.backups_coverage": "تعذّرت قراءة تغطية النسخ الاحتياطية",
		"err.backup_create":    "تعذّر إنشاء نسخة احتياطية",
		"err.copy_failed":      "تعذّر نسخ القيمة",
		"err.logout_failed":    "تعذّر تسجيل الخروج",
		"err.show_details":     "عرض التفاصيل",
		"err.hide_details":     "إخفاء التفاصيل",
		"err.retry_hint":       "أعد المحاولة بعد قليل.",

		// topbar
		"topbar.refresh":      "تحديث هذه الصفحة",
		"topbar.theme_toggle": "تبديل المظهر",
		"topbar.lang_toggle":  "تبديل اللغة",

		// swarm / cluster vocabulary
		"cluster.state":            "حالة العنقود",
		"cluster.quorum":           "النصاب {0}/{1}",
		"cluster.nodes":            "العقد",
		"cluster.managers":         "المديرون",
		"cluster.services":         "الخدمات",
		"cluster.stacks":           "الحزم",
		"cluster.cpus":             "وحدات CPU",
		"cluster.memory":           "الذاكرة",
		"cluster.engine":           "المحرّك",
		"cluster.daemon":           "الخادم",
		"cluster.docker":           "محرّك الحاويات",
		"cluster.capacity":         "السعة",
		"cluster.unreachable_hint": "حالة العنقود غير متاحة لأن محرّك الحاويات لم يستجب.",
		"cluster.unknown_hint":     "هذه القيمة غير معروفة بعد، وليست صفرًا.",
	})

	// counted phrases — the six categories, with a case-inflected dual
	registerPlural(AR, "plural.stacks", PluralForms{
		Zero: "لا توجد حزم", One: "حزمة واحدة",
		TwoNom: "حزمتان", TwoObl: "حزمتين",
		Few: "{n} حزم", Many: "{n} حزمةً", Other: "{n} حزمة",
	})
	registerPlural(AR, "plural.services", PluralForms{
		Zero: "لا توجد خدمات", One: "خدمة واحدة",
		TwoNom: "خدمتان", TwoObl: "خدمتين",
		Few: "{n} خدمات", Many: "{n} خدمةً", Other: "{n} خدمة",
	})
	registerPlural(AR, "plural.nodes", PluralForms{
		Zero: "لا توجد عقد", One: "عقدة واحدة",
		TwoNom: "عقدتان", TwoObl: "عقدتين",
		Few: "{n} عقد", Many: "{n} عقدةً", Other: "{n} عقدة",
	})
	registerPlural(AR, "plural.revisions", PluralForms{
		Zero: "لا توجد مراجعات", One: "مراجعة واحدة",
		TwoNom: "مراجعتان", TwoObl: "مراجعتين",
		Few: "{n} مراجعات", Many: "{n} مراجعةً", Other: "{n} مراجعة",
	})
	registerPlural(AR, "plural.backups", PluralForms{
		Zero: "لا توجد نسخ احتياطية", One: "نسخة احتياطية واحدة",
		TwoNom: "نسختان احتياطيتان", TwoObl: "نسختين احتياطيتين",
		Few: "{n} نسخ احتياطية", Many: "{n} نسخةً احتياطية", Other: "{n} نسخة احتياطية",
	})
	registerPlural(AR, "plural.certificates", PluralForms{
		Zero: "لا توجد شهادات", One: "شهادة واحدة",
		TwoNom: "شهادتان", TwoObl: "شهادتين",
		Few: "{n} شهادات", Many: "{n} شهادةً", Other: "{n} شهادة",
	})
	registerPlural(AR, "plural.users", PluralForms{
		Zero: "لا يوجد مستخدمون", One: "مستخدم واحد",
		TwoNom: "مستخدمان", TwoObl: "مستخدمين",
		Few: "{n} مستخدمين", Many: "{n} مستخدمًا", Other: "{n} مستخدم",
	})
	registerPlural(AR, "plural.webhooks", PluralForms{
		Zero: "لا توجد خطافات", One: "خطاف واحد",
		TwoNom: "خطافان", TwoObl: "خطافين",
		Few: "{n} خطافات", Many: "{n} خطافًا", Other: "{n} خطاف",
	})
	registerPlural(AR, "plural.secrets", PluralForms{
		Zero: "لا توجد أسرار", One: "سر واحد",
		TwoNom: "سرّان", TwoObl: "سرّين",
		Few: "{n} أسرار", Many: "{n} سرًّا", Other: "{n} سر",
	})
	registerPlural(AR, "plural.configs", PluralForms{
		Zero: "لا توجد تهيئات", One: "تهيئة واحدة",
		TwoNom: "تهيئتان", TwoObl: "تهيئتين",
		Few: "{n} تهيئات", Many: "{n} تهيئةً", Other: "{n} تهيئة",
	})
	registerPlural(AR, "plural.keys", PluralForms{
		Zero: "لا توجد مفاتيح", One: "مفتاح واحد",
		TwoNom: "مفتاحان", TwoObl: "مفتاحين",
		Few: "{n} مفاتيح", Many: "{n} مفتاحًا", Other: "{n} مفتاح",
	})
	registerPlural(AR, "plural.tasks", PluralForms{
		Zero: "لا توجد مهام", One: "مهمة واحدة",
		TwoNom: "مهمتان", TwoObl: "مهمتين",
		Few: "{n} مهام", Many: "{n} مهمةً", Other: "{n} مهمة",
	})
	registerPlural(AR, "plural.lines", PluralForms{
		Zero: "لا توجد أسطر", One: "سطر واحد",
		TwoNom: "سطران", TwoObl: "سطرين",
		Few: "{n} أسطر", Many: "{n} سطرًا", Other: "{n} سطر",
	})
	registerPlural(AR, "plural.cores", PluralForms{
		Zero: "لا توجد أنوية", One: "نواة واحدة",
		TwoNom: "نواتان", TwoObl: "نواتين",
		Few: "{n} أنوية", Many: "{n} نواةً", Other: "{n} نواة",
	})
	registerPlural(AR, "plural.seconds", PluralForms{
		Zero: "أقل من ثانية", One: "ثانية واحدة",
		TwoNom: "ثانيتان", TwoObl: "ثانيتين",
		Few: "{n} ثوانٍ", Many: "{n} ثانيةً", Other: "{n} ثانية",
	})
	registerPlural(AR, "plural.minutes", PluralForms{
		Zero: "أقل من دقيقة", One: "دقيقة واحدة",
		TwoNom: "دقيقتان", TwoObl: "دقيقتين",
		Few: "{n} دقائق", Many: "{n} دقيقةً", Other: "{n} دقيقة",
	})
	registerPlural(AR, "plural.hours", PluralForms{
		Zero: "أقل من ساعة", One: "ساعة واحدة",
		TwoNom: "ساعتان", TwoObl: "ساعتين",
		Few: "{n} ساعات", Many: "{n} ساعةً", Other: "{n} ساعة",
	})
	registerPlural(AR, "plural.days", PluralForms{
		Zero: "اليوم", One: "يوم واحد",
		TwoNom: "يومان", TwoObl: "يومين",
		Few: "{n} أيام", Many: "{n} يومًا", Other: "{n} يوم",
	})
}

// الاستخدام صفحة للقراءة فقط ضمن مجموعة العنقود.
func init() {
	register(AR, map[string]string{"nav.usage": "الاستخدام"})
}

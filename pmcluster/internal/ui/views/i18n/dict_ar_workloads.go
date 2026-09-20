package i18n

// Arabic dictionary — the workloads screens: the stacks index, one stack's
// inspector, a single revision's manifests, and a stack's configs + secrets.
//
// Voice rules from the arabic-ui skill applied throughout this file:
//   · buttons/links take المصدر ("إعادة الإطلاق", "التراجع", "مسح التصفية")
//   · placeholders take فعل الأمر ("اكتب اسمًا أو مراجعة أو مستودعًا للتصفية")
//   · statuses take اسم الفاعل / اسم المفعول ("منشورة", "الحالية")
//   · completed actions use المبني للمجهول ("حُذفت الحزمة", "أُنشئت التهيئة"),
//     never "تم + مصدر"
//   · failures use تعذّر / لم نتمكن من ("تعذّرت قراءة هذه الحزمة")
//   · possession uses كاف الخطاب ("مستودعه")
//   · counts are registered as plurals elsewhere; this page set adds no
//     plural.* key, because the English workloads part defines none
//
// Vocabulary notes for this domain:
//   · manifest is ملف التعريف: the stack YAML as stored, and its rendering
//     after the daemon interpolates variables ("بعد التصيير").
//   · daemon is الخادم (cluster.* ships the same gloss), never الخدمة.
//   · unknown and empty stay distinct in every state: "غير معروفة وليست
//     فارغة" for an unanswered API, "لا توجد…" for an empty answer.
//   · config(name) / secrets(name) stay in Latin script: the operator copies
//     them into the manifest verbatim.

func init() {
	register(AR, map[string]string{
		// ---- stacks list -----------------------------------------------------
		"stacks.title":           "الحزم",
		"stacks.sub":             "{0}\u00a0· أحدث تغيير {1}",
		"stacks.sub_unknown":     "قائمة الحزم غير متاحة — لم تستجب واجهة API للعنقود",
		"stacks.list_title":      "الحزم المُطلقة",
		"stacks.search_ph":       "اكتب اسمًا أو مراجعة أو مستودعًا للتصفية",
		"stacks.clear_filter":    "مسح التصفية",
		"stacks.showing":         "عرض {0} من {1}",
		"stacks.col_repo":        "المستودع",
		"stacks.repo_unset":      "لا يوجد مستودع",
		"stacks.state_none":      "لا توجد خدمات",
		"stacks.stat_stacks":     "الحزم",
		"stacks.stat_stacks_f":   "تتتبّعها لوحة التحكم هذه",
		"stacks.stat_replicas":   "النسخ",
		"stacks.stat_replicas_f": "يعمل / مطلوب في كل الحزم",
		"stacks.stat_git":        "مدعومة بـ Git",
		"stacks.stat_git_f":      "مُطلقة من مستودع",
		"stacks.stat_changed":    "آخر تغيير",
		"stacks.stat_changed_f":  "أحدث تحديث لحزمة",
		"stacks.act_redeploy":    "إعادة الإطلاق",
		"stacks.act_config":      "التهيئة والأسرار",
		"stacks.act_backups":     "النسخ الاحتياطية",
		"stacks.confirm_sync":    "إعادة إطلاق {0} من مستودعه؟ يطبّق الخادم أحدث ملف تعريف، وتظل الحزمة تعمل أثناء ذلك.",
		"stacks.confirm_delete":  "حذف الحزمة {0}؟ ستُزال خدماتها ومراجعاتها من الخادم، ولا يمكن التراجع عن ذلك.",
		"stacks.msg_removed":     "حُذفت الحزمة {0}.",
		"stacks.unknown_title":   "قائمة الحزم غير متاحة",
		"stacks.unknown_body":    "لم تستجب واجهة API للعنقود لهذا الطلب، لذا الحزم أدناه غير معروفة وليست فارغة. تحقّق من عنوان API والرمز في الإعدادات.",
		"stacks.empty_title":     "لا توجد حزم بعد",
		"stacks.empty_body":      "الحزمة ملف تعريف يُطلقه الخادم ويتتبّعه بالمراجعة. أطلق واحدة، أو اربط خطاف ويب بمستودع ليُطلَق تلقائيًا عند كل دفع.",
		"stacks.no_match_title":  "لا توجد حزمة تطابق «{0}»",
		"stacks.no_match_body":   "لم تطابق التصفية أي اسم حزمة أو مراجعة أو مستودع. مسحها يعرض كل حزم العنقود.",

		// ---- one stack's inspector ------------------------------------------
		"stack.sync_hint":        "إعادة الإطلاق تعيد تطبيق أحدث ملف تعريف من الخادم؛ ولا تظهر مراجعة جديدة إلا إذا تغيّر ذلك الملف.",
		"stack.backup_title":     "آخر نسخة احتياطية",
		"stack.backup_started":   "بدأت",
		"stack.backup_finished":  "انتهت",
		"stack.backup_none":      "لا توجد نسخة احتياطية مسجّلة لهذه الحزمة بعد.",
		"stack.backup_hint":      "تجد سجل النسخ الاحتياطية في صفحة",
		"stack.revisions_title":  "سجل المراجعات",
		"stack.rev_current":      "الحالية",
		"stack.rev_when":         "أُنشئت {0}",
		"stack.view_manifest":    "ملفات التعريف",
		"stack.rollback":         "التراجع",
		"stack.confirm_rollback": "إرجاع {0} إلى المراجعة {1}؟ يعيد الخادم إطلاق ملف التعريف الخاص بتلك المراجعة ويسجّل لها مراجعة جديدة.",
		"stack.revs_empty_title": "لا توجد مراجعات بعد",
		"stack.revs_empty_body":  "يسجّل الخادم مراجعة عند أول إطلاق للحزمة. أطلق هذه الحزمة لبدء سجلها.",
		"stack.unknown_title":    "تعذّرت قراءة هذه الحزمة",
		"stack.unknown_body":     "الحزمة غائبة عن استجابة الخادم: ربما حُذفت، أو أن الاسم في العنوان خطأ.",
		"stack.err_bad_revision": "رقم المراجعة هذا غير صالح.",
		"revision.err_number":    "رقم المراجعة هذا غير صالح.",
		"stack.msg_synced":       "أُعيد إطلاق {0} — المراجعة {1}.",
		"stack.msg_synced_same":  "أُعيد إطلاق {0} — لم يتغيّر ملف التعريف، فلم تُسجَّل مراجعة جديدة.",
		"stack.msg_rolled_back":  "أُعيدت {0} إلى المراجعة {1}، وسُجّلت كمراجعة {2}.",

		// ---- one revision's manifests ---------------------------------------
		"revision.title":    "المراجعة {0} · {1}",
		"revision.sub":      "سُجّلت {0}",
		"revision.source":   "ملف التعريف المصدر",
		"revision.rendered": "ملف التعريف بعد التصيير",

		// ---- a stack's configs + secrets ------------------------------------
		"stackconfigs.title":                 "التهيئة والأسرار · {0}",
		"stackconfigs.sub":                   "تهيئات وأسرار على نطاق الخدمة لهذه الحزمة، تُشار إليها من ملف تعريفها بصيغة config(name) وsecrets(name). تُخزَّن القيم مشفّرة، ولا تُعرض الأسرار مرة أخرى في هذه الصفحة.",
		"stackconfigs.configs_title":         "التهيئات",
		"stackconfigs.secrets_title":         "الأسرار",
		"stackconfigs.form_new_secret":       "سر جديد",
		"stackconfigs.reveal":                "إظهار",
		"stackconfigs.secret_hidden":         "القيمة مخفية",
		"stackconfigs.confirm_reveal":        "إظهار قيمة {0}؟ سيظهر نصّها الصريح على الشاشة، ويمكن نسخه أو تصويره أو تسجيله.",
		"stackconfigs.confirm_config_remove": "حذف التهيئة {0} وسجل إصداراتها؟ لا تُطلَق الحزم التي تشير إليها حتى توجد تهيئة بهذا الاسم مرة أخرى.",
		"stackconfigs.confirm_secret_remove": "حذف السر {0}؟ لا تُطلَق الحزم التي تشير إليه حتى يوجد سر بهذا الاسم مرة أخرى.",
		"stackconfigs.configs_unknown_title": "التهيئات غير متاحة",
		"stackconfigs.configs_unknown_body":  "لم تُحمَّل قائمة التهيئات، لذا تهيئات هذه الحزمة غير معروفة وليست فارغة. الأسرار أدناه قُرئت على حدة، وهي قائمة بذاتها.",
		"stackconfigs.configs_empty_title":   "لا توجد تهيئات لهذه الحزمة",
		"stackconfigs.configs_empty_body":    "أضف واحدة لحفظ ملف أو قالب أو كتلة متغيّرات بيئة مع الحزمة، ثم أشِر إليها من ملف التعريف.",
		"stackconfigs.secrets_unknown_title": "الأسرار غير متاحة",
		"stackconfigs.secrets_unknown_body":  "لم تُحمَّل قائمة الأسرار، لذا أسرار هذه الحزمة غير معروفة وليست فارغة. لا شيء مُدرج هنا لأن القراءة لم تحدث، لا لأنها غير موجودة.",
		"stackconfigs.secrets_empty_title":   "لا توجد أسرار لهذه الحزمة",
		"stackconfigs.secrets_empty_body":    "أضف سرًّا لتخزين قيمة تقرأها خدمات الحزمة عند الإطلاق، دون أن تكون مكتوبة في ملف التعريف.",
		"stackconfigs.err_bad_name":          "هذا الاسم غير صالح.",
		"stackconfigs.err_bad_version":       "معرّف الإصدار هذا غير صالح.",
		"stackconfigs.msg_config_added":      "أُنشئت التهيئة {0} للحزمة {1}.",
		"stackconfigs.msg_config_edited":     "حُدّثت التهيئة {0} — وسُجّل إصدار جديد.",
		"stackconfigs.msg_config_rolled":     "أُعيدت التهيئة {0} إلى الإصدار {1}.",
		"stackconfigs.msg_config_removed":    "حُذفت التهيئة {0}.",
		"stackconfigs.msg_secret_added":      "أُنشئ السر {0} للحزمة {1}.",
		"stackconfigs.msg_secret_edited":     "حُدّث السر {0}.",
		"stackconfigs.msg_secret_removed":    "حُذف السر {0}.",

		// ---- errors the stacks controllers raise -----------------------------
		"err.stacks":               "الحزم غير متاحة.",
		"err.stack":                "تعذّرت قراءة هذه الحزمة.",
		"err.stack_sync":           "تعذّرت إعادة إطلاق الحزمة.",
		"err.stack_rollback":       "تعذّر التراجع عن الحزمة.",
		"err.stack_remove":         "تعذّر حذف الحزمة.",
		"err.revision":             "تعذّرت قراءة هذه المراجعة.",
		"err.stackconfigs_configs": "تعذّرت قراءة تهيئات الحزمة.",
		"err.stackconfigs_secrets": "تعذّرت قراءة أسرار الحزمة.",
		"err.config_remove":        "تعذّر حذف التهيئة.",
		"err.config_rollback":      "تعذّر التراجع عن التهيئة.",
		"err.secret_remove":        "تعذّر حذف السر.",
		"stacks.col_file":          "الملف",
		"stacks.file_unset":        "لم يُسجَّل ملف",
		"revision.pipeline":        "مراحل التنفيذ",
		"revision.pipeline_empty":  "لم تُسجّل هذه المراجعة أي مرحلة.",
		"revision.pipeline_sub":    "المراحل التي نفّذها الخادم بهذا الترتيب.",
	})
}

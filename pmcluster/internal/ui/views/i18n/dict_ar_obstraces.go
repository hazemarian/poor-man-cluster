package i18n

func init() {
	register(AR, map[string]string{
		"obs.tab_traces":           "التتبعات",
		"obs.traces_title":         "التتبعات",
		"obs.traces_sub":           "الطلبات المرسلة من تطبيقاتك عبر OpenTelemetry، الأحدث أولاً.",
		"obs.traces_none_title":    "لا توجد تتبعات في هذه الفترة",
		"obs.traces_none_body":     "أرسل تتبعات OTLP إلى المنفذ 4318 للمجمّع (http://<host>:4318/v1/traces) وحدّد service.name في تطبيقك.",
		"obs.col_trace":            "التتبع",
		"obs.col_root":             "النطاق الجذري",
		"obs.col_spans":            "النطاقات",
		"obs.col_duration":         "المدة",
		"obs.col_started":          "البدء",
		"obs.filter_trace_service": "اسم الخدمة",
		"obs.filter_errors":        "الأخطاء فقط",
		"obs.trace_back":           "كل التتبعات",
		"obs.trace_notfound":       "لم يُعثر على التتبع في هذه الفترة. جرّب فترة أطول.",
		"obs.trace_total":          "الإجمالي",
		"obs.trace_spans_label":    "نطاقات",
		"obs.trace_errors_label":   "أخطاء",
		"obs.trace_truncated":      "عرض أول 500 نطاق.",
		"obs.trace_orphan":         "(الجذر خارج الفترة)",
		"obs.live":                 "بث مباشر",
		"obs.live_on":              "مباشر · تحديث كل 5 ثوانٍ",
		"obs.live_note":            "أحدث السجلات فقط. أوقفه للتصفح للخلف.",
	})
}

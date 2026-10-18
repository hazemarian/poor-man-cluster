package i18n

func init() {
	register(EN, map[string]string{
		"obs.tab_traces":           "Traces",
		"obs.traces_title":         "Traces",
		"obs.traces_sub":           "Requests sent by your apps over OpenTelemetry, newest first.",
		"obs.traces_none_title":    "No traces in this window",
		"obs.traces_none_body":     "Send OTLP traces to port 4318 of the collector (http://<host>:4318/v1/traces) and set a service.name on your app.",
		"obs.col_trace":            "Trace",
		"obs.col_root":             "Root span",
		"obs.col_spans":            "Spans",
		"obs.col_duration":         "Duration",
		"obs.col_started":          "Started",
		"obs.filter_trace_service": "Service name",
		"obs.filter_errors":        "Errors only",
		"obs.trace_back":           "All traces",
		"obs.trace_notfound":       "Trace not found in this window. Try a longer range.",
		"obs.trace_total":          "Total",
		"obs.trace_spans_label":    "spans",
		"obs.trace_errors_label":   "errors",
		"obs.trace_truncated":      "Showing the first 500 spans.",
		"obs.trace_orphan":         "(root outside window)",
		"obs.live":                 "Live tail",
		"obs.live_on":              "Live · refreshing every 5 seconds",
		"obs.live_note":            "Newest records only. Turn off to page back.",
	})
}

package logger

import (
	"context"
	"encoding/json"
	"time"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// otelWriter is an io.Writer fed by zerolog. Each Write receives one
// complete JSON log event; we decode it and forward to the global OTel
// LoggerProvider so audit lines land in OpenObserve alongside metrics
// and spans.
//
// When telemetry.Init hasn't run (or the endpoint is empty), the global
// provider is a noop and emits become zero-cost — so it's always safe
// to wire this writer into logger.New.
type otelWriter struct {
	logger log.Logger
}

func newOTelWriter() *otelWriter {
	return &otelWriter{
		logger: global.GetLoggerProvider().Logger(
			"github.com/hazemarian/poor-man-stack/pmcluster/internal/logger",
		),
	}
}

// Write parses the zerolog JSON line and forwards a structured Record
// to OTel. Malformed JSON is dropped silently — the file + console
// writers in the multi-writer chain already have the raw bytes for
// post-mortem investigation.
func (w *otelWriter) Write(p []byte) (int, error) {
	n := len(p)
	var entry map[string]any
	if err := json.Unmarshal(p, &entry); err != nil {
		return n, nil
	}

	var rec log.Record
	rec.SetTimestamp(parseTime(entry))
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(severityFromLevel(entry["level"]))
	rec.SetSeverityText(stringField(entry["level"]))
	if msg, ok := entry["message"].(string); ok {
		rec.SetBody(log.StringValue(msg))
	}

	for k, v := range entry {
		switch k {
		case "level", "time", "message":
			continue
		}
		rec.AddAttributes(log.KeyValue{Key: k, Value: toLogValue(v)})
	}

	w.logger.Emit(context.Background(), rec)
	return n, nil
}

func parseTime(entry map[string]any) time.Time {
	s, ok := entry["time"].(string)
	if !ok {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

func stringField(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// severityFromLevel maps zerolog levels to OTel severity numbers. Mirrors
// the levels accepted by parseLevel — keep in sync.
func severityFromLevel(v any) log.Severity {
	switch stringField(v) {
	case "debug":
		return log.SeverityDebug
	case "info":
		return log.SeverityInfo
	case "warn", "warning":
		return log.SeverityWarn
	case "error":
		return log.SeverityError
	case "fatal":
		return log.SeverityFatal
	case "panic":
		return log.SeverityFatal4
	default:
		return log.SeverityUndefined
	}
}

// toLogValue converts JSON-decoded values into OTel log.Value variants.
// Falls back to String(fmt.Sprint(v)) for shapes the typed API doesn't
// cover — keeps the writer total.
func toLogValue(v any) log.Value {
	switch x := v.(type) {
	case string:
		return log.StringValue(x)
	case bool:
		return log.BoolValue(x)
	case float64:

		if x == float64(int64(x)) {
			return log.Int64Value(int64(x))
		}
		return log.Float64Value(x)
	case nil:
		return log.StringValue("")
	default:

		b, err := json.Marshal(x)
		if err != nil {
			return log.StringValue("")
		}
		return log.StringValue(string(b))
	}
}

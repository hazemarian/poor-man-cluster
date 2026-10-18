package observability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Bounds for the interactive log and metric views. They sit below the shared
// evidence limits: a page of rows is for reading, not for bulk export.
const (
	MaxLogPageSize = 200
	MaxLogOffset   = 2000
	MaxSearchRunes = 100
	MaxMetricRows  = 20_000
)

// Metric names the console understands. They are shared so the adapter that
// selects them and the view that projects them cannot drift apart.
const (
	MetricNodeCPUUtilization     = "system.cpu.utilization"
	MetricNodeMemUtilization     = "system.memory.utilization"
	MetricNodeLoad1m             = "system.cpu.load_average.1m"
	MetricContainerCPU           = "container.cpu.utilization"
	MetricContainerMemoryPercent = "container.memory.percent"
)

// Severities is the allowlist behind the log severity filter. The value is
// never interpolated into SQL; each maps to a fixed set of level spellings.
var Severities = []string{"error", "warn", "info", "debug"}

// ValidSeverity reports whether value is an allowlisted severity filter. The
// empty string means no filter.
func ValidSeverity(value string) bool {
	if value == "" {
		return true
	}
	for _, s := range Severities {
		if s == value {
			return true
		}
	}
	return false
}

// CleanSearch normalises free text typed into the log search box: control
// characters are dropped and the length is capped. It performs no SQL
// escaping; adapters escape for their own dialect.
func CleanSearch(value string) string {
	var b strings.Builder
	runes := 0
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsControl(r) {
			continue
		}
		if runes >= MaxSearchRunes {
			break
		}
		b.WriteRune(r)
		runes++
	}
	return strings.TrimSpace(b.String())
}

// LogQuery is a newest-first, paged read of log records. Search matches the
// stored (unredacted) body; only the rows returned are redacted, so whether a
// search term matches can reveal that a redacted value exists. Operators with
// read access to the page already hold read access to the telemetry backend.
type LogQuery struct {
	Service  string
	Window   Window
	Severity string
	Search   string
	Limit    int
	Offset   int
}

// Validate checks every bound that does not depend on the daemon inventory.
func (q LogQuery) Validate() error {
	base := Query{Service: q.Service, Window: q.Window, Limit: q.Limit}
	if err := base.Validate(); err != nil {
		return err
	}
	if q.Limit > MaxLogPageSize {
		return fmt.Errorf("log page size exceeds %d", MaxLogPageSize)
	}
	if q.Offset < 0 || q.Offset > MaxLogOffset {
		return fmt.Errorf("log offset must be between 0 and %d", MaxLogOffset)
	}
	if !ValidSeverity(q.Severity) {
		return errors.New("unsupported severity filter")
	}
	if len([]rune(q.Search)) > MaxSearchRunes {
		return errors.New("search text is too long")
	}
	return nil
}

// LogSource reads newest-first log pages. Result.Truncated means another page
// exists beyond the returned rows.
type LogSource interface {
	Logs(ctx context.Context, q LogQuery) (Result, error)
}

// MetricQuery bounds a metric read to a window and a bucket width.
type MetricQuery struct {
	Window Window
	Bucket time.Duration
}

// Validate keeps the number of buckets per series small and predictable.
func (q MetricQuery) Validate() error {
	base := Query{Window: q.Window, Limit: 1}
	if err := base.Validate(); err != nil {
		return err
	}
	if q.Bucket < 10*time.Second {
		return errors.New("metric bucket is too small")
	}
	if q.Window.End.Sub(q.Window.Start)/q.Bucket > 400 {
		return errors.New("metric window has too many buckets")
	}
	return nil
}

// BucketFor picks a bucket width that yields at most about 100 points per
// series and is never finer than the 30 second collection interval.
func BucketFor(w Window) time.Duration {
	span := w.End.Sub(w.Start)
	switch {
	case span <= 15*time.Minute:
		return 30 * time.Second
	case span <= time.Hour:
		return time.Minute
	case span <= 6*time.Hour:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// MetricSource reads fixed metric aggregations.
//
// NodeMetrics rows carry bucket, host, metric, state and value.
// ServiceMetrics rows carry stack, service, container, metric, avg and max.
type MetricSource interface {
	NodeMetrics(ctx context.Context, q MetricQuery) (Result, error)
	ServiceMetrics(ctx context.Context, q MetricQuery) (Result, error)
}

// Bounds for trace reads. A list page is for reading; a single trace is capped
// so one runaway trace cannot flood the page.
const (
	MaxTracePageSize = 50
	MaxTraceOffset   = 1000
	MaxTraceSpans    = 500
)

// ValidTraceID reports whether value is a 32 character lowercase hex trace id.
// Trace ids are the only caller-supplied value placed in a trace detail query,
// so this is a strict shape check rather than an escaping step.
func ValidTraceID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// TraceQuery is a newest-first, paged read of trace summaries. Service matches
// the OTel service.name of any span in the trace; ErrorsOnly keeps traces with
// at least one error span.
type TraceQuery struct {
	Service    string
	Window     Window
	ErrorsOnly bool
	Limit      int
	Offset     int
}

// Validate checks every bound that does not depend on the daemon inventory.
func (q TraceQuery) Validate() error {
	base := Query{Window: q.Window, Limit: 1}
	if err := base.Validate(); err != nil {
		return err
	}
	if q.Limit <= 0 || q.Limit > MaxTracePageSize {
		return fmt.Errorf("trace page size must be between 1 and %d", MaxTracePageSize)
	}
	if q.Offset < 0 || q.Offset > MaxTraceOffset {
		return fmt.Errorf("trace offset must be between 0 and %d", MaxTraceOffset)
	}
	if len([]rune(q.Service)) > MaxSearchRunes {
		return errors.New("service filter is too long")
	}
	return nil
}

// TraceSource reads trace summaries and the spans of one trace.
//
// Traces rows carry trace_id, started, duration_ns, spans, errors, root and
// service. Trace rows carry span_id, parent_id, name, service, kind, status,
// message, start_ns and duration_ns.
type TraceSource interface {
	Traces(ctx context.Context, q TraceQuery) (Result, error)
	Trace(ctx context.Context, window Window, traceID string) (Result, error)
}

// StatsSource reads the fixed aggregations behind the overview dashboard.
//
// LogVolume rows carry bucket, level (error, warn or info) and n. TraceStats
// returns one row with traces, error_traces and spans.
type StatsSource interface {
	LogVolume(ctx context.Context, q MetricQuery) (Result, error)
	TraceStats(ctx context.Context, q MetricQuery) (Result, error)
}

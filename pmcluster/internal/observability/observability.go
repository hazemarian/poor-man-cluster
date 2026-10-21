// Package observability contains the transport-independent contracts used by
// the native console observability surface.
//
// The package deliberately has no knowledge of Gin, Parseable, or pmapi.  A
// caller first proves that a service belongs to the daemon inventory, then
// passes a bounded Query to a Source implementation.  This keeps request
// parsing and backend request assembly out of the UI controller.
package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Bounds are the hard limits shared by all evidence sources.  A source may
// use tighter limits, but it must never widen these values from request data.
const (
	DefaultWindow       = time.Hour
	MaxWindow           = 24 * time.Hour
	DefaultRowLimit     = 500
	MaxRowLimit         = 50_000
	MaxResponseBytes    = 4 << 20
	MaxQueryDuration    = 10 * time.Second
	MaxServiceNameBytes = 255
)

// Status describes whether a source answered.  Empty and unavailable are
// intentionally different: an empty successful query is useful evidence.
type Status string

const (
	StatusAvailable   Status = "available"
	StatusUnavailable Status = "unavailable"
	StatusStale       Status = "stale"
)

// Window is an explicit UTC evidence interval.  Both ends are required by the
// Parseable API even when the SQL contains its own time predicate.
type Window struct {
	Start time.Time
	End   time.Time
}

// NewWindow returns a bounded interval ending at end.  A zero end uses the
// current UTC time.  The returned values are normalized to UTC.
func NewWindow(end time.Time, duration time.Duration) (Window, error) {
	if end.IsZero() {
		end = time.Now()
	}
	if duration <= 0 {
		return Window{}, errors.New("evidence window must be positive")
	}
	if duration > MaxWindow {
		return Window{}, fmt.Errorf("evidence window exceeds %s", MaxWindow)
	}
	end = end.UTC()
	return Window{Start: end.Add(-duration), End: end}, nil
}

// ParseWindow parses the small set of UI range values.  It accepts an explicit
// duration as a convenience for callers and clamps only through validation;
// callers should show a validation error rather than silently widen a query.
func ParseWindow(value string, now time.Time) (Window, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		value = "1h"
	}
	var duration time.Duration
	switch value {
	case "15m":
		duration = 15 * time.Minute
	case "1h":
		duration = time.Hour
	case "6h":
		duration = 6 * time.Hour
	case "24h", "1d":
		duration = MaxWindow
	default:
		var err error
		duration, err = time.ParseDuration(value)
		if err != nil {
			return Window{}, fmt.Errorf("invalid evidence range %q", value)
		}
	}
	return NewWindow(now, duration)
}

// Query is the only request a Source accepts.  There is no SQL or upstream
// URL field by design: those values belong to the adapter configuration.
type Query struct {
	Service string
	Window  Window
	Limit   int
}

// Validate checks bounds that are independent of a daemon's service
// inventory.  A controller must still resolve Service against that inventory
// before issuing the query.
func (q Query) Validate() error {
	if q.Window.Start.IsZero() || q.Window.End.IsZero() || !q.Window.Start.Before(q.Window.End) {
		return errors.New("evidence window must have start before end")
	}
	if q.Window.End.Sub(q.Window.Start) > MaxWindow {
		return fmt.Errorf("evidence window exceeds %s", MaxWindow)
	}
	if q.Limit <= 0 {
		return errors.New("evidence row limit must be positive")
	}
	if q.Limit > MaxRowLimit {
		return fmt.Errorf("evidence row limit exceeds %d", MaxRowLimit)
	}
	if len(q.Service) > MaxServiceNameBytes {
		return errors.New("service name is too long")
	}
	if strings.ContainsAny(q.Service, "\r\n\x00") {
		return errors.New("service name contains control characters")
	}
	return nil
}

// Row is a decoded telemetry record.  Values stay typed so adapters can
// preserve timestamps and numeric fields while the view escapes strings.
type Row map[string]any

// Result is a successful or partial response from a Source.  Available=true
// with zero Rows means the query answered successfully with no records.
type Result struct {
	Status            Status
	Rows              []Row
	ObservedAt        time.Time
	Window            Window
	Truncated         bool
	TruncationReasons []string
	Coverage          string
}

// Unavailable returns a result suitable for a failed source call while
// keeping the error outside the evidence payload shown to browsers.
func Unavailable(q Query, now time.Time, coverage string) Result {
	return Result{
		Status:     StatusUnavailable,
		ObservedAt: now.UTC(),
		Window:     q.Window,
		Coverage:   coverage,
	}
}

// Source is a bounded read-only telemetry source.
type Source interface {
	Query(ctx context.Context, q Query) (Result, error)
}

// ErrUnavailable marks a source that could not answer.  Adapters should wrap
// the underlying cause with this sentinel and redact the public message.
var ErrUnavailable = errors.New("observability source unavailable")

// ServicePath is the canonical UI identity used in evidence filters.
// Returning a pair prevents callers from accidentally treating a service
// name as a globally unique identifier.
type ServicePath struct {
	Stack   string
	Service string
}

func (p ServicePath) String() string {
	if p.Stack == "" {
		return p.Service
	}
	return p.Stack + "/" + p.Service
}

var servicePart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ParseServicePath validates the public service filter syntax.  The daemon
// inventory remains authoritative; this only rejects malformed input early.
func ParseServicePath(value string) (ServicePath, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > MaxServiceNameBytes {
		return ServicePath{}, errors.New("service filter is empty or too long")
	}
	parts := strings.Split(value, "/")
	if len(parts) == 1 {
		if !servicePart.MatchString(parts[0]) {
			return ServicePath{}, errors.New("invalid service filter")
		}
		return ServicePath{Service: parts[0]}, nil
	}
	if len(parts) != 2 || !servicePart.MatchString(parts[0]) || !servicePart.MatchString(parts[1]) {
		return ServicePath{}, errors.New("invalid service filter")
	}
	return ServicePath{Stack: parts[0], Service: parts[1]}, nil
}

// Log is a redacted log record ready for display or compaction.
type Log struct{ Timestamp, Severity, Service, Body string }

// ProjectLog selects supported log fields without exposing arbitrary metadata.
func ProjectLog(row Row) Log {
	text := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := row[key]; ok && v != nil {
				if str, ok := v.(string); ok {
					return str
				}
				b, err := json.Marshal(v)
				if err == nil {
					return string(b)
				}
			}
		}
		return ""
	}
	body := []rune(text("body", "message", "log"))
	if len(body) > 2000 {
		body = append(body[:2000], '…')
	}
	return Log{Timestamp: text("p_timestamp", "timestamp", "time"), Severity: text("severity_text", "severity", "level"), Service: text("service.name", "service_name", "service"), Body: string(body)}
}

// RowForLog constructs a record for evidence compaction from selected fields.
func RowForLog(service, severity, body, timestamp string) Row {
	return Row{"service.name": service, "severity_text": severity, "body": body, "p_timestamp": timestamp}
}

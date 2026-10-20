package observability

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// NodeReader is the small inventory capability the read service needs. The
// full runtime.Client is intentionally not required, which keeps the service
// usable with a fixture and prevents telemetry reads from gaining Docker
// mutation capabilities.
type NodeReader interface {
	NodeList(context.Context) ([]runtime.Node, error)
}

// Reader is the authenticated, read-only observability surface shared by the
// daemon API, the remote CLI adapter and the local CLI adapter.
type Reader interface {
	Status(context.Context, Window) (StatusResult, error)
	Logs(context.Context, LogQuery) (Result, error)
	NodeMetrics(context.Context, MetricQuery) (Result, error)
	ServiceMetrics(context.Context, MetricQuery) (Result, error)
	Traces(context.Context, TraceQuery) (Result, error)
	Trace(context.Context, Window, string) (Result, error)
}

// Status values used by the inventory comparison. Source Status remains
// available/unavailable; these values add the distinctions an operator needs
// when a cluster has no samples or only some nodes are reporting.
const (
	StatusEmpty           Status = "empty"
	StatusInventoryFailed Status = "inventory_failed"
	StatusMissing         Status = "missing"
	StatusFresh           Status = "fresh"
	StatusStaleNode       Status = "stale"
)

const DefaultTelemetryFreshness = 5 * time.Minute

// NodeHealth compares one expected Swarm node with its newest telemetry
// sample. LastSeen is zero for missing/unparseable samples.
type NodeHealth struct {
	ID              string
	Hostname        string
	Role            string
	Availability    string
	SwarmStatus     string
	IsLeader        bool
	TelemetryStatus Status
	LastSeen        time.Time
}

// StatusResult is deliberately an envelope even when a source is unavailable:
// daemon inventory remains useful and callers can distinguish empty data from
// an unreachable or malformed telemetry backend.
type StatusResult struct {
	Status             Status
	InventoryStatus    Status
	TelemetryStatus    Status
	ObservedAt         time.Time
	Window             Window
	Nodes              []NodeHealth
	ExpectedNodes      int
	ReportingNodes     int
	UnknownNodeSamples int
	// Truncated means the node metric read hit a bound. Per-node absence or age
	// is then not evidence of a missing or stale node; see Status.
	Truncated         bool
	TruncationReasons []string
}

// ReadService composes Swarm inventory with bounded evidence sources. It has
// no write methods and never accepts SQL, URLs or backend credentials from a
// request.
type ReadService struct {
	Inventory NodeReader
	LogsSrc   LogSource
	Metrics   MetricSource
	TracesSrc TraceSource
	Now       func() time.Time
	Freshness time.Duration
}

var _ Reader = (*ReadService)(nil)

func (s *ReadService) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *ReadService) freshness() time.Duration {
	if s != nil && s.Freshness > 0 {
		return s.Freshness
	}
	return DefaultTelemetryFreshness
}

func (s *ReadService) Logs(ctx context.Context, q LogQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	if s == nil || s.LogsSrc == nil {
		return Unavailable(Query{Service: q.Service, Window: q.Window, Limit: q.Limit}, s.now(), "logs source unavailable"), ErrUnavailable
	}
	return s.LogsSrc.Logs(ctx, q)
}

func (s *ReadService) NodeMetrics(ctx context.Context, q MetricQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	if s == nil || s.Metrics == nil {
		return Unavailable(Query{Window: q.Window, Limit: MaxMetricRows}, s.now(), "metrics source unavailable"), ErrUnavailable
	}
	return s.Metrics.NodeMetrics(ctx, q)
}

func (s *ReadService) ServiceMetrics(ctx context.Context, q MetricQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	if s == nil || s.Metrics == nil {
		return Unavailable(Query{Window: q.Window, Limit: MaxMetricRows}, s.now(), "metrics source unavailable"), ErrUnavailable
	}
	return s.Metrics.ServiceMetrics(ctx, q)
}

func (s *ReadService) Traces(ctx context.Context, q TraceQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	if s == nil || s.TracesSrc == nil {
		return Unavailable(Query{Service: q.Service, Window: q.Window, Limit: q.Limit}, s.now(), "traces source unavailable"), ErrUnavailable
	}
	return s.TracesSrc.Traces(ctx, q)
}

func (s *ReadService) Trace(ctx context.Context, w Window, traceID string) (Result, error) {
	if err := (Query{Window: w, Limit: 1}).Validate(); err != nil {
		return Result{}, err
	}
	if !ValidTraceID(traceID) {
		return Result{}, errors.New("trace id must be 32 lowercase hex characters")
	}
	if s == nil || s.TracesSrc == nil {
		return Unavailable(Query{Window: w, Limit: MaxTraceSpans}, s.now(), "traces source unavailable"), ErrUnavailable
	}
	return s.TracesSrc.Trace(ctx, w, traceID)
}

// Status compares the Swarm inventory with the newest fixed node metric row.
// A successful query with no rows is empty; a source failure is unavailable;
// missing and stale are attached to individual expected nodes and reflected
// in the aggregate status.
func (s *ReadService) Status(ctx context.Context, w Window) (StatusResult, error) {
	if err := (Query{Window: w, Limit: MaxMetricRows}).Validate(); err != nil {
		return StatusResult{}, err
	}
	now := s.now()
	out := StatusResult{
		Status:          StatusAvailable,
		InventoryStatus: StatusAvailable,
		TelemetryStatus: StatusAvailable,
		ObservedAt:      now,
		Window:          w,
	}
	if s == nil || s.Inventory == nil {
		out.Status = StatusInventoryFailed
		out.InventoryStatus = StatusInventoryFailed
		return out, nil
	}
	nodes, err := s.Inventory.NodeList(ctx)
	if err != nil {
		out.Status = StatusInventoryFailed
		out.InventoryStatus = StatusInventoryFailed
		return out, nil
	}
	out.ExpectedNodes = len(nodes)
	out.Nodes = make([]NodeHealth, 0, len(nodes))
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, NodeHealth{
			ID: n.ID, Hostname: n.Hostname, Role: n.Role,
			Availability: n.Availability, SwarmStatus: n.Status, IsLeader: n.IsLeader,
			TelemetryStatus: StatusMissing,
		})
	}
	if len(nodes) == 0 {
		out.Status = StatusEmpty
		out.TelemetryStatus = StatusEmpty
		return out, nil
	}
	if s.Metrics == nil {
		out.Status = StatusUnavailable
		out.TelemetryStatus = StatusUnavailable
		return out, nil
	}
	metricQuery := MetricQuery{Window: w, Bucket: BucketFor(w)}
	if err := metricQuery.Validate(); err != nil {
		return StatusResult{}, err
	}
	res, err := s.Metrics.NodeMetrics(ctx, metricQuery)
	if err != nil || res.Status == StatusUnavailable {
		out.Status = StatusUnavailable
		out.TelemetryStatus = StatusUnavailable
		return out, nil
	}
	out.Truncated = res.Truncated
	out.TruncationReasons = append([]string(nil), res.TruncationReasons...)
	latest := make(map[string]time.Time, len(res.Rows))
	for _, row := range res.Rows {
		host := rowString(row, "host", "host.name", "hostname")
		if host == "" {
			continue
		}
		// Bucketed display rows are intentionally coarse (up to 15 minutes for a
		// 24-hour view). Prefer the aggregate's actual newest sample so a fresh
		// node is not labelled stale merely because its bucket started earlier.
		seen, ok := evidenceTime(rowValue(row, "last_seen", "p_timestamp", "timestamp", "time", "bucket"))
		if !ok {
			continue
		}
		if prev, exists := latest[host]; !exists || seen.After(prev) {
			latest[host] = seen
		}
	}
	for host := range latest {
		found := false
		for i := range out.Nodes {
			if out.Nodes[i].Hostname != host && !strings.EqualFold(out.Nodes[i].Hostname, host) {
				continue
			}
			found = true
			last := latest[host]
			out.Nodes[i].LastSeen = last
			if now.Sub(last) <= s.freshness() || last.After(now) {
				out.Nodes[i].TelemetryStatus = StatusFresh
				out.ReportingNodes++
			} else {
				out.Nodes[i].TelemetryStatus = StatusStaleNode
				out.ReportingNodes++
			}
			break
		}
		if !found {
			out.UnknownNodeSamples++
		}
	}
	if res.Truncated {
		// Rows are capped oldest-first, so the newest sample for a node may have
		// been cut off. A fresh sample is still valid evidence; absence and
		// staleness are not. Keep the inventory and report telemetry as
		// unavailable instead of labelling healthy nodes stale or missing.
		for i := range out.Nodes {
			if out.Nodes[i].TelemetryStatus != StatusFresh {
				out.Nodes[i].TelemetryStatus = StatusUnavailable
			}
		}
		out.Status = StatusUnavailable
		out.TelemetryStatus = StatusUnavailable
		return out, nil
	}
	if len(res.Rows) == 0 {
		out.Status = StatusEmpty
		out.TelemetryStatus = StatusEmpty
		return out, nil
	}
	// Aggregate per-node evidence: any missing node makes the cluster missing,
	// otherwise any stale node makes it stale, otherwise every inventory node
	// has a fresh sample.
	missing, stale := false, false
	for _, node := range out.Nodes {
		switch node.TelemetryStatus {
		case StatusMissing:
			missing = true
		case StatusStaleNode:
			stale = true
		}
	}
	switch {
	case missing:
		out.Status = StatusMissing
	case stale:
		out.Status = StatusStale
	default:
		out.Status = StatusFresh
	}
	out.TelemetryStatus = out.Status
	return out, nil
}

func rowValue(row Row, keys ...string) any {
	for _, key := range keys {
		if value, ok := row[key]; ok {
			return value
		}
	}
	return nil
}

func rowString(row Row, keys ...string) string {
	value := rowValue(row, keys...)
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(stringValue(value))
}

func stringValue(value any) string {
	switch v := value.(type) {
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func evidenceTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		return v.UTC(), true
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC(), true
			}
		}
	case json.Number:
		if n, err := v.Float64(); err == nil {
			return unixEvidenceTime(n)
		}
	case float64:
		return unixEvidenceTime(v)
	case int64:
		return unixEvidenceTime(float64(v))
	case int:
		return unixEvidenceTime(float64(v))
	}
	return time.Time{}, false
}

func unixEvidenceTime(value float64) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}
	// Parseable may decode epoch seconds, milliseconds or nanoseconds depending
	// on the selected projection. Normalize all three without guessing for
	// ordinary small numbers.
	switch {
	case value > 1e17:
		return time.Unix(0, int64(value)).UTC(), true
	case value > 1e11:
		return time.UnixMilli(int64(value)).UTC(), true
	default:
		return time.Unix(int64(value), 0).UTC(), true
	}
}

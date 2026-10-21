package remote

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// Observability is the authenticated HTTP adapter for the daemon's bounded
// native telemetry reads. Backend credentials and Parseable URLs stay on the
// daemon; this client only carries the caller's bearer token.
type Observability struct{ c *Client }

func NewObservability(c *Client) observability.Reader { return &Observability{c: c} }

type observabilityWindowDTO struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type evidenceDTO struct {
	Status            string                 `json:"status"`
	ObservedAt        string                 `json:"observed_at"`
	Window            observabilityWindowDTO `json:"window"`
	Rows              []map[string]any       `json:"rows"`
	Truncated         bool                   `json:"truncated"`
	TruncationReasons []string               `json:"truncation_reasons"`
	Coverage          string                 `json:"coverage"`
	Error             string                 `json:"error"`
}

type nodeDTO struct {
	ID              string `json:"id"`
	Hostname        string `json:"hostname"`
	Role            string `json:"role"`
	Availability    string `json:"availability"`
	SwarmStatus     string `json:"swarm_status"`
	IsLeader        bool   `json:"is_leader"`
	TelemetryStatus string `json:"telemetry_status"`
	LastSeen        string `json:"last_seen"`
}

type statusDTO struct {
	Status             string                 `json:"status"`
	InventoryStatus    string                 `json:"inventory_status"`
	TelemetryStatus    string                 `json:"telemetry_status"`
	ObservedAt         string                 `json:"observed_at"`
	Window             observabilityWindowDTO `json:"window"`
	ExpectedNodes      int                    `json:"expected_nodes"`
	ReportingNodes     int                    `json:"reporting_nodes"`
	UnknownNodeSamples int                    `json:"unknown_node_samples"`
	Truncated          bool                   `json:"truncated"`
	TruncationReasons  []string               `json:"truncation_reasons"`
	Nodes              []nodeDTO              `json:"nodes"`
}

func (a *Observability) Status(ctx context.Context, w observability.Window) (observability.StatusResult, error) {
	if err := (observability.Query{Window: w, Limit: 1}).Validate(); err != nil {
		return observability.StatusResult{}, err
	}
	// Preserve the caller's absolute window across the daemon hop. Rebuilding
	// it from a duration at the receiver would move the start to the receiver's
	// current clock and break deterministic reads and pagination.
	path := "/observability/status?" + queryWindow(w)
	var out statusDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return observability.StatusResult{}, err
	}
	return statusFromDTO(out)
}

func (a *Observability) Logs(ctx context.Context, q observability.LogQuery) (observability.Result, error) {
	if err := q.Validate(); err != nil {
		return observability.Result{}, err
	}
	path := "/observability/logs?" + queryWindow(q.Window) + "&limit=" + itoa(q.Limit) + "&offset=" + itoa(q.Offset)
	if q.Service != "" {
		path += "&service=" + urlQueryEscape(q.Service)
	}
	if q.Severity != "" {
		path += "&severity=" + urlQueryEscape(q.Severity)
	}
	if q.Search != "" {
		path += "&search=" + urlQueryEscape(q.Search)
	}
	var out evidenceDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return observability.Result{}, err
	}
	return resultFromDTO(out)
}

func (a *Observability) NodeMetrics(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	return a.metrics(ctx, "/observability/metrics/nodes", q)
}

func (a *Observability) ServiceMetrics(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	return a.metrics(ctx, "/observability/metrics/services", q)
}

func (a *Observability) LogVolume(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	return a.metrics(ctx, "/observability/volume", q)
}

func (a *Observability) TraceStats(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	return a.metrics(ctx, "/observability/traces/stats", q)
}

// ServiceNames reads the distinct log service identities in the window.
func (a *Observability) ServiceNames(ctx context.Context, w observability.Window) ([]string, error) {
	if err := (observability.Query{Window: w, Limit: 1}).Validate(); err != nil {
		return nil, err
	}
	var out struct {
		Status   string   `json:"status"`
		Services []string `json:"services"`
	}
	if err := a.c.do(ctx, http.MethodGet, "/observability/services?"+queryWindow(w), nil, &out); err != nil {
		return nil, err
	}
	if out.Status == string(observability.StatusUnavailable) {
		return nil, observability.ErrUnavailable
	}
	if out.Services == nil {
		return []string{}, nil
	}
	return out.Services, nil
}

func (a *Observability) metrics(ctx context.Context, endpoint string, q observability.MetricQuery) (observability.Result, error) {
	if err := q.Validate(); err != nil {
		return observability.Result{}, err
	}
	path := endpoint + "?" + queryWindow(q.Window) + "&bucket=" + q.Bucket.String()
	var out evidenceDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return observability.Result{}, err
	}
	return resultFromDTO(out)
}

func (a *Observability) Traces(ctx context.Context, q observability.TraceQuery) (observability.Result, error) {
	if err := q.Validate(); err != nil {
		return observability.Result{}, err
	}
	path := "/observability/traces?" + queryWindow(q.Window) + "&limit=" + itoa(q.Limit) + "&offset=" + itoa(q.Offset)
	if q.Service != "" {
		path += "&service=" + urlQueryEscape(q.Service)
	}
	if q.ErrorsOnly {
		path += "&errors_only=true"
	}
	var out evidenceDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return observability.Result{}, err
	}
	return resultFromDTO(out)
}

func (a *Observability) Trace(ctx context.Context, w observability.Window, traceID string) (observability.Result, error) {
	if err := (observability.Query{Window: w, Limit: 1}).Validate(); err != nil {
		return observability.Result{}, err
	}
	if !observability.ValidTraceID(traceID) {
		return observability.Result{}, errors.New("trace id must be 32 lowercase hex characters")
	}
	path := "/observability/traces/" + traceID + "?" + queryWindow(w)
	var out evidenceDTO
	if err := a.c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return observability.Result{}, err
	}
	return resultFromDTO(out)
}

func queryWindow(w observability.Window) string {
	return "start=" + urlQueryEscape(w.Start.UTC().Format(time.RFC3339Nano)) + "&end=" + urlQueryEscape(w.End.UTC().Format(time.RFC3339Nano))
}

func resultFromDTO(d evidenceDTO) (observability.Result, error) {
	w, err := windowFromDTO(d.Window)
	if err != nil {
		return observability.Result{}, err
	}
	observed, _ := time.Parse(time.RFC3339Nano, d.ObservedAt)
	status := observability.Status(d.Status)
	if status == observability.StatusEmpty {
		status = observability.StatusAvailable
	}
	res := observability.Result{Status: status, Rows: rowsFromDTO(d.Rows), ObservedAt: observed.UTC(), Window: w, Truncated: d.Truncated, TruncationReasons: d.TruncationReasons, Coverage: d.Coverage}
	if d.Status == string(observability.StatusUnavailable) {
		return res, observability.ErrUnavailable
	}
	return res, nil
}

func statusFromDTO(d statusDTO) (observability.StatusResult, error) {
	w, err := windowFromDTO(d.Window)
	if err != nil {
		return observability.StatusResult{}, err
	}
	observed, _ := time.Parse(time.RFC3339Nano, d.ObservedAt)
	res := observability.StatusResult{Status: observability.Status(d.Status), InventoryStatus: observability.Status(d.InventoryStatus), TelemetryStatus: observability.Status(d.TelemetryStatus), ObservedAt: observed.UTC(), Window: w, ExpectedNodes: d.ExpectedNodes, ReportingNodes: d.ReportingNodes, UnknownNodeSamples: d.UnknownNodeSamples, Truncated: d.Truncated, TruncationReasons: d.TruncationReasons}
	res.Nodes = make([]observability.NodeHealth, 0, len(d.Nodes))
	for _, node := range d.Nodes {
		last, _ := time.Parse(time.RFC3339Nano, node.LastSeen)
		res.Nodes = append(res.Nodes, observability.NodeHealth{ID: node.ID, Hostname: node.Hostname, Role: node.Role, Availability: node.Availability, SwarmStatus: node.SwarmStatus, IsLeader: node.IsLeader, TelemetryStatus: observability.Status(node.TelemetryStatus), LastSeen: last.UTC()})
	}
	return res, nil
}

// rowsFromDTO converts decoded JSON rows into Row values, keeping a nil slice
// nil so absent rows and an empty array remain distinguishable.
func rowsFromDTO(in []map[string]any) []observability.Row {
	if in == nil {
		return nil
	}
	out := make([]observability.Row, 0, len(in))
	for _, row := range in {
		out = append(out, observability.Row(row))
	}
	return out
}

func windowFromDTO(d observabilityWindowDTO) (observability.Window, error) {
	start, err := time.Parse(time.RFC3339Nano, d.Start)
	if err != nil {
		return observability.Window{}, errors.New("daemon returned an invalid observability window")
	}
	end, err := time.Parse(time.RFC3339Nano, d.End)
	if err != nil {
		return observability.Window{}, errors.New("daemon returned an invalid observability window")
	}
	return observability.Window{Start: start.UTC(), End: end.UTC()}, nil
}

func urlQueryEscape(value string) string {
	return url.QueryEscape(value)
}

// Telemetry adapts the daemon's observability API to every source interface the
// console needs, so the console process never holds a backend credential and
// never talks to the telemetry store directly. It exists because the console
// asks for one thing the public API deliberately does not model: the oldest-first
// evidence slice a page renders (Source.Query). Everything else is a straight
// delegation to Observability.
type Telemetry struct{ *Observability }

// NewTelemetry builds the console's telemetry source over the daemon API.
func NewTelemetry(base, token string, timeout time.Duration) *Telemetry {
	return &Telemetry{Observability: NewObservability(New(base, token, timeout)).(*Observability)}
}

// Query implements observability.Source: the bounded, oldest-first evidence
// slice. The API pages newest-first (that is what an operator scrolling a log
// view wants), so the rows are reversed here rather than growing a second order
// parameter the public API does not need.
func (t *Telemetry) Query(ctx context.Context, q observability.Query) (observability.Result, error) {
	res, err := t.Logs(ctx, observability.LogQuery{
		Service: q.Service,
		Window:  q.Window,
		Limit:   q.Limit,
	})
	if err != nil {
		return res, err
	}
	for i, j := 0, len(res.Rows)-1; i < j; i, j = i+1, j-1 {
		res.Rows[i], res.Rows[j] = res.Rows[j], res.Rows[i]
	}
	return res, nil
}

var (
	_ observability.Source            = (*Telemetry)(nil)
	_ observability.LogSource         = (*Telemetry)(nil)
	_ observability.MetricSource      = (*Telemetry)(nil)
	_ observability.TraceSource       = (*Telemetry)(nil)
	_ observability.StatsSource       = (*Telemetry)(nil)
	_ observability.ServiceNameSource = (*Telemetry)(nil)
	_ observability.Reader            = (*Telemetry)(nil)
)

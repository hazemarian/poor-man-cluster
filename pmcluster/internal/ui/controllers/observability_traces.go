package controllers

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

const (
	obsTraceMaxDepthPx = 120
	obsTraceIndentPx   = 14
)

// obsTraceRow is one trace summary in the list.
type obsTraceRow struct {
	ID       string
	Short    string
	Started  string
	Root     string
	Service  string
	Spans    int
	Errors   int
	DurText  string
	DurPct   int
	HasError bool
}

// obsSpan is one bar in the waterfall. Left and Width are percentages of the
// trace extent; Indent is a bounded pixel offset for the tree depth.
type obsSpan struct {
	Name    string
	Service string
	Kind    string
	Message string
	DurText string
	Left    float64
	Width   float64
	Indent  int
	Error   bool
}

type obsTraces struct {
	Queried     bool
	Unavailable bool
	Empty       bool
	Rows        []obsTraceRow
	HasPrev     bool
	HasNext     bool
	PrevOffset  int
	NextOffset  int
	FirstRow    int
	LastRow     int

	// Detail is set when a single trace was requested.
	Detail     bool
	NotFound   bool
	TraceID    string
	Spans      []obsSpan
	TotalText  string
	SpanCount  int
	ErrorCount int
	Truncated  bool
}

// fmtDuration renders nanoseconds as µs, ms or s.
func fmtDuration(ns float64) string {
	switch {
	case ns < 1e3:
		return fmt.Sprintf("%.0f ns", ns)
	case ns < 1e6:
		return fmt.Sprintf("%.0f µs", ns/1e3)
	case ns < 1e9:
		return fmt.Sprintf("%.1f ms", ns/1e6)
	default:
		return fmt.Sprintf("%.2f s", ns/1e9)
	}
}

func intOf(v any) int {
	f, _ := toFloat(v)
	return int(f)
}

func (c Observability) loadTraces(g *gin.Context, d *observabilityData, window observability.Window, redactor observability.Redactor) {
	d.Traces.Queried = true
	d.Evidence.WindowStart = window.Start.UTC().Format(time.RFC3339)
	d.Evidence.WindowEnd = window.End.UTC().Format(time.RFC3339)
	if d.ErrKey == "observability.invalid_range" {
		d.Traces.Unavailable = true
		return
	}
	src, ok := c.Telemetry.(observability.TraceSource)
	if !ok {
		d.Traces.Unavailable = true
		return
	}
	if id := strings.TrimSpace(g.Query("trace")); id != "" {
		d.Traces.Detail = true
		d.Traces.TraceID = id
		if !observability.ValidTraceID(id) {
			d.Traces.NotFound = true
			return
		}
		res, err := src.Trace(g.Request.Context(), window, id)
		if err != nil || res.Status == observability.StatusUnavailable {
			d.Traces.Unavailable = true
			return
		}
		d.Traces.Truncated = res.Truncated
		d.Traces.NotFound = len(res.Rows) == 0
		d.Traces.Spans, d.Traces.TotalText, d.Traces.ErrorCount = buildWaterfall(res.Rows)
		d.Traces.SpanCount = len(res.Rows)
		return
	}
	// The service filter reaches a SQL literal in the Parseable adapter, so it
	// is checked against the daemon's service grammar first, exactly as the
	// logs tab and the daemon's own trace endpoint check theirs.
	service, validService := traceServiceFilter(d.TraceService)
	if !validService {
		d.ErrKey = "observability.invalid_service"
		d.Traces.Unavailable = true
		return
	}
	q := observability.TraceQuery{
		Service: service, Window: window, ErrorsOnly: d.ErrorsOnly,
		Limit: obsTracePageSize, Offset: d.TraceOffset,
	}
	res, err := src.Traces(g.Request.Context(), q)
	if err != nil || res.Status == observability.StatusUnavailable {
		d.Traces.Unavailable = true
		return
	}
	d.Traces.Empty = len(res.Rows) == 0
	d.Traces.Rows = projectTraces(res.Rows)
	d.Traces.HasNext = res.Truncated && d.TraceOffset+obsTracePageSize <= observability.MaxTraceOffset
	d.Traces.HasPrev = d.TraceOffset > 0
	d.Traces.PrevOffset = max(d.TraceOffset-obsTracePageSize, 0)
	d.Traces.NextOffset = min(d.TraceOffset+obsTracePageSize, observability.MaxTraceOffset)
	d.Traces.FirstRow = d.TraceOffset + 1
	d.Traces.LastRow = d.TraceOffset + len(d.Traces.Rows)
}

// traceServiceFilter applies the daemon's service-filter grammar to the console
// trace filter. An empty filter means no filter. "stack/service" maps to the
// backend's stack_service name, as the daemon's own filter does. The second
// result is false for input outside that grammar.
func traceServiceFilter(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	path, err := observability.ParseServicePath(value)
	if err != nil {
		return "", false
	}
	if path.Stack != "" {
		return path.Stack + "_" + path.Service, true
	}
	return path.Service, true
}

func projectTraces(rows []observability.Row) []obsTraceRow {
	var maxDur float64
	for _, r := range rows {
		if f, ok := toFloat(r["duration_ns"]); ok && f > maxDur {
			maxDur = f
		}
	}
	out := make([]obsTraceRow, 0, len(rows))
	for _, r := range rows {
		id := rowString(r, "trace_id")
		dur, _ := toFloat(r["duration_ns"])
		row := obsTraceRow{
			ID: id, Short: id, Started: shortTime(rowString(r, "started")),
			Root: rowString(r, "root"), Service: rowString(r, "service"),
			Spans: intOf(r["spans"]), Errors: intOf(r["errors"]), DurText: fmtDuration(dur),
		}
		if len(id) > 8 {
			row.Short = id[:8]
		}
		if maxDur > 0 {
			row.DurPct = max(int(dur/maxDur*100), 1)
		}
		row.HasError = row.Errors > 0
		out = append(out, row)
	}
	return out
}

// buildWaterfall orders spans depth-first by parent, positions each bar against
// the whole trace extent and returns the total duration text and error count.
// A span whose parent is not in the result is treated as a root so a trace
// with its root outside the window still renders.
func buildWaterfall(rows []observability.Row) ([]obsSpan, string, int) {
	type node struct {
		row      observability.Row
		id       string
		start    float64
		dur      float64
		children []*node
	}
	nodes := make([]*node, 0, len(rows))
	byID := map[string]*node{}
	var lo, hi float64
	for i, r := range rows {
		start, _ := toFloat(r["start_ns"])
		dur, _ := toFloat(r["duration_ns"])
		n := &node{row: r, id: rowString(r, "span_id"), start: start, dur: dur}
		nodes = append(nodes, n)
		byID[n.id] = n
		if i == 0 || start < lo {
			lo = start
		}
		if i == 0 || start+dur > hi {
			hi = start + dur
		}
	}
	var roots []*node
	for _, n := range nodes {
		parent := rowString(n.row, "parent_id")
		if p, ok := byID[parent]; ok && parent != "" && p != n {
			p.children = append(p.children, n)
		} else {
			roots = append(roots, n)
		}
	}
	extent := hi - lo
	var out []obsSpan
	errCount := 0
	seen := map[*node]bool{}
	var walk func(n *node, depth int)
	walk = func(n *node, depth int) {
		if seen[n] {
			return
		}
		seen[n] = true
		sort.SliceStable(n.children, func(i, j int) bool { return n.children[i].start < n.children[j].start })
		failed := intOf(n.row["status"]) == 2
		if failed {
			errCount++
		}
		span := obsSpan{
			Name: rowString(n.row, "name"), Service: rowString(n.row, "service"), Kind: strings.ToLower(rowString(n.row, "kind")),
			Message: rowString(n.row, "message"), DurText: fmtDuration(n.dur), Error: failed,
			Indent: min(depth*obsTraceIndentPx, obsTraceMaxDepthPx),
		}
		if extent > 0 {
			span.Left = fmin(fmax((n.start-lo)/extent*100, 0), 100)
			span.Width = fmin(fmax(n.dur/extent*100, 0.4), 100-span.Left)
		} else {
			span.Width = 100
		}
		out = append(out, span)
		for _, ch := range n.children {
			walk(ch, depth+1)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].start < roots[j].start })
	for _, r := range roots {
		walk(r, 0)
	}
	// Spans in a parent cycle are unreachable from any root; show them flat
	// rather than silently dropping them.
	for _, n := range nodes {
		walk(n, 0)
	}
	return out, fmtDuration(extent), errCount
}

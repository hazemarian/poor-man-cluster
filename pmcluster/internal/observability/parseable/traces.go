package parseable

import (
	"context"
	"errors"
	"strconv"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

var _ observability.TraceSource = (*Client)(nil)

// TracesSQL returns the fixed trace summary aggregation. A trace whose root
// span is outside the window has a NULL root and service; the caller labels it.
func (c *Client) TracesSQL(q observability.TraceQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	sql := "SELECT span_trace_id AS trace_id, MIN(span_start_time_unix_nano) AS started," +
		" MAX(span_end_time_unix_nano_epoch) - MIN(span_start_time_unix_nano_epoch) AS duration_ns," +
		" COUNT(*) AS spans, SUM(CASE WHEN span_status_code = 2 THEN 1 ELSE 0 END) AS errors," +
		" MAX(CASE WHEN span_parent_span_id = '' THEN span_name END) AS root," +
		" MAX(CASE WHEN span_parent_span_id = '' THEN `service.name` END) AS service" +
		" FROM `" + escapeIdentifier(c.traces) + "` WHERE " + timeBounds(q.Window)
	if q.Service != "" {
		sql += " AND `service.name` = '" + escapeLiteral(q.Service) + "'"
	}
	sql += " GROUP BY trace_id"
	if q.ErrorsOnly {
		sql += " HAVING SUM(CASE WHEN span_status_code = 2 THEN 1 ELSE 0 END) > 0"
	}
	return sql + " ORDER BY started DESC LIMIT " + strconv.Itoa(q.Limit+1) + " OFFSET " + strconv.Itoa(q.Offset), nil
}

// Traces returns one newest-first page of trace summaries. Truncated is true
// when a later page exists.
func (c *Client) Traces(ctx context.Context, q observability.TraceQuery) (observability.Result, error) {
	sql, err := c.TracesSQL(q)
	if err != nil {
		return observability.Result{}, err
	}
	rows, hasMore, err := c.execute(ctx, sql, q.Window)
	if err != nil {
		return observability.Result{}, err
	}
	more := hasMore || len(rows) > q.Limit
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return c.traceResult(rows, q.Window, more), nil
}

// TraceSQL returns the fixed span query for one trace id.
func (c *Client) TraceSQL(traceID string) (string, error) {
	if !observability.ValidTraceID(traceID) {
		return "", errors.New("trace id must be 32 lowercase hex characters")
	}
	return "SELECT span_span_id AS span_id, span_parent_span_id AS parent_id, span_name AS name, `service.name` AS service," +
		" span_kind_description AS kind, span_status_code AS status, span_status_message AS message," +
		" span_start_time_unix_nano_epoch AS start_ns, span_duration_ns AS duration_ns FROM `" + escapeIdentifier(c.traces) +
		"` WHERE span_trace_id = '" + traceID + "' ORDER BY span_start_time_unix_nano_epoch ASC LIMIT " +
		strconv.Itoa(observability.MaxTraceSpans+1), nil
}

// Trace returns the spans of one trace, earliest first. More than
// MaxTraceSpans rows is reported as Truncated.
func (c *Client) Trace(ctx context.Context, window observability.Window, traceID string) (observability.Result, error) {
	sql, err := c.TraceSQL(traceID)
	if err != nil {
		return observability.Result{}, err
	}
	if err := (observability.Query{Window: window, Limit: 1}).Validate(); err != nil {
		return observability.Result{}, err
	}
	rows, hasMore, err := c.execute(ctx, sql, window)
	if err != nil {
		return observability.Result{}, err
	}
	more := hasMore || len(rows) > observability.MaxTraceSpans
	if len(rows) > observability.MaxTraceSpans {
		rows = rows[:observability.MaxTraceSpans]
	}
	return c.traceResult(rows, window, more), nil
}

// traceResult redacts only the free-text fields. Ids, timestamps, durations and
// status codes are structured data the text redactor would corrupt (it rewrites
// long digit runs and stringifies numbers).
func (c *Client) traceResult(rows []observability.Row, w observability.Window, more bool) observability.Result {
	for _, row := range rows {
		for _, key := range []string{"name", "message", "root", "service"} {
			if s, ok := row[key].(string); ok {
				row[key] = c.redactor.Text(s)
			}
		}
	}
	return observability.Result{
		Status: observability.StatusAvailable, Rows: rows, ObservedAt: c.now().UTC(),
		Window: w, Truncated: more, Coverage: "bounded query window",
	}
}

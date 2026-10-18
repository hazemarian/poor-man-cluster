package parseable

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// severitySQL maps an allowlisted severity filter to the level spellings the
// collector and applications emit. Keys are the only accepted input.
var severitySQL = map[string]string{
	"error": "'error','err','fatal','critical','crit','panic','emerg','alert'",
	"warn":  "'warn','warning'",
	"info":  "'info','notice'",
	"debug": "'debug','trace'",
}

var (
	_ observability.LogSource    = (*Client)(nil)
	_ observability.MetricSource = (*Client)(nil)
)

func timeBounds(w observability.Window) string {
	return "p_timestamp >= TIMESTAMP '" + escapeLiteral(w.Start.UTC().Format(time.RFC3339Nano)) +
		"' AND p_timestamp <= TIMESTAMP '" + escapeLiteral(w.End.UTC().Format(time.RFC3339Nano)) + "'"
}

// escapeLike escapes a user search term for a LIKE pattern that declares
// backslash as its escape character, then for a SQL string literal.
func escapeLike(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
	return escapeLiteral(value)
}

// LogsPageSQL returns the exact SELECT sent by Logs. It reads one row more
// than the page size so the caller can tell whether another page exists.
func (c *Client) LogsPageSQL(q observability.LogQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	sql := "SELECT p_timestamp, severity_text, body, `service.name` FROM `" + escapeIdentifier(c.stream) + "` WHERE " + timeBounds(q.Window)
	if q.Service != "" {
		sql += " AND `service.name` = '" + escapeLiteral(q.Service) + "'"
	}
	if q.Severity != "" {
		sql += " AND LOWER(severity_text) IN (" + severitySQL[q.Severity] + ")"
	}
	if q.Search != "" {
		sql += " AND body ILIKE '%" + escapeLike(q.Search) + "%' ESCAPE '\\'"
	}
	return sql + " ORDER BY p_timestamp DESC LIMIT " + strconv.Itoa(q.Limit+1) + " OFFSET " + strconv.Itoa(q.Offset), nil
}

// Logs returns one newest-first page. Truncated is true when a later page
// exists.
func (c *Client) Logs(ctx context.Context, q observability.LogQuery) (observability.Result, error) {
	if q.Limit <= 0 || q.Limit > c.maxRows {
		q.Limit = min(c.maxRows, observability.MaxLogPageSize)
	}
	sql, err := c.LogsPageSQL(q)
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
	return observability.Result{
		Status:     observability.StatusAvailable,
		Rows:       c.redactKeepingTime(rows),
		ObservedAt: c.now().UTC(),
		Window:     q.Window,
		Truncated:  more,
		Coverage:   "bounded query window",
	}, nil
}

func bucketSQL(b time.Duration) string {
	return "date_bin(INTERVAL '" + strconv.Itoa(int(b/time.Second)) + " seconds', p_timestamp)"
}

// NodeMetricsSQL returns the fixed per-node aggregation: CPU idle share,
// memory used share and one-minute load, averaged per bucket and host. The
// utilization series are doubles; the integer usage series are not mapped by
// Parseable and cannot be charted.
func (c *Client) NodeMetricsSQL(q observability.MetricQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	return "SELECT " + bucketSQL(q.Bucket) + " AS bucket, `host.name` AS host, metric_name AS metric, state, AVG(data_point_value) AS value FROM `" +
		escapeIdentifier(c.metrics) + "` WHERE " + timeBounds(q.Window) +
		" AND ((metric_name = 'system.cpu.utilization' AND state = 'idle')" +
		" OR (metric_name = 'system.memory.utilization' AND state = 'used')" +
		" OR metric_name = 'system.cpu.load_average.1m')" +
		" GROUP BY bucket, host, metric, state ORDER BY bucket ASC LIMIT " + strconv.Itoa(observability.MaxMetricRows), nil
}

// NodeMetrics returns bucketed per-node series. Missing buckets are simply
// absent from the rows; callers must draw them as gaps, never as zero.
func (c *Client) NodeMetrics(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	sql, err := c.NodeMetricsSQL(q)
	if err != nil {
		return observability.Result{}, err
	}
	return c.metricResult(ctx, sql, q)
}

// ServiceMetricsSQL returns average and peak container CPU and memory per
// container over the window.
func (c *Client) ServiceMetricsSQL(q observability.MetricQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	return "SELECT `stack.name` AS stack, `compose.service` AS service, `container.name` AS container, metric_name AS metric," +
		" AVG(data_point_value) AS avg, MAX(data_point_value) AS max FROM `" + escapeIdentifier(c.metrics) + "` WHERE " + timeBounds(q.Window) +
		" AND metric_name IN ('container.cpu.utilization','container.memory.percent')" +
		" GROUP BY stack, service, container, metric ORDER BY max DESC LIMIT 400", nil
}

// ServiceMetrics returns per-container aggregates, highest peak first.
func (c *Client) ServiceMetrics(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	sql, err := c.ServiceMetricsSQL(q)
	if err != nil {
		return observability.Result{}, err
	}
	return c.metricResult(ctx, sql, q)
}

func (c *Client) metricResult(ctx context.Context, sql string, q observability.MetricQuery) (observability.Result, error) {
	rows, hasMore, err := c.execute(ctx, sql, q.Window)
	if err != nil {
		return observability.Result{}, err
	}
	if len(rows) > observability.MaxMetricRows {
		return observability.Result{}, fmt.Errorf("%w: metric response exceeded limit", observability.ErrUnavailable)
	}
	// Metric rows are bucket timestamps, numbers and infrastructure names, never
	// free text, so they bypass text redaction. The redactor would otherwise
	// rewrite date digits in the bucket as a phone number and stringify values.
	return observability.Result{
		Status:     observability.StatusAvailable,
		Rows:       rows,
		ObservedAt: c.now().UTC(),
		Window:     q.Window,
		Truncated:  hasMore,
		Coverage:   "bounded query window",
	}, nil
}

// redactKeepingTime redacts every field except the record timestamp, which is
// structured data the text redactor would otherwise rewrite as a phone number.
func (c *Client) redactKeepingTime(rows []observability.Row) []observability.Row {
	out := c.redactor.Rows(rows)
	for i := range rows {
		if ts, ok := rows[i]["p_timestamp"]; ok {
			out[i]["p_timestamp"] = ts
		}
	}
	return out
}

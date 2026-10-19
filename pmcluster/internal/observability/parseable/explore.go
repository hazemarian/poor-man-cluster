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
		sql += " AND " + severityPredicate(q.Severity)
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
//
// service.name is the shared metric attribute emitted by the embedded
// collector from the Swarm service label. The same identity is used by logs
// and traces, so every signal can be queried with one schema.
func (c *Client) ServiceMetricsSQL(q observability.MetricQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	return "SELECT `stack.name` AS stack, `service.name` AS service, `container.name` AS container, metric_name AS metric," +
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

var _ observability.StatsSource = (*Client)(nil)

// levelSQL buckets a record into error, warn or info. Container output has no
// level of its own and the collector labels stderr ERROR, so a record labelled
// error that plainly carries an info or debug level in its first 200
// characters is counted as info; counting it as an error would make a quiet
// system look broken. The patterns mirror investigation.inferSeverity.
const (
	stderrLabelSQL = "LOWER(severity_text) IN ('error','err','fatal','critical','crit','panic','emerg','alert')"
	// The first 200 characters carry the level in the formats the cluster's own
	// software uses: tab-separated (OTel collector), level= (logfmt), a bracket
	// prefix, glog (I1018), and PostgreSQL's "] LOG:" / "] WARNING:".
	infoHintSQL = "regexp_like(SUBSTR(body, 1, 200), '(?i)(\\tinfo\\t|\\tdebug\\t|level=(info|debug)|^[\\[(]?(info|debug)[\\]) :]|^I[0-9]{4} [0-9]|\\][ ]+(log|notice|info|statement|debug[0-9]?):)')"
	warnHintSQL = "regexp_like(SUBSTR(body, 1, 200), '(?i)(\\twarn(ing)?\\t|level=warn(ing)?|^[\\[(]?warn(ing)?[\\]) :]|^W[0-9]{4} [0-9]|\\][ ]+warning:)')"
	levelSQL    = "CASE WHEN " + stderrLabelSQL + " AND " + infoHintSQL + " THEN 'info'" +
		" WHEN " + stderrLabelSQL + " AND " + warnHintSQL + " THEN 'warn'" +
		" WHEN " + stderrLabelSQL + " THEN 'error'" +
		" WHEN LOWER(severity_text) IN ('warn','warning') THEN 'warn' ELSE 'info' END"
)

// LogVolumeSQL returns the fixed per-bucket record count by level.
func (c *Client) LogVolumeSQL(q observability.MetricQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	return "SELECT " + bucketSQL(q.Bucket) + " AS bucket, " + levelSQL + " AS level, COUNT(*) AS n FROM `" +
		escapeIdentifier(c.stream) + "` WHERE " + timeBounds(q.Window) +
		" GROUP BY bucket, level ORDER BY bucket ASC LIMIT " + strconv.Itoa(observability.MaxMetricRows), nil
}

// LogVolume returns bucketed record counts by level.
func (c *Client) LogVolume(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	sql, err := c.LogVolumeSQL(q)
	if err != nil {
		return observability.Result{}, err
	}
	return c.metricResult(ctx, sql, q)
}

// TraceStatsSQL returns the fixed trace totals for the window.
func (c *Client) TraceStatsSQL(q observability.MetricQuery) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	return "SELECT COUNT(DISTINCT span_trace_id) AS traces," +
		" COUNT(DISTINCT CASE WHEN span_status_code = 2 THEN span_trace_id END) AS error_traces, COUNT(*) AS spans FROM `" +
		escapeIdentifier(c.traces) + "` WHERE " + timeBounds(q.Window), nil
}

// TraceStats returns the number of traces, error traces and spans.
func (c *Client) TraceStats(ctx context.Context, q observability.MetricQuery) (observability.Result, error) {
	sql, err := c.TraceStatsSQL(q)
	if err != nil {
		return observability.Result{}, err
	}
	return c.metricResult(ctx, sql, q)
}

// severityPredicate filters by the same classification the dashboard counts, so
// the Error rate tile, Top errors and the Logs level filter agree. Error, warn
// and info use levelSQL, which corrects the stderr label; debug uses the stored
// level because nothing infers it.
func severityPredicate(severity string) string {
	switch severity {
	case "error":
		return "(" + levelSQL + ") = 'error'"
	case "warn":
		return "(" + levelSQL + ") = 'warn'"
	case "info":
		return "(" + levelSQL + ") = 'info' AND LOWER(severity_text) NOT IN (" + severitySQL["debug"] + ")"
	default:
		return "LOWER(severity_text) IN (" + severitySQL[severity] + ")"
	}
}

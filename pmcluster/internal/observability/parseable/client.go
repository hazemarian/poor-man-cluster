// Package parseable implements the bounded read-only Parseable adapter used by
// the native pmcluster console.
package parseable

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

var streamName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Config controls the adapter. Credentials are held only in the process and
// are used to construct an Authorization header; they are never put into a
// Result or an error string.
type Config struct {
	BaseURL       string
	Stream        string
	MetricsStream string
	Username      string
	Password      string
	AuthHeader    string
	HTTPClient    *http.Client
	MaxRows       int
	MaxBytes      int64
	Timeout       time.Duration
	MaxConcurrent int
	Now           func() time.Time
	Redactor      observability.Redactor
}

// Client is a fixed-contract Parseable query source.  It has no method that
// accepts arbitrary SQL or a caller-supplied URL.
type Client struct {
	baseURL    string
	stream     string
	metrics    string
	authHeader string
	http       *http.Client
	maxRows    int
	maxBytes   int64
	timeout    time.Duration
	now        func() time.Time
	sem        chan struct{}
	redactor   observability.Redactor
}

// New validates adapter configuration and returns a client.  The caller can
// leave BaseURL empty to disable telemetry; the UI will show an unavailable
// source and still render daemon-derived health.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("parseable endpoint is empty")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("parseable endpoint must be an http(s) URL without credentials")
	}
	stream := strings.TrimSpace(cfg.Stream)
	if stream == "" {
		stream = "pmcluster-logs"
	}
	if !streamName.MatchString(stream) {
		return nil, errors.New("parseable stream name is invalid")
	}
	metrics := strings.TrimSpace(cfg.MetricsStream)
	if metrics == "" {
		metrics = "pmcluster-metrics"
	}
	if !streamName.MatchString(metrics) {
		return nil, errors.New("parseable metrics stream name is invalid")
	}
	auth := strings.TrimSpace(cfg.AuthHeader)
	if auth == "" && cfg.Username != "" && cfg.Password != "" {
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.Username+":"+cfg.Password))
	}
	if auth != "" && !strings.HasPrefix(strings.ToLower(auth), "basic ") {
		return nil, errors.New("parseable auth header must use Basic authentication")
	}
	maxRows := cfg.MaxRows
	if maxRows <= 0 {
		maxRows = observability.DefaultRowLimit
	}
	if maxRows > observability.MaxRowLimit {
		maxRows = observability.MaxRowLimit
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 || maxBytes > observability.MaxResponseBytes {
		maxBytes = observability.MaxResponseBytes
	}
	timeout := cfg.Timeout
	if timeout <= 0 || timeout > observability.MaxQueryDuration {
		timeout = observability.MaxQueryDuration
	}
	concurrency := cfg.MaxConcurrent
	if concurrency <= 0 {
		concurrency = 2
	}
	if concurrency > 8 {
		concurrency = 8
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		baseURL: base, stream: stream, metrics: metrics, authHeader: auth, http: hc,
		maxRows: maxRows, maxBytes: maxBytes, timeout: timeout,
		now: now, sem: make(chan struct{}, concurrency), redactor: cfg.Redactor,
	}, nil
}

// Stream returns the configured, fixed stream name.
func (c *Client) Stream() string { return c.stream }

// LogsSQL returns the exact SELECT sent by Query.  It is exported for fixture
// tests and diagnostics; callers cannot supply their own SQL.
func (c *Client) LogsSQL(q observability.Query) (string, error) {
	if err := q.Validate(); err != nil {
		return "", err
	}
	start := q.Window.Start.UTC().Format(time.RFC3339Nano)
	end := q.Window.End.UTC().Format(time.RFC3339Nano)
	limit := q.Limit
	if limit <= 0 || limit > c.maxRows {
		limit = c.maxRows
	}
	// Parseable's OTLP rows expose the OTel resource attribute as
	// `service.name`; the stream and service identifiers are adapter-controlled
	// and SQL-escaped. The request body also carries startTime/endTime because
	// Parseable validates those bounds independently of the SQL predicate.
	sql := "SELECT p_timestamp, severity_text, body, `service.name` FROM `" + escapeIdentifier(c.stream) +
		"` WHERE p_timestamp >= TIMESTAMP '" + escapeLiteral(start) + "' AND p_timestamp <= TIMESTAMP '" + escapeLiteral(end) + "'"
	if q.Service != "" {
		sql += " AND `service.name` = '" + escapeLiteral(q.Service) + "'"
	}
	return sql + " ORDER BY p_timestamp ASC LIMIT " + strconv.Itoa(limit), nil
}

// Query executes the fixed log query. It returns an available empty result for
// a successful zero-row response, and a typed unavailable error for transport,
// HTTP, malformed, or over-limit responses.
func (c *Client) Query(ctx context.Context, q observability.Query) (observability.Result, error) {
	if err := q.Validate(); err != nil {
		return observability.Result{}, err
	}
	if q.Limit <= 0 || q.Limit > c.maxRows {
		q.Limit = c.maxRows
	}
	sql, err := c.LogsSQL(q)
	if err != nil {
		return observability.Result{}, err
	}
	rows, hasMore, err := c.execute(ctx, sql, q.Window)
	if err != nil {
		return observability.Result{}, err
	}
	rows = c.redactor.Rows(rows)
	truncated := hasMore || len(rows) >= q.Limit
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	reasons := []string(nil)
	if hasMore {
		reasons = append(reasons, "backend_has_more")
	}
	if len(rows) >= q.Limit {
		reasons = append(reasons, "row_limit")
	}
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		truncated = true
	}
	return observability.Result{
		Status:            observability.StatusAvailable,
		Rows:              rows,
		ObservedAt:        c.now().UTC(),
		Window:            q.Window,
		Truncated:         truncated,
		TruncationReasons: reasons,
		Coverage:          "bounded query window",
	}, nil
}

// execute sends one adapter-built SELECT and decodes the bounded response.
// Every public query path goes through here so the timeout, capacity gate,
// response cap and error redaction are applied uniformly.
func (c *Client) execute(ctx context.Context, sql string, window observability.Window) ([]observability.Row, bool, error) {
	qctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.acquire(qctx); err != nil {
		return nil, false, fmt.Errorf("%w: query capacity unavailable", observability.ErrUnavailable)
	}
	defer c.release()
	payload := map[string]string{
		"query":     sql,
		"startTime": window.Start.UTC().Format(time.RFC3339Nano),
		"endTime":   window.End.UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode request", observability.ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(qctx, http.MethodPost, c.baseURL+"/api/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("%w: build request", observability.ErrUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%w: query request failed", observability.ErrUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("%w: query returned HTTP %d", observability.ErrUnavailable, resp.StatusCode)
	}
	data, tooLarge, err := readBounded(resp.Body, c.maxBytes)
	if err != nil {
		return nil, false, fmt.Errorf("%w: read response", observability.ErrUnavailable)
	}
	if tooLarge {
		return nil, false, fmt.Errorf("%w: response exceeded limit", observability.ErrUnavailable)
	}
	rows, hasMore, err := decodeRows(data)
	if err != nil {
		return nil, false, fmt.Errorf("%w: malformed response", observability.ErrUnavailable)
	}
	return rows, hasMore, nil
}

func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errors.New("query capacity exhausted")
	}
}

func (c *Client) release() { <-c.sem }

func readBounded(r io.Reader, max int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) > max, nil
}

func decodeRows(data []byte) ([]observability.Row, bool, error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, false, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, false, errors.New("multiple JSON values")
		}
		return nil, false, err
	}
	var rowsRaw any
	hasMore := false
	switch value := raw.(type) {
	case []any:
		rowsRaw = value
	case map[string]any:
		for _, key := range []string{"data", "result", "records", "rows"} {
			if candidate, ok := value[key]; ok {
				rowsRaw = candidate
				break
			}
		}
		for _, key := range []string{"has_more", "hasMore", "truncated"} {
			if candidate, ok := value[key].(bool); ok && candidate {
				hasMore = true
			}
		}
		if nested, ok := rowsRaw.(map[string]any); ok {
			for _, key := range []string{"data", "rows"} {
				if candidate, found := nested[key]; found {
					rowsRaw = candidate
					break
				}
			}
		}
	default:
		return nil, false, errors.New("response is not an object or array")
	}
	rows, ok := rowsRaw.([]any)
	if !ok {
		return nil, false, errors.New("response does not contain a row array")
	}
	out := make([]observability.Row, len(rows))
	for i, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, false, errors.New("response row is not an object")
		}
		out[i] = observability.Row(row)
	}
	return out, hasMore, nil
}

func escapeIdentifier(value string) string { return strings.ReplaceAll(value, "`", "``") }

func escapeLiteral(value string) string { return strings.ReplaceAll(value, "'", "''") }

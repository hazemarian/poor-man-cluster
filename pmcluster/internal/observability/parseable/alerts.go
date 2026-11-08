package parseable

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

var _ observability.AlertSource = (*Client)(nil)

// Alerts lists the backend's configured alerts, bounded by the query limit.
// The rows are the backend's own alert objects, redacted before they leave the
// adapter. Alert definitions are not a time series, so there is no window and
// no SQL — the only parameter is the limit.
func (c *Client) Alerts(ctx context.Context, q observability.AlertQuery) (observability.Result, error) {
	if err := q.Validate(); err != nil {
		return observability.Result{}, err
	}
	rows, err := c.getJSON(ctx, "/api/v1/alerts")
	if err != nil {
		return observability.Result{}, err
	}
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	out := make([]observability.Row, len(rows))
	for i, row := range rows {
		out[i] = observability.Row(c.redactor.Value(map[string]any(row)).(map[string]any))
	}
	return observability.Result{
		Status:     observability.StatusAvailable,
		Rows:       out,
		ObservedAt: c.now().UTC(),
		Coverage:   "alert definitions",
	}, nil
}

// getJSON performs an authenticated GET against the Parseable API, applying the
// same timeout, capacity gate and response cap as the SQL query path.
func (c *Client) getJSON(ctx context.Context, path string) ([]observability.Row, error) {
	qctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.acquire(qctx); err != nil {
		return nil, fmt.Errorf("%w: query capacity unavailable", observability.ErrUnavailable)
	}
	defer c.release()
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", observability.ErrUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: alerts request failed", observability.ErrUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: alerts returned HTTP %d", observability.ErrUnavailable, resp.StatusCode)
	}
	data, tooLarge, err := readBounded(resp.Body, c.maxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: read response", observability.ErrUnavailable)
	}
	if tooLarge {
		return nil, fmt.Errorf("%w: response exceeded limit", observability.ErrUnavailable)
	}
	return decodeAlertRows(data)
}

// decodeAlertRows decodes the alerts list, tolerating both a bare array and the
// object-wrapped forms the backend may use.
//
// An object that carries NONE of the known envelopes is an error, not an empty
// list: "no alerts are configured" and "the response was not an alert list"
// are different facts, and reporting the second as the first hides a malformed
// or changed upstream behind a reassuring zero. A known key that is explicitly
// null is an empty list — that is how a backend with no alerts may render.
func decodeAlertRows(data []byte) ([]observability.Row, error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	var arr []any
	switch v := raw.(type) {
	case []any:
		arr = v
	case map[string]any:
		found := false
		for _, key := range []string{"alerts", "data", "result", "records", "rows", "items"} {
			candidate, ok := v[key]
			if !ok {
				continue
			}
			if list, ok := candidate.([]any); ok {
				arr, found = list, true
				break
			}
			if candidate == nil {
				arr, found = []any{}, true
				break
			}
		}
		if !found {
			return nil, errors.New("alerts response does not contain a list")
		}
	default:
		return nil, errors.New("alerts response is not an object or array")
	}
	out := make([]observability.Row, len(arr))
	for i, item := range arr {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("alert row is not an object")
		}
		out[i] = observability.Row(row)
	}
	return out, nil
}

package observability

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EdgeForwardConfig describes the one daemon-to-edge hop used to reach the
// Parseable source. The edge token is the managed edge_api_token; callers
// cannot provide a Parseable URL, SQL, or backend password.
type EdgeForwardConfig struct {
	BaseURL    string
	EdgeToken  string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// EdgeForward is a typed, read-only adapter for the edge's reserved internal
// observability route. It deliberately implements only the source interfaces;
// callers cannot use it to reach arbitrary edge paths.
type EdgeForward struct {
	baseURL string
	token   string
	http    *http.Client
	timeout time.Duration
}

var (
	_ LogSource    = (*EdgeForward)(nil)
	_ MetricSource = (*EdgeForward)(nil)
	_ TraceSource  = (*EdgeForward)(nil)
)

// NewEdgeForward validates the fixed edge endpoint and managed token.
func NewEdgeForward(cfg EdgeForwardConfig) (*EdgeForward, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("edge observability endpoint must be an http(s) URL without credentials")
	}
	token := strings.TrimSpace(cfg.EdgeToken)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("edge observability token is empty or invalid")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	// The edge endpoint is fixed and the request carries a managed credential.
	// Copy the caller's client before disabling redirects so an injected client
	// is not mutated for unrelated callers and credentials cannot follow a
	// redirect to another host.
	safeClient := *hc
	safeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	timeout := cfg.Timeout
	if timeout <= 0 || timeout > MaxQueryDuration {
		timeout = MaxQueryDuration
	}
	return &EdgeForward{baseURL: base, token: token, http: &safeClient, timeout: timeout}, nil
}

func (e *EdgeForward) Logs(ctx context.Context, q LogQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	values := windowValues(q.Window)
	values.Set("limit", fmt.Sprintf("%d", q.Limit))
	values.Set("offset", fmt.Sprintf("%d", q.Offset))
	if q.Service != "" {
		values.Set("service", q.Service)
	}
	if q.Severity != "" {
		values.Set("severity", q.Severity)
	}
	if q.Search != "" {
		values.Set("search", q.Search)
	}
	return e.get(ctx, "/internal/observability/logs", values)
}

func (e *EdgeForward) NodeMetrics(ctx context.Context, q MetricQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	values := windowValues(q.Window)
	values.Set("bucket", q.Bucket.String())
	return e.get(ctx, "/internal/observability/metrics/nodes", values)
}

func (e *EdgeForward) ServiceMetrics(ctx context.Context, q MetricQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	values := windowValues(q.Window)
	values.Set("bucket", q.Bucket.String())
	return e.get(ctx, "/internal/observability/metrics/services", values)
}

func (e *EdgeForward) Traces(ctx context.Context, q TraceQuery) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	values := windowValues(q.Window)
	values.Set("limit", fmt.Sprintf("%d", q.Limit))
	values.Set("offset", fmt.Sprintf("%d", q.Offset))
	if q.Service != "" {
		values.Set("service", q.Service)
	}
	if q.ErrorsOnly {
		values.Set("errors_only", "true")
	}
	return e.get(ctx, "/internal/observability/traces", values)
}

func (e *EdgeForward) Trace(ctx context.Context, w Window, traceID string) (Result, error) {
	if err := (Query{Window: w, Limit: 1}).Validate(); err != nil {
		return Result{}, err
	}
	if !ValidTraceID(traceID) {
		return Result{}, errors.New("trace id must be 32 lowercase hex characters")
	}
	return e.get(ctx, "/internal/observability/traces/"+traceID, windowValues(w))
}

func windowValues(w Window) url.Values {
	values := url.Values{}
	values.Set("start", w.Start.UTC().Format(time.RFC3339Nano))
	values.Set("end", w.End.UTC().Format(time.RFC3339Nano))
	return values
}

func (e *EdgeForward) get(ctx context.Context, path string, values url.Values) (Result, error) {
	if e == nil || e.http == nil {
		return Result{}, fmt.Errorf("%w: edge source is not configured", ErrUnavailable)
	}
	qctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, e.baseURL+path+"?"+values.Encode(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: build edge request", ErrUnavailable)
	}
	// This is the managed edge token, never the caller's bearer. The edge
	// checks it before dispatching to this reserved route and does not forward
	// it to Parseable.
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Accept", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		if qctx.Err() != nil {
			return Result{}, qctx.Err()
		}
		return Result{}, fmt.Errorf("%w: edge request failed", ErrUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return Result{}, fmt.Errorf("%w: edge returned HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil || int64(len(data)) > MaxResponseBytes {
		return Result{}, fmt.Errorf("%w: invalid edge response", ErrUnavailable)
	}
	var dto evidenceDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return Result{}, fmt.Errorf("%w: invalid edge response", ErrUnavailable)
	}
	result, err := resultFromEdgeDTO(dto)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func resultFromEdgeDTO(dto evidenceDTO) (Result, error) {
	w, err := edgeWindowFromDTO(dto.Window)
	if err != nil {
		return Result{}, err
	}
	observed, _ := time.Parse(time.RFC3339Nano, dto.ObservedAt)
	status := Status(dto.Status)
	if status == StatusEmpty {
		status = StatusAvailable
	}
	result := Result{
		Status:            status,
		Rows:              rowsFromDTO(dto.Rows),
		ObservedAt:        observed.UTC(),
		Window:            w,
		Truncated:         dto.Truncated,
		TruncationReasons: dto.TruncationReasons,
		Coverage:          dto.Coverage,
	}
	if dto.Status == string(StatusUnavailable) {
		return result, ErrUnavailable
	}
	return result, nil
}

// rowsFromDTO converts decoded JSON rows into Row values. A nil slice (absent
// or null "rows") stays nil; an explicit empty array stays empty but non-nil.
func rowsFromDTO(in []map[string]any) []Row {
	if in == nil {
		return nil
	}
	out := make([]Row, 0, len(in))
	for _, row := range in {
		out = append(out, Row(row))
	}
	return out
}

func edgeWindowFromDTO(dto windowDTO) (Window, error) {
	start, err := time.Parse(time.RFC3339Nano, dto.Start)
	if err != nil {
		return Window{}, errors.New("edge returned an invalid observability window")
	}
	end, err := time.Parse(time.RFC3339Nano, dto.End)
	if err != nil {
		return Window{}, errors.New("edge returned an invalid observability window")
	}
	w := Window{Start: start.UTC(), End: end.UTC()}
	if err := (Query{Window: w, Limit: 1}).Validate(); err != nil {
		return Window{}, errors.New("edge returned an invalid observability window")
	}
	return w, nil
}

// EdgeBearerValid performs the edge-side constant-time comparison for the
// single reserved internal route. It is separate from the HTTP reader so the
// edge process can keep its auth check ahead of any source call.
func EdgeBearerValid(header, managedToken string) bool {
	if managedToken == "" {
		return false
	}
	prefix := "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	provided := strings.TrimSpace(header[len(prefix):])
	if provided == "" || strings.ContainsAny(provided, "\r\n") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(managedToken)) == 1
}

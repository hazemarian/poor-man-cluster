package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

type readerFixture struct {
	logsResult   Result
	logsErr      error
	traceResult  Result
	traceErr     error
	statusResult StatusResult
	statusErr    error
	statsResult  Result
	statsErr     error
	names        []string
	namesErr     error
	logCalls     int
	traceCalls   int
	statsCalls   int
}

func (f *readerFixture) Status(context.Context, Window) (StatusResult, error) {
	return f.statusResult, f.statusErr
}
func (f *readerFixture) Logs(context.Context, LogQuery) (Result, error) {
	f.logCalls++
	return f.logsResult, f.logsErr
}
func (f *readerFixture) NodeMetrics(context.Context, MetricQuery) (Result, error) {
	return Result{Status: StatusAvailable}, nil
}
func (f *readerFixture) ServiceMetrics(context.Context, MetricQuery) (Result, error) {
	return Result{Status: StatusAvailable}, nil
}
func (f *readerFixture) Traces(context.Context, TraceQuery) (Result, error) {
	return Result{Status: StatusAvailable}, nil
}
func (f *readerFixture) Trace(context.Context, Window, string) (Result, error) {
	f.traceCalls++
	return f.traceResult, f.traceErr
}
func (f *readerFixture) LogVolume(context.Context, MetricQuery) (Result, error) {
	f.statsCalls++
	return f.statsResult, f.statsErr
}
func (f *readerFixture) TraceStats(context.Context, MetricQuery) (Result, error) {
	f.statsCalls++
	return f.statsResult, f.statsErr
}
func (f *readerFixture) ServiceNames(context.Context, Window) ([]string, error) {
	return f.names, f.namesErr
}

func observabilityRouter(f Reader, now time.Time) http.Handler {
	r := chi.NewRouter()
	(&HTTP{Svc: f, Now: func() time.Time { return now }}).Mount(r)
	return r
}

func decodeBody(t *testing.T, resp *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, resp.Body.String())
	}
	return body
}

func TestObservabilityHTTPRejectsInvalidArgumentsBeforeSource(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	f := &readerFixture{}
	h := observabilityRouter(f, now)

	for _, path := range []string{
		"/observability/logs?limit=201",
		"/observability/logs?severity=error%27%20OR%201%3D1",
		"/observability/logs?start=2026-10-20T11:00:00Z",
		"/observability/metrics/nodes?bucket=1s",
		"/observability/traces/invalid",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, req)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", resp.Code, resp.Body.String())
			}
		})
	}
	if f.logCalls != 0 || f.traceCalls != 0 {
		t.Fatalf("source calls = logs:%d trace:%d, want zero", f.logCalls, f.traceCalls)
	}
}

func TestObservabilityHTTPDistinguishesEmptyAndUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := Window{Start: now.Add(-time.Hour), End: now}

	t.Run("empty", func(t *testing.T) {
		f := &readerFixture{logsResult: Result{Status: StatusAvailable, Window: w}}
		resp := httptest.NewRecorder()
		observabilityRouter(f, now).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/logs?range=1h", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d", resp.Code)
		}
		body := decodeBody(t, resp)
		if body["status"] != string(StatusEmpty) || body["error"] != nil {
			t.Fatalf("body = %#v, want empty without error", body)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		f := &readerFixture{logsResult: Result{Status: StatusUnavailable, Window: w}, logsErr: errors.New("secret backend details")}
		resp := httptest.NewRecorder()
		observabilityRouter(f, now).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/logs?range=1h", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d", resp.Code)
		}
		body := decodeBody(t, resp)
		if body["status"] != string(StatusUnavailable) || body["error"] != "observability source unavailable" {
			t.Fatalf("body = %#v, want sanitized unavailable envelope", body)
		}
		if strings.Contains(resp.Body.String(), "secret backend") {
			t.Fatal("backend error leaked into response")
		}
	})
}

func TestObservabilityHTTPUsesLowerSnakeCaseAndStripsTerminalControls(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := Window{Start: now.Add(-time.Hour), End: now}
	f := &readerFixture{logsResult: Result{
		Status: StatusAvailable,
		Window: w,
		Rows:   []Row{{"service.name": "web", "body": "\x1b[31mboom\x1b[0m", "nested": map[string]any{"value": "\x1b[2Ksafe"}}},
	}}
	resp := httptest.NewRecorder()
	observabilityRouter(f, now).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/logs", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if strings.Contains(resp.Body.String(), "\x1b[") {
		t.Fatalf("terminal escape leaked: %q", resp.Body.String())
	}
	body := decodeBody(t, resp)
	if _, ok := body["observed_at"]; !ok {
		t.Fatalf("response missing lower_snake observed_at: %#v", body)
	}
	if _, ok := body["observedAt"]; ok {
		t.Fatal("camelCase DTO field present")
	}
}

func TestObservabilityHTTPStatusEnvelope(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := Window{Start: now.Add(-time.Hour), End: now}
	f := &readerFixture{statusResult: StatusResult{
		Status: StatusFresh, InventoryStatus: StatusAvailable, TelemetryStatus: StatusFresh,
		ObservedAt: now, Window: w, ExpectedNodes: 3, ReportingNodes: 3,
	}}
	resp := httptest.NewRecorder()
	observabilityRouter(f, now).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/status", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	body := decodeBody(t, resp)
	if body["status"] != string(StatusFresh) || body["expected_nodes"] != float64(3) {
		t.Fatalf("status envelope = %#v", body)
	}
	if body["truncated"] != false {
		t.Fatalf("untruncated status must still carry truncated=false: %#v", body)
	}
}

func TestObservabilityHTTPStatusTruncationFields(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := Window{Start: now.Add(-time.Hour), End: now}
	f := &readerFixture{statusResult: StatusResult{
		Status: StatusUnavailable, InventoryStatus: StatusAvailable, TelemetryStatus: StatusUnavailable,
		ObservedAt: now, Window: w, ExpectedNodes: 1,
		Truncated: true, TruncationReasons: []string{"row_limit"},
	}}
	resp := httptest.NewRecorder()
	observabilityRouter(f, now).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/status", nil))
	body := decodeBody(t, resp)
	if body["truncated"] != true {
		t.Fatalf("status JSON truncated = %#v, want true: %#v", body["truncated"], body)
	}
	reasons, ok := body["truncation_reasons"].([]any)
	if !ok || len(reasons) != 1 || reasons[0] != "row_limit" {
		t.Fatalf("status JSON truncation_reasons = %#v, want [row_limit]", body["truncation_reasons"])
	}
}

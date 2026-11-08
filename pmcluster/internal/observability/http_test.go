package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	alertsResult Result
	alertsErr    error
	logCalls     int
	traceCalls   int
	statsCalls   int
	alertCalls   int
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
func (f *readerFixture) Alerts(context.Context, AlertQuery) (Result, error) {
	f.alertCalls++
	return f.alertsResult, f.alertsErr
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

// TestObservabilityHTTPOverviewRoutes covers the three reads the console
// overview needs, which used to live inside the console process: log volume by
// level, trace totals, and the service-name list. They are part of the daemon
// API now, so the console never holds a backend client — and that means their
// envelopes are a contract the console depends on.
func TestObservabilityHTTPOverviewRoutes(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	w := Window{Start: now.Add(-time.Hour), End: now}

	t.Run("volume and trace stats are evidence envelopes", func(t *testing.T) {
		f := &readerFixture{statsResult: Result{
			Status: StatusAvailable, Window: w,
			Rows: []Row{{"bucket": "2026-10-20T11:00:00Z", "level": "error", "n": 3}},
		}}
		h := observabilityRouter(f, now)

		for _, path := range []string{"/observability/volume?range=1h", "/observability/traces/stats?range=1h"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("%s status = %d, want 200 (%s)", path, resp.Code, resp.Body.String())
			}
			body := decodeBody(t, resp)
			if body["status"] != string(StatusAvailable) {
				t.Errorf("%s status field = %v, want available", path, body["status"])
			}
			rows, _ := body["rows"].([]any)
			if len(rows) != 1 {
				t.Errorf("%s returned %d rows, want 1", path, len(rows))
			}
		}
		if f.statsCalls != 2 {
			t.Errorf("stats source calls = %d, want 2", f.statsCalls)
		}
	})

	t.Run("service names are a list, and unavailability is distinguishable", func(t *testing.T) {
		f := &readerFixture{names: []string{"demo_web", "shop_api"}}
		h := observabilityRouter(f, now)

		req := httptest.NewRequest(http.MethodGet, "/observability/services?range=1h", nil)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		body := decodeBody(t, resp)
		if body["status"] != string(StatusAvailable) {
			t.Fatalf("status = %v, want available", body["status"])
		}
		names, _ := body["services"].([]any)
		if len(names) != 2 || names[0] != "demo_web" {
			t.Fatalf("services = %v, want [demo_web shop_api]", names)
		}

		unavailable := &readerFixture{namesErr: ErrUnavailable}
		req = httptest.NewRequest(http.MethodGet, "/observability/services?range=1h", nil)
		resp = httptest.NewRecorder()
		observabilityRouter(unavailable, now).ServeHTTP(resp, req)
		body = decodeBody(t, resp)
		if body["status"] != string(StatusUnavailable) {
			t.Errorf("unavailable status = %v, want unavailable", body["status"])
		}
		if got, _ := body["services"].([]any); len(got) != 0 {
			t.Errorf("unavailable services = %v, want empty", got)
		}
	})

	t.Run("an unavailable source never becomes a 500", func(t *testing.T) {
		f := &readerFixture{statsResult: Result{Status: StatusUnavailable, Window: w}, statsErr: ErrUnavailable}
		h := observabilityRouter(f, now)
		req := httptest.NewRequest(http.MethodGet, "/observability/volume?range=1h", nil)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 with an unavailable envelope", resp.Code)
		}
		if decodeBody(t, resp)["status"] != string(StatusUnavailable) {
			t.Error("expected status=unavailable")
		}
	})

	t.Run("the literal paths are not read as a trace id", func(t *testing.T) {
		f := &readerFixture{}
		h := observabilityRouter(f, now)
		req := httptest.NewRequest(http.MethodGet, "/observability/traces/stats?range=1h", nil)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("/traces/stats = %d, want 200 (%s)", resp.Code, resp.Body.String())
		}
		if f.traceCalls != 0 {
			t.Error("/traces/stats reached the single-trace handler")
		}
	})
}

func TestObservabilityHTTPAlerts(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	t.Run("bounded list", func(t *testing.T) {
		f := &readerFixture{alertsResult: Result{
			Status: StatusAvailable,
			Rows:   []Row{{"name": "disk-full", "triggered": true}},
		}}
		h := observabilityRouter(f, now)
		req := httptest.NewRequest(http.MethodGet, "/observability/alerts?limit=500", nil)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.Code, resp.Body.String())
		}
		body := decodeBody(t, resp)
		if body["status"] != string(StatusAvailable) {
			t.Fatalf("status = %v, want available", body["status"])
		}
		alerts, _ := body["alerts"].([]any)
		if len(alerts) != 1 {
			t.Fatalf("alerts = %v, want 1 entry", alerts)
		}
	})

	t.Run("limit is bounded", func(t *testing.T) {
		f := &readerFixture{}
		h := observabilityRouter(f, now)
		for _, path := range []string{"/observability/alerts?limit=0", "/observability/alerts?limit=501"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, req)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, want 400", path, resp.Code)
			}
		}
		if f.alertCalls != 0 {
			t.Fatal("bounded rejection reached the source")
		}
	})

	t.Run("unavailable is distinguishable, never a 500", func(t *testing.T) {
		f := &readerFixture{alertsErr: ErrUnavailable}
		h := observabilityRouter(f, now)
		req := httptest.NewRequest(http.MethodGet, "/observability/alerts", nil)
		resp := httptest.NewRecorder()
		h.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.Code)
		}
		body := decodeBody(t, resp)
		if body["status"] != string(StatusUnavailable) {
			t.Fatalf("status = %v, want unavailable", body["status"])
		}
	})
}

// alertRecordingReader is a source that counts calls (via countingReader) and
// records the exact AlertQuery the handler built, so a limit assertion is about
// the value that reached the source rather than about the response.
type alertRecordingReader struct {
	*countingReader
	mu   sync.Mutex
	seen []AlertQuery
}

func (r *alertRecordingReader) Alerts(ctx context.Context, q AlertQuery) (Result, error) {
	r.mu.Lock()
	r.seen = append(r.seen, q)
	r.mu.Unlock()
	return r.countingReader.Alerts(ctx, q)
}

func (r *alertRecordingReader) queries() []AlertQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AlertQuery(nil), r.seen...)
}

// TestObservabilityHTTPAlertsLimitReachesTheSource: the bound the caller asks
// for is the bound the source receives, and the default is the page cap (500).
func TestObservabilityHTTPAlertsLimitReachesTheSource(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/observability/alerts", want: MaxAlertPageSize},
		{path: "/observability/alerts?limit=1", want: 1},
		{path: "/observability/alerts?limit=7", want: 7},
		{path: "/observability/alerts?limit=500", want: MaxAlertPageSize},
	} {
		t.Run(tc.path, func(t *testing.T) {
			src := &alertRecordingReader{countingReader: &countingReader{}}
			h := observabilityRouter(src, now)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", resp.Code, resp.Body.String())
			}
			got := src.queries()
			if len(got) != 1 {
				t.Fatalf("source calls = %d, want 1", len(got))
			}
			if got[0].Limit != tc.want {
				t.Fatalf("source received limit %d, want %d", got[0].Limit, tc.want)
			}
		})
	}
}

// TestObservabilityHTTPAlertsRefusesOutOfRangeLimit: 0, negative, over the cap
// and non-numeric are all 400s that name the range, and none of them reaches
// the source (so a caller cannot cost a backend read by probing the bound).
func TestObservabilityHTTPAlertsRefusesOutOfRangeLimit(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, limit := range []string{"0", "-1", "501", "5000", "abc", "1.5"} {
		t.Run(limit, func(t *testing.T) {
			src := &alertRecordingReader{countingReader: &countingReader{}}
			h := observabilityRouter(src, now)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/alerts?limit="+limit, nil))

			if resp.Code != http.StatusBadRequest {
				t.Fatalf("limit %s status = %d, want 400", limit, resp.Code)
			}
			body := decodeBody(t, resp)
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, "500") {
				t.Errorf("error %q must name the 500 cap", msg)
			}
			if n := len(src.queries()); n != 0 {
				t.Fatalf("limit %s reached the source %d times, want 0", limit, n)
			}
		})
	}
}

// TestObservabilityHTTPAlertsUnavailableKeepsTheDocumentedShape: an upstream
// failure is a 200 with status=unavailable, an empty (never null) alerts array
// and an error string — the shape in openapi.yaml — and never a 500 or an
// upstream error leaked to the caller.
func TestObservabilityHTTPAlertsUnavailableKeepsTheDocumentedShape(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		src  Reader
	}{
		{name: "source error", src: &readerFixture{alertsErr: ErrUnavailable}},
		{name: "source reports unavailable", src: &readerFixture{alertsResult: Result{Status: StatusUnavailable}}},
		{name: "no source wired", src: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := observabilityRouter(tc.src, now)
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/alerts?limit=10", nil))

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want the documented 200 (%s)", resp.Code, resp.Body.String())
			}
			body := decodeBody(t, resp)
			if body["status"] != string(StatusUnavailable) {
				t.Errorf("status = %v, want unavailable", body["status"])
			}
			alerts, ok := body["alerts"].([]any)
			if !ok {
				t.Fatalf("alerts = %#v, want an array (never null)", body["alerts"])
			}
			if len(alerts) != 0 {
				t.Errorf("alerts = %v, want empty", alerts)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Error("a failed read must carry an error string")
			}
			if strings.Contains(resp.Body.String(), "chromium") {
				t.Errorf("response leaked internals: %s", resp.Body.String())
			}
		})
	}
}

// TestObservabilityHTTPAlertsConcurrentPairSharesOneUpstreamRead is the cache
// working through the handler: two identical requests issued together must
// produce ONE upstream read, and both callers must get the same alert list.
func TestObservabilityHTTPAlertsConcurrentPairSharesOneUpstreamRead(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	src := &alertRecordingReader{countingReader: &countingReader{delay: 50 * time.Millisecond}}
	h := observabilityRouter(NewCachedReader(src), now)

	type answer struct {
		code int
		body string
	}
	answers := make([]answer, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp := httptest.NewRecorder()
			h.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/observability/alerts?limit=25", nil))
			answers[i] = answer{code: resp.Code, body: resp.Body.String()}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := len(src.queries()); got != 1 {
		t.Fatalf("upstream reads = %d, want 1 (identical concurrent requests must share one read)", got)
	}
	for i, a := range answers {
		if a.code != http.StatusOK {
			t.Fatalf("caller %d status = %d, want 200 (%s)", i, a.code, a.body)
		}
		if !strings.Contains(a.body, `"available"`) {
			t.Fatalf("caller %d did not get an available list: %s", i, a.body)
		}
	}
	if answers[0].body != answers[1].body {
		t.Fatalf("the two callers saw different bodies:\n%s\n%s", answers[0].body, answers[1].body)
	}
	// The limit that reached the source is the bounded one.
	if q := src.queries()[0]; q.Limit != 25 {
		t.Fatalf("upstream limit = %d, want 25", q.Limit)
	}
}

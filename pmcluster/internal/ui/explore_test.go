package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

type exploreFixture struct {
	evidenceFixture
	logs, node, svc atomic.Int32
	lastLog         observability.LogQuery
}

func (f *exploreFixture) Logs(_ context.Context, q observability.LogQuery) (observability.Result, error) {
	f.logs.Add(1)
	f.lastLog = q
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Truncated: true,
		Rows: []observability.Row{{"service.name": "demo_web", "body": "line one\nline two <b>x</b>", "severity_text": "ERROR", "p_timestamp": "2026-10-18T19:07:30.310"}}}, nil
}

func (f *exploreFixture) NodeMetrics(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	f.node.Add(1)
	t0 := q.Window.End.Add(-30 * time.Minute).UTC().Format("2006-01-02T15:04:05")
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, Rows: []observability.Row{
		{"bucket": t0, "host": "n1", "metric": observability.MetricNodeCPUUtilization, "state": "idle", "value": 0.75},
		{"bucket": t0, "host": "n1", "metric": observability.MetricNodeMemUtilization, "state": "used", "value": 0.5},
	}}, nil
}

func (f *exploreFixture) ServiceMetrics(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	f.svc.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, Rows: []observability.Row{
		{"stack": "demo", "service": "web", "metric": observability.MetricContainerMemoryPercent, "avg": 40.0, "max": 55.0},
	}}, nil
}

// obsLogPageSizeForTest mirrors the controller page size.
const obsLogPageSizeForTest = 50

func exploreApp(t *testing.T, me int, f *exploreFixture) *App {
	t.Helper()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			w.WriteHeader(me)
			fmt.Fprint(w, `{"id":1,"name":"edge"}`)
		case "/api/services":
			fmt.Fprint(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":1,"desired":1},{"name":"demo_db","stack":"demo","replicas":0,"desired":1}]}`)
		case "/api/nodes":
			fmt.Fprint(w, `{"nodes":[{"hostname":"n1","role":"manager","status":"ready","availability":"active"}]}`)
		case "/api/backups":
			fmt.Fprint(w, `{"backups":[]}`)
		case "/api/tls/hosts":
			fmt.Fprint(w, `{"hosts":[]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(daemon.Close)
	app, err := NewApp(Config{DataDir: ":memory:", PMAPIURL: daemon.URL, PMAPIToken: "bare-credential", LoginDisabled: true, UpstreamTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Store.Close() })
	app.ctrl.Telemetry = f
	return app
}

func get(app *App, target string) (int, string) {
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://x"+target, nil))
	return w.Code, w.Body.String()
}

func TestOverviewShowsNodeChartsGroupedServicesAndNoLogQuery(t *testing.T) {
	f := &exploreFixture{}
	_, body := get(exploreApp(t, 200, f), "/web/observability")
	if f.logs.Load() != 0 || f.calls.Load() != 0 || f.svc.Load() != 0 {
		t.Fatalf("overview must not read logs or service metrics: %d %d %d", f.logs.Load(), f.calls.Load(), f.svc.Load())
	}
	for _, want := range []string{`class="obs-node"`, `class="obs-line"`, "25.0%", "50.0%", `class="obs-stack"`, "demo"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// The degraded stack opens by default so a problem is never below the fold.
	if !strings.Contains(body, `<details class="obs-stack" open>`) {
		t.Error("unhealthy stack should be open")
	}
}

func TestLogsTabPagesNewestFirstAndEscapes(t *testing.T) {
	f := &exploreFixture{}
	code, body := get(exploreApp(t, 200, f), "/web/observability/evidence?service=demo/web&severity=error&search=boom&offset=50")
	if code != 200 || f.logs.Load() != 1 {
		t.Fatalf("code=%d logs=%d", code, f.logs.Load())
	}
	q := f.lastLog
	if q.Service != "demo_web" || q.Severity != "error" || q.Search != "boom" || q.Offset != 50 || q.Limit != obsLogPageSizeForTest {
		t.Fatalf("query: %+v", q)
	}
	if strings.Contains(body, "<b>x</b>") || !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Error("log body must be escaped")
	}
	if !strings.Contains(body, "Older") || !strings.Contains(body, "Newer") {
		t.Error("pager missing")
	}
}

func TestInjectedFiltersAreDroppedNotForwarded(t *testing.T) {
	f := &exploreFixture{}
	get(exploreApp(t, 200, f), "/web/observability/evidence?severity=error%27)%20OR%201%3D1&offset=999999&search="+strings.Repeat("a", 500))
	q := f.lastLog
	if q.Severity != "" || q.Offset != observability.MaxLogOffset || len([]rune(q.Search)) != observability.MaxSearchRunes {
		t.Fatalf("filters not sanitised: %+v", q)
	}
}

func TestExploreTabsFailClosedWithoutDaemonScope(t *testing.T) {
	for _, target := range []string{"/web/observability?tab=logs", "/web/observability?tab=metrics", "/web/observability", "/web/observability/evidence"} {
		f := &exploreFixture{}
		get(exploreApp(t, 403, f), target)
		if f.logs.Load()+f.calls.Load()+f.node.Load()+f.svc.Load() != 0 {
			t.Errorf("%s reached the telemetry source without scope", target)
		}
	}
}

func TestMetricsTabReadsNodeAndContainerAggregates(t *testing.T) {
	f := &exploreFixture{}
	_, body := get(exploreApp(t, 200, f), "/web/observability?tab=metrics&range=6h")
	if f.node.Load() != 1 || f.svc.Load() != 1 {
		t.Fatalf("node=%d svc=%d", f.node.Load(), f.svc.Load())
	}
	for _, want := range []string{"demo/web", "55.0%", `class="obs-bar-track"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

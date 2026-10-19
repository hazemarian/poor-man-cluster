package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
)

type fleetTelemetryFixture struct {
	queries   atomic.Int32
	body      string
	host      string
	container string
	logsErr   bool
}

func (f *fleetTelemetryFixture) Query(context.Context, observability.Query) (observability.Result, error) {
	f.queries.Add(1)
	return observability.Result{Status: observability.StatusAvailable}, nil
}

func (f *fleetTelemetryFixture) NodeMetrics(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	f.queries.Add(1)
	at := q.Window.End.UTC().Truncate(time.Minute).Format("2006-01-02T15:04:05.999999999")
	rows := []observability.Row{
		{"bucket": at, "host": f.host, "metric": observability.MetricNodeCPUUtilization, "value": 0.2},
		{"bucket": at, "host": f.host, "metric": observability.MetricNodeMemUtilization, "value": 0.3},
		{"bucket": at, "host": f.host, "metric": observability.MetricNodeLoad1m, "value": 0.4},
	}
	return observability.Result{Status: observability.StatusAvailable, Rows: rows}, nil
}

func (f *fleetTelemetryFixture) ServiceMetrics(context.Context, observability.MetricQuery) (observability.Result, error) {
	f.queries.Add(1)
	container := f.container
	if container == "" {
		container = "replica.1"
	}
	return observability.Result{Status: observability.StatusAvailable, Rows: []observability.Row{
		{"stack": "demo", "service": "demo_web", "container": container, "metric": observability.MetricContainerMemoryPercent, "avg": 12.0, "max": 20.0},
	}}, nil
}

func (f *fleetTelemetryFixture) Logs(_ context.Context, q observability.LogQuery) (observability.Result, error) {
	f.queries.Add(1)
	if f.logsErr {
		return observability.Result{}, errors.New("fixture logs unavailable")
	}
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, Rows: []observability.Row{
		{"p_timestamp": q.Window.End.UTC().Format(time.RFC3339Nano), "severity_text": "INFO", "service.name": "demo_web", "body": f.body},
	}}, nil
}

func (f *fleetTelemetryFixture) LogVolume(context.Context, observability.MetricQuery) (observability.Result, error) {
	f.queries.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Rows: []observability.Row{{"bucket": "2026-10-19T00:00:00", "level": "info", "n": 1}}}, nil
}

func (f *fleetTelemetryFixture) TraceStats(context.Context, observability.MetricQuery) (observability.Result, error) {
	f.queries.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Rows: []observability.Row{{"traces": 1, "error_traces": 0, "spans": 2}}}, nil
}

func fleetController(t *testing.T) (*Controller, *views.Renderer) {
	t.Helper()
	renderer, err := views.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{Views: renderer, Redactor: observability.NewRedactor("global-secret")}, renderer
}

func fleetDaemon(t *testing.T, hostname, token string, status int, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status != 0 && r.URL.Path == "/api/me" {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/me":
			_, _ = w.Write([]byte(`{"id":1,"name":"fleet"}`))
		case "/api/nodes":
			_, _ = fmt.Fprintf(w, `{"nodes":[{"hostname":%q,"role":"manager","status":"ready","availability":"active","is_leader":true}]}`, hostname)
		case "/api/services":
			_, _ = w.Write([]byte(`{"services":[{"name":"demo_web","stack":"demo","replicas":1,"desired":1}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
}

func newFleetForTest(t *testing.T, entries []FleetHostConfig) *Fleet {
	t.Helper()
	ctrl, _ := fleetController(t)
	fleet, err := NewFleet(ctrl, entries, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return fleet
}

func TestLoadFleetConfigBoundsAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.json")
	entry := FleetHostConfig{Name: "alpha", Hostname: "alpha", DaemonURL: "http://127.0.0.1:9090", DaemonToken: "daemon-secret", ParseableURL: "http://127.0.0.1:18080", ParseableAuth: "Basic c2VjcmV0"}
	b, _ := json.Marshal([]FleetHostConfig{entry})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFleetConfig(path)
	if err != nil || len(got) != 1 || got[0].Name != "alpha" {
		t.Fatalf("LoadFleetConfig = %+v, %v", got, err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFleetConfig(path); err == nil || strings.Contains(err.Error(), "daemon-secret") {
		t.Fatalf("insecure config error=%v", err)
	}
	if _, err := LoadFleetConfig(""); err != nil {
		t.Fatalf("empty config path: %v", err)
	}
	entry.DaemonURL = "http://192.0.2.1:9090"
	bad, _ := json.Marshal([]FleetHostConfig{entry})
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFleetConfig(path); err == nil || strings.Contains(err.Error(), "192.0.2.1") {
		t.Fatalf("non-loopback endpoint error=%v", err)
	}
	for _, suffix := range []string{"?debug=1", "#fragment"} {
		entry.DaemonURL = "http://127.0.0.1:9090" + suffix
		bad, _ = json.Marshal([]FleetHostConfig{entry})
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFleetConfig(path); err == nil {
			t.Fatalf("endpoint %q was accepted", suffix)
		}
	}
}

func TestFleetAuthAndInventoryPrecedeTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		meStatus   int
		hostname   string
		wantReason string
	}{
		{name: "unauthorized", meStatus: http.StatusUnauthorized, hostname: "alpha", wantReason: "authorization"},
		{name: "wrong-host", hostname: "other", wantReason: "hostname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := fleetDaemon(t, tc.hostname, "daemon-secret", tc.meStatus, nil)
			defer daemon.Close()
			fleet := newFleetForTest(t, []FleetHostConfig{{Name: "alpha", Hostname: "alpha", DaemonURL: daemon.URL, DaemonToken: "daemon-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic c2VjcmV0"}})
			fixture := &fleetTelemetryFixture{host: "alpha"}
			fleet.hosts[0].telemetry = fixture
			w, _ := observability.ParseWindow("15m", time.Now())
			view := fleet.collectHost(context.Background(), fleet.hosts[0], w, "")
			if view.StatusReason != tc.wantReason || fixture.queries.Load() != 0 {
				t.Fatalf("view=%+v telemetry queries=%d", view, fixture.queries.Load())
			}
		})
	}
}

func TestFleetKeepsHostsAndSecretsIsolated(t *testing.T) {
	var callsA, callsB atomic.Int32
	daemonA := fleetDaemon(t, "alpha", "alpha-secret", 0, &callsA)
	daemonB := fleetDaemon(t, "beta", "beta-secret", 0, &callsB)
	defer daemonA.Close()
	defer daemonB.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{
		{Name: "alpha", Hostname: "alpha", DaemonURL: daemonA.URL, DaemonToken: "alpha-secret", ParseableURL: daemonA.URL, ParseableAuth: "Basic YWxwaGE="},
		{Name: "beta", Hostname: "beta", DaemonURL: daemonB.URL, DaemonToken: "beta-secret", ParseableURL: daemonB.URL, ParseableAuth: "Basic YmV0YQ=="},
	})
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha", body: "alpha-secret beta-secret", container: "replica.1.beta-secret"}
	fleet.hosts[1].telemetry = &fleetTelemetryFixture{host: "beta", body: "beta-secret", container: "replica.1.beta-secret"}
	w, _ := observability.ParseWindow("15m", time.Now())
	views := []FleetHostView{fleet.collectHost(context.Background(), fleet.hosts[0], w, ""), fleet.collectHost(context.Background(), fleet.hosts[1], w, "")}
	if !views[0].Online || !views[1].Online || views[0].Name != "alpha" || views[1].Name != "beta" {
		t.Fatalf("host views=%+v", views)
	}
	if len(views[0].Logs) == 0 || len(views[1].Logs) == 0 || strings.Contains(views[0].Logs[0].Body, "alpha-secret") || strings.Contains(views[0].Logs[0].Body, "beta-secret") || strings.Contains(views[1].Logs[0].Body, "alpha-secret") || strings.Contains(views[1].Logs[0].Body, "beta-secret") {
		t.Fatalf("unredacted or missing logs: %+v %+v", views[0].Logs, views[1].Logs)
	}
	for _, hostView := range views {
		for _, metric := range hostView.Services {
			if strings.Contains(metric.Label, "alpha-secret") || strings.Contains(metric.Label, "beta-secret") {
				t.Fatalf("unredacted metric label: %+v", metric)
			}
		}
	}
	if views[0].Logs[0].Host == views[1].Logs[0].Host || views[0].Services[0].Label != views[1].Services[0].Label {
		t.Fatalf("host identity was not kept separate: %+v %+v", views[0].Logs[0], views[1].Logs[0])
	}
	if callsA.Load() == 0 || callsB.Load() == 0 {
		t.Fatal("one daemon was not queried")
	}
}

func TestFleetShowsRecentLogCoverageFailure(t *testing.T) {
	daemon := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	defer daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{{Name: "alpha", Hostname: "alpha", DaemonURL: daemon.URL, DaemonToken: "alpha-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic YWxwaGE="}})
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha", logsErr: true}

	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/web/fleet?range=15m", nil)
	fleet.Page(ctx)
	if !strings.Contains(rr.Body.String(), "Recent logs unavailable") || strings.Contains(rr.Body.String(), "No recent records") {
		t.Fatalf("recent log failure was rendered as an empty successful result: %s", rr.Body.String())
	}

	w, _ := observability.ParseWindow("15m", time.Now())
	view := fleet.collectHost(context.Background(), fleet.hosts[0], w, "")
	if view.RecentLogsKnown || !view.LogRecordsKnown {
		t.Fatalf("volume and recent-log coverage were conflated: %+v", view)
	}
	d := FleetData{Hosts: []FleetHostView{view}}
	fleet.aggregate(&d)
	if d.RecentLogsKnown || !d.RecentLogsPartial || !d.Partial {
		t.Fatalf("aggregate did not retain recent-log coverage: %+v", d)
	}
}

func TestFleetOfflineHostDoesNotHideHealthyHost(t *testing.T) {
	daemon := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	deadURL := daemon.URL
	daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{
		{Name: "alpha", Hostname: "alpha", DaemonURL: deadURL, DaemonToken: "alpha-secret", ParseableURL: deadURL, ParseableAuth: "Basic YWxwaGE="},
		{Name: "beta", Hostname: "beta", DaemonURL: "http://127.0.0.1:9", DaemonToken: "beta-secret", ParseableURL: "http://127.0.0.1:9", ParseableAuth: "Basic YmV0YQ=="},
	})
	// Swap the dead alpha endpoint to a live daemon after constructing the
	// clients, leaving beta as the intentionally unreachable host.
	healthy := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	defer healthy.Close()
	fleet.hosts[0].api = pmapi.NewWithHTTPClient(healthy.URL, "alpha-secret", time.Second, fleetHTTPClient(time.Second))
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha", body: "alpha-secret"}
	fleet.hosts[1].telemetry = &fleetTelemetryFixture{host: "beta", body: "beta-secret"}
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/web/fleet?range=15m", nil)
	d := fleet.load(ctx)
	if d.Online != 1 || len(d.Hosts) != 2 {
		t.Fatalf("offline host hid healthy host: online=%d hosts=%+v", d.Online, d.Hosts)
	}
	if d.Hosts[0].Status != "online" || d.Hosts[1].Status != "offline" {
		t.Fatalf("unexpected host statuses: %+v", d.Hosts)
	}
}

func TestFleetPageDoesNotRenderCredentials(t *testing.T) {
	daemon := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	defer daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{{Name: "alpha", Hostname: "alpha", DaemonURL: daemon.URL, DaemonToken: "alpha-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic YWxwaGE="}})
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha", body: "alpha-secret Basic YWxwaGE="}
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/web/fleet?range=15m", nil)
	fleet.Page(ctx)
	body := rr.Body.String()
	if strings.Contains(body, "alpha-secret") || strings.Contains(body, "Basic YWxwaGE=") || strings.Contains(body, daemon.URL) {
		t.Fatalf("fleet page rendered a secret or endpoint: %s", body)
	}
}

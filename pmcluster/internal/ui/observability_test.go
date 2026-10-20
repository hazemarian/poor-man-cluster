package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/investigation"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/server"
	dbstore "github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
)

type evidenceFixture struct{ calls atomic.Int32 }

func (f *evidenceFixture) Query(_ context.Context, q observability.Query) (observability.Result, error) {
	f.calls.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Rows: []observability.Row{{"service.name": "demo_web", "body": `<script>alert(1)</script> password="test-secret" bare-credential`, "severity_text": "ERROR"}}}, nil
}

func TestNativeObservabilityScopeAndRendering(t *testing.T) {
	for _, tc := range []struct {
		name          string
		me, inventory int
		service       string
		wantCalls     int32
	}{
		{"available", 200, 200, "demo/web", 1}, {"scoped", 403, 200, "", 0},
		{"inventory-failed", 200, 503, "", 0}, {"unknown-service", 200, 200, "other/web", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/me":
					w.WriteHeader(tc.me)
					fmt.Fprint(w, `{"id":1,"name":"edge"}`)
				case "/api/services":
					w.WriteHeader(tc.inventory)
					fmt.Fprint(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":1,"desired":1}]}`)
				case "/api/nodes":
					fmt.Fprint(w, `{"nodes":[]}`)
				case "/api/backups":
					fmt.Fprint(w, `{"backups":[]}`)
				case "/api/tls/hosts":
					fmt.Fprint(w, `{"hosts":[]}`)
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer daemon.Close()
			app, err := NewApp(Config{DataDir: ":memory:", PMAPIURL: daemon.URL, PMAPIToken: "bare-credential", LoginDisabled: true, UpstreamTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Store.Close()
			source := &evidenceFixture{}
			app.ctrl.Telemetry = source
			req := httptest.NewRequest("GET", "http://x/web/observability?tab=logs&service="+tc.service, nil)
			w := httptest.NewRecorder()
			app.Handler().ServeHTTP(w, req)
			body := w.Body.String()
			if source.calls.Load() != tc.wantCalls {
				t.Fatalf("unauthorized source calls: %d", source.calls.Load())
			}
			if strings.Contains(body, "test-secret") || strings.Contains(body, "bare-credential") || strings.Contains(body, "<script>alert(1)</script>") {
				t.Fatal("unsafe evidence rendered")
			}
			if !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "Observability") {
				t.Fatalf("native shell missing: status%d", w.Code)
			}
		})
	}
}

// nativeClusterFixture models the normal full-cluster composition: one
// daemon inventory contains every Swarm node and one Parseable source carries
// the same node identities across all signals. There is no observer mode or
// host federation in this path.
type nativeClusterFixture struct {
	logs, nodes, services, traces atomic.Int32
}

func (f *nativeClusterFixture) Query(_ context.Context, q observability.Query) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now()}, nil
}

func (f *nativeClusterFixture) Logs(_ context.Context, q observability.LogQuery) (observability.Result, error) {
	f.logs.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Rows: []observability.Row{
		{"service.name": "demo_web", "body": "native cluster log", "severity_text": "ERROR", "p_timestamp": q.Window.End.UTC().Format(time.RFC3339)},
	}}, nil
}

func (f *nativeClusterFixture) NodeMetrics(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	f.nodes.Add(1)
	bucket := q.Window.End.Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	rows := make([]observability.Row, 0, 9)
	for i, host := range []string{"node-1", "node-2", "node-3"} {
		rows = append(rows,
			observability.Row{"bucket": bucket, "host": host, "metric": observability.MetricNodeCPUUtilization, "state": "idle", "value": 0.25 + float64(i)/10},
			observability.Row{"bucket": bucket, "host": host, "metric": observability.MetricNodeMemUtilization, "state": "used", "value": 0.45 + float64(i)/10},
			observability.Row{"bucket": bucket, "host": host, "metric": observability.MetricNodeLoad1m, "state": "", "value": 0.5 + float64(i)/10},
		)
	}
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Rows: rows}, nil
}

func (f *nativeClusterFixture) ServiceMetrics(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	f.services.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Rows: []observability.Row{
		{"stack": "demo", "service": "demo_web", "container": "demo_web.1", "metric": observability.MetricContainerCPU, "avg": 12.0, "max": 18.0},
		{"stack": "demo", "service": "demo_web", "container": "demo_web.1", "metric": observability.MetricContainerMemoryPercent, "avg": 40.0, "max": 55.0},
	}}, nil
}

func (f *nativeClusterFixture) Traces(_ context.Context, q observability.TraceQuery) (observability.Result, error) {
	f.traces.Add(1)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, ObservedAt: time.Now(), Rows: []observability.Row{
		{"trace_id": "0123456789abcdef0123456789abcdef", "started": q.Window.End.UTC().Format(time.RFC3339), "duration_ns": 50000000.0, "spans": 2.0, "errors": 0.0, "root": "GET /health", "service": "demo_web"},
	}}, nil
}

func (f *nativeClusterFixture) Trace(context.Context, observability.Window, string) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}

func (f *nativeClusterFixture) LogVolume(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	bucket := q.Window.End.Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, Rows: []observability.Row{
		{"bucket": bucket, "level": "info", "n": 10.0}, {"bucket": bucket, "level": "error", "n": 1.0},
	}}, nil
}

func (f *nativeClusterFixture) TraceStats(_ context.Context, q observability.MetricQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, Rows: []observability.Row{{"traces": 1.0, "error_traces": 0.0, "spans": 2.0}}}, nil
}

func TestNativeObservabilityUsesFullClusterComposition(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			fmt.Fprint(w, `{"id":1,"name":"edge"}`)
		case "/api/services":
			fmt.Fprint(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":3,"desired":3},{"name":"demo_worker","stack":"demo","replicas":3,"desired":3}]}`)
		case "/api/nodes":
			fmt.Fprint(w, `{"nodes":[{"hostname":"node-1","role":"manager","status":"ready","availability":"active","is_leader":true},{"hostname":"node-2","role":"manager","status":"ready","availability":"active"},{"hostname":"node-3","role":"worker","status":"ready","availability":"active"}]}`)
		case "/api/backups":
			fmt.Fprint(w, `{"backups":[]}`)
		case "/api/tls/hosts":
			fmt.Fprint(w, `{"hosts":[]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer daemon.Close()

	app, err := NewApp(Config{DataDir: ":memory:", PMAPIURL: daemon.URL, PMAPIToken: "fixture-token", LoginDisabled: true, UpstreamTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	fixture := &nativeClusterFixture{}
	app.ctrl.Telemetry = fixture

	overviewCode, overview := get(app, "/web/observability")
	if overviewCode != http.StatusOK {
		t.Fatalf("overview status=%d", overviewCode)
	}
	if got := strings.Count(overview, `<article class="obs-node">`); got != 3 {
		t.Fatalf("overview node cards=%d, want 3", got)
	}
	assertNodeCard := func(page, host string, cpu, memory, load float64) {
		t.Helper()
		var card string
		for _, section := range strings.Split(page, `<article class="obs-node">`)[1:] {
			if strings.Contains(section, ">"+host+"</h3>") {
				card = section
				if end := strings.Index(card, "</article>"); end >= 0 {
					card = card[:end]
				}
				break
			}
		}
		if card == "" {
			t.Fatalf("missing host-scoped card for %q", host)
		}
		for _, want := range []string{
			"<dt>CPU</dt>", fmt.Sprintf("%.1f%%", cpu),
			"<dt>Memory</dt>", fmt.Sprintf("%.1f%%", memory),
			"<dt>Load (1 min)</dt>", fmt.Sprintf("%.1f", load),
		} {
			if !strings.Contains(card, want) {
				t.Errorf("host %q card missing %q: %s", host, want, card)
			}
		}
	}
	for i, host := range []string{"node-1", "node-2", "node-3"} {
		assertNodeCard(overview, host, 75-float64(i)*10, 45+float64(i)*10, 0.5+float64(i)/10)
	}
	for _, want := range []string{`href="/web/overview"`, `href="/web/stacks"`, "node-1", "node-2", "node-3"} {
		if !strings.Contains(overview, want) {
			t.Errorf("full console missing %q", want)
		}
	}

	metricsCode, metrics := get(app, "/web/observability?tab=metrics&range=1h")
	if metricsCode != http.StatusOK {
		t.Fatalf("metrics status=%d", metricsCode)
	}
	if got := strings.Count(metrics, `<article class="obs-node">`); got != 3 {
		t.Fatalf("metrics node cards=%d, want 3", got)
	}
	for i, host := range []string{"node-1", "node-2", "node-3"} {
		assertNodeCard(metrics, host, 75-float64(i)*10, 45+float64(i)*10, 0.5+float64(i)/10)
	}
	if !strings.Contains(metrics, "demo/web") {
		t.Fatal("metrics page missing native service aggregate")
	}

	logsCode, logs := get(app, "/web/observability?tab=logs&range=1h")
	if logsCode != http.StatusOK || !strings.Contains(logs, "native cluster log") {
		t.Fatalf("logs status=%d body=%s", logsCode, logs)
	}
	tracesCode, traces := get(app, "/web/observability?tab=traces&range=1h")
	if tracesCode != http.StatusOK || !strings.Contains(traces, "0123456789abcdef") {
		t.Fatalf("traces status=%d body=%s", tracesCode, traces)
	}
	if fixture.logs.Load() == 0 || fixture.nodes.Load() < 2 || fixture.services.Load() == 0 || fixture.traces.Load() == 0 {
		t.Fatalf("native source calls logs=%d nodes=%d services=%d traces=%d", fixture.logs.Load(), fixture.nodes.Load(), fixture.services.Load(), fixture.traces.Load())
	}
}

func TestNativeObservabilityRequiresLogin(t *testing.T) {
	app, err := NewApp(Config{DataDir: ":memory:", CookieName: "pmui_session", EnvUser: "admin", EnvPass: "fixture-password", SessionSecret: []byte("0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Store.Close()
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://x/web/observability", nil))
	_, _ = io.Copy(io.Discard, w.Result().Body)
	if w.Code < 300 || w.Code >= 400 {
		t.Fatalf("unauthenticated status=%d", w.Code)
	}
}

type investigationAdminLookup struct{}

func (investigationAdminLookup) UserByToken(context.Context, string) (*auth.User, error) {
	return &auth.User{ID: 1, Name: "edge", Role: auth.RoleAdmin}, nil
}

func TestNativeInvestigationEndToEndWithFixtures(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("provider request")
		}
		var in investigation.Input
		if len(request.Messages) != 2 || json.Unmarshal([]byte(request.Messages[1].Content), &in) != nil || len(in.Evidence) == 0 {
			t.Error("invalid evidence input")
			w.WriteHeader(400)
			return
		}
		if strings.Contains(request.Messages[1].Content, "test-secret") || strings.Contains(request.Messages[1].Content, "bare-credential") {
			t.Error("secret reached provider")
		}
		verdict := investigation.Verdict{Summary: "<script>alert(1)</script> Observed failures", Evidence: []string{in.Evidence[0].ID}}
		content, _ := json.Marshal(verdict)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}, "finish_reason": "stop"}}, "usage": map[string]int{"total_tokens": 64}})
	}))
	defer provider.Close()
	st, err := dbstore.Open(filepath.Join(t.TempDir(), "daemon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local := &investigation.Local{Store: st}
	p := investigation.DefaultPolicy()
	p.Enabled = true
	p.Sharing = true
	p.Model = "fixture"
	p.BaseURL = provider.URL + "/v1"
	p.DailyTokens = 18000
	if err = local.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	accounting := server.New(server.Deps{Lookup: investigationAdminLookup{}, Investigation: local})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/investigation/") {
			accounting.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/me":
			fmt.Fprint(w, `{"id":1,"name":"edge"}`)
		case "/api/services":
			fmt.Fprint(w, `{"services":[{"name":"demo_web","stack":"demo","replicas":1,"desired":1}]}`)
		case "/api/nodes":
			fmt.Fprint(w, `{"nodes":[]}`)
		case "/api/backups":
			fmt.Fprint(w, `{"backups":[]}`)
		case "/api/tls/hosts":
			fmt.Fprint(w, `{"hosts":[]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer daemon.Close()
	app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true, PMAPIURL: daemon.URL, PMAPIToken: "bare-credential", InvestigationKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.ctrl.Telemetry = &evidenceFixture{}
	form := url.Values{"question": {"Why password=test-secret?"}, "service": {"demo/web"}, "range": {"1h"}}.Encode()
	req := httptest.NewRequest("POST", "http://x/web/observability/investigations", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://x")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, req)
	match := regexp.MustCompile(`/web/observability/investigations/([a-f0-9]{32})`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatalf("job not queued: %s", w.Body.String())
	}
	var result string
	for i := 0; i < 5; i++ {
		poll := httptest.NewRequest("GET", "http://x/web/observability/investigations/"+match[1], nil)
		poll.Header.Set("HX-Request", "true")
		out := httptest.NewRecorder()
		app.Handler().ServeHTTP(out, poll)
		result = out.Body.String()
		if strings.Contains(result, "Investigation complete") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(result, "Investigation complete") || strings.Contains(result, "<script>alert(1)</script>") || !strings.Contains(result, "&lt;script&gt;") {
		t.Fatalf("unsafe or incomplete result: %s", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
	b, _ := st.InvestigationBudget(context.Background())
	if b.Charged != 64 || b.Held != 0 {
		t.Fatalf("budget=%+v", b)
	}
}

func TestInvestigationOriginAndRoleGates(t *testing.T) {
	app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	for _, origin := range []string{"", "null", "http://evil.example", "https://x", "http://user@x"} {
		req := httptest.NewRequest("POST", "http://x/web/observability/investigations", strings.NewReader("question=why"))
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("origin %q status%d", origin, w.Code)
		}
	}
	local, err := NewApp(Config{DataDir: ":memory:", CookieName: "pmui_session", EnvUser: "admin", EnvPass: "fixture-password", SessionSecret: []byte("0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err = local.Store.CreateUser(context.Background(), "viewer", "hash", true, store.RoleViewer); err != nil {
		t.Fatal(err)
	}
	jar := map[string]*http.Cookie{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "http://x/", nil)
	local.Auth.SetCookie(ctx, "viewer", time.Hour)
	for _, cookie := range recorder.Result().Cookies() {
		jar[cookie.Name] = cookie
	}
	for _, path := range []string{"/web/observability/investigations", "/web/observability/settings"} {
		resp := doRequest(t, local, "POST", path, "question=why", jar)
		if resp.StatusCode != 403 {
			t.Fatalf("viewer path%s status%d", path, resp.StatusCode)
		}
	}
}

func TestObservabilityExpiredCertificateIsExplicit(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			expiry := time.Now().Add(time.Hour)
			if expired {
				expiry = time.Now().Add(-time.Hour)
			}
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/me":
					fmt.Fprint(w, `{"id":1}`)
				case "/api/services":
					fmt.Fprint(w, `{"services":[]}`)
				case "/api/nodes":
					fmt.Fprint(w, `{"nodes":[]}`)
				case "/api/backups":
					fmt.Fprint(w, `{"backups":[]}`)
				case "/api/tls/hosts":
					fmt.Fprint(w, `{"hosts":[]}`)
				case "/api/tls/site":
					_ = json.NewEncoder(w).Encode(map[string]string{"not_after": expiry.Format(time.RFC3339)})
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer daemon.Close()
			app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true, PMAPIURL: daemon.URL, PMAPIToken: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			w := httptest.NewRecorder()
			app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://x/web/observability", nil))
			if strings.Contains(w.Body.String(), ">Expired<") != expired {
				t.Fatalf("expired=%v status%d", expired, w.Code)
			}
		})
	}
}

// A console trace filter that the daemon grammar refuses must never reach the
// Parseable query, while a valid stack/service filter must.
func TestConsoleTraceServiceFilterIsValidatedBeforeTheQuery(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			fmt.Fprint(w, `{"id":1,"name":"edge"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	defer daemon.Close()

	app, err := NewApp(Config{DataDir: ":memory:", PMAPIURL: daemon.URL, PMAPIToken: "fixture-token", LoginDisabled: true, UpstreamTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	fixture := &nativeClusterFixture{}
	app.ctrl.Telemetry = fixture

	code, body := get(app, "/web/observability/traces?svc="+url.QueryEscape("web' OR '1'='1"))
	if code != http.StatusOK {
		t.Fatalf("invalid filter status=%d", code)
	}
	if n := fixture.traces.Load(); n != 0 {
		t.Fatalf("invalid service filter reached the trace query %d time(s)", n)
	}
	if strings.Contains(body, "GET /health") {
		t.Error("an invalid service filter must not render trace rows")
	}

	if code, _ := get(app, "/web/observability/traces?svc="+url.QueryEscape("demo/web")); code != http.StatusOK {
		t.Fatalf("valid filter status=%d", code)
	}
	if n := fixture.traces.Load(); n != 1 {
		t.Fatalf("valid service filter should query traces once, got %d", n)
	}
}

// The /internal/observability hop returns Parseable rows to the edge's daemon
// caller, so the edge token must be redacted there as well as on console pages.
func TestInternalObservabilityRedactsTheEdgeToken(t *testing.T) {
	const edgeToken = "edge-token-value-7731"
	parseableFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"p_timestamp":"2026-10-18T19:07:30.310","service.name":"demo_web","severity_text":"ERROR","body":"leaked `+edgeToken+` in a log line"}]`)
	}))
	defer parseableFake.Close()

	app, err := NewApp(Config{DataDir: ":memory:", PMAPIURL: "http://127.0.0.1:1", PMAPIToken: edgeToken, ParseableURL: parseableFake.URL, LoginDisabled: true, UpstreamTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	req := httptest.NewRequest("GET", "/internal/observability/logs", nil)
	req.Header.Set("Authorization", "Bearer "+edgeToken)
	w := httptest.NewRecorder()
	app.InternalObservabilityHandler(edgeToken).ServeHTTP(w, req)

	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "leaked") {
		t.Fatalf("status=%d, want the fixture row back; body=%s", w.Code, body)
	}
	if strings.Contains(body, edgeToken) {
		t.Fatalf("edge token reached the internal response: %s", body)
	}
}

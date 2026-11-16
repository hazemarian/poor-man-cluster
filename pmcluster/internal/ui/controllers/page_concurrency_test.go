package controllers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/middleware"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/store"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/views"
)

// gate counts how many reads are inside a source at once, and can hold each of
// them for a fixed time. A page that still issues its reads one after another
// reports a peak of 1, which is what these tests refuse.
type gate struct {
	mu       sync.Mutex
	inflight int
	peak     int
	// waves counts how many times a read STARTED while nothing else was in
	// flight. It is the shape a wall-clock budget was trying to measure: five
	// reads issued together are ONE wave; five issued one after another are five.
	// Time was the wrong instrument — on a loaded CI runner a single
	// descheduling event inside one correct wave reads as a waterfall (two 80ms
	// reads took 168ms), and a budget that survives that is too loose to catch
	// anything.
	waves int
	delay time.Duration
	calls int
	// shared counts every read held by ANY gate in the test, so "did the two
	// families overlap" is one number instead of a guess from two peaks.
	shared *gate
}

func (g *gate) enter() {
	g.mu.Lock()
	if g.inflight == 0 {
		g.waves++
	}
	g.inflight++
	g.calls++
	if g.inflight > g.peak {
		g.peak = g.inflight
	}
	g.mu.Unlock()
	if g.shared != nil {
		g.shared.mu.Lock()
		g.shared.inflight++
		if g.shared.inflight > g.shared.peak {
			g.shared.peak = g.shared.inflight
		}
		g.shared.mu.Unlock()
	}
	time.Sleep(g.delay)
}

func (g *gate) leave() {
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
	if g.shared != nil {
		g.shared.mu.Lock()
		g.shared.inflight--
		g.shared.mu.Unlock()
	}
}

func (g *gate) snapshot() (peak, calls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak, g.calls
}

// waveCount is how many separate bursts of reads the gate saw.
func (g *gate) waveCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waves
}

// slowTelemetry is every telemetry trait the observability page consumes, behind
// one gate, so the page's read graph is observable from the test.
type slowTelemetry struct{ g *gate }

func (s slowTelemetry) wrap() observability.Result {
	s.g.enter()
	defer s.g.leave()
	return observability.Result{Status: observability.StatusAvailable}
}

func (s slowTelemetry) Query(context.Context, observability.Query) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) Logs(context.Context, observability.LogQuery) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) LogVolume(context.Context, observability.MetricQuery) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) TraceStats(context.Context, observability.MetricQuery) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) NodeMetrics(context.Context, observability.MetricQuery) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) ServiceMetrics(context.Context, observability.MetricQuery) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) ServiceNames(context.Context, observability.Window) ([]string, error) {
	s.wrap()
	return nil, nil
}
func (s slowTelemetry) Traces(context.Context, observability.TraceQuery) (observability.Result, error) {
	return s.wrap(), nil
}
func (s slowTelemetry) Trace(context.Context, observability.Window, string) (observability.Result, error) {
	return s.wrap(), nil
}

// slowDaemon fronts the harness daemon so every API read the console makes is
// counted and held, without changing what the console reads.
func slowDaemon(t *testing.T, g *gate) string {
	t.Helper()
	inner := ctrlFakeDaemon(t)
	t.Cleanup(inner.Close)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.enter()
		defer g.leave()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.RequestURI(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body) //nolint:errcheck
	}))
	t.Cleanup(front.Close)
	return front.URL
}

// pageHarness wires a Controller to the slow daemon (and, when withTelemetry is
// set, to a slow telemetry source) and mounts the routes under test. Both halves
// are gated separately so a test can see which family of reads overlapped.
func pageHarness(t *testing.T, apiRate time.Duration, withTelemetry bool) (*gin.Engine, *gate, *gate) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	both := &gate{}
	apiGate := &gate{delay: apiRate, shared: both}
	telemetryGate := &gate{delay: apiRate, shared: both}
	var tmpl observability.Source
	if withTelemetry {
		tmpl = slowTelemetry{g: telemetryGate}
	}

	base := slowDaemon(t, apiGate)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	renderer, err := views.NewRenderer()
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	renderer.ShellData = views.ShellBase
	ctrl := &Controller{
		Store: st, Views: renderer,
		Auth:    middleware.NewAuth(st, []byte("0123456789abcdefgh"), "pmui_session"),
		API:     pmapi.New(base, "pmc_test", 10*time.Second),
		Version: "test", Domain: "example.test",
		EnvAPI: base, EnvToken: "pmc_test",
		Telemetry:     tmpl,
		Redactor:      observability.NewRedactor("pmc_test"),
		ObsCache:      NewObservabilityCache(),
		EvidenceCache: NewEvidenceCache(),
	}
	engine := gin.New()
	engine.GET("/web/overview", (Overview{Controller: ctrl}).Fragment)
	engine.GET("/web/stacks", (Stacks{Controller: ctrl}).List)
	obs := Observability{Controller: ctrl}
	engine.GET("/web/observability", obs.Page)
	engine.GET("/web/observability/summary", obs.Summary)
	engine.GET("/web/observability/evidence", obs.Evidence)
	engine.GET("/web/observability/traces", obs.Traces)
	return engine, apiGate, telemetryGate
}

func doGet(t *testing.T, engine *gin.Engine, path string) time.Duration {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://x"+path, nil)
	rr := httptest.NewRecorder()
	start := time.Now()
	engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, rr.Code)
	}
	return time.Since(start)
}

// TestOverviewFragmentIssuesItsReadsTogether: the landing fragment makes five
// independent daemon reads. Sequentially they cost five round trips; the peak
// concurrency proves they are issued as one wave, and the wall clock proves it.
func TestOverviewFragmentIssuesItsReadsTogether(t *testing.T) {
	const delay = 60 * time.Millisecond
	engine, apiGate, _ := pageHarness(t, delay, false)

	doGet(t, engine, "/web/overview")
	peak, calls := apiGate.snapshot()

	if calls < 4 {
		t.Fatalf("overview made %d daemon reads, want at least 4", calls)
	}
	if peak < 4 {
		t.Fatalf("peak concurrent daemon reads = %d, want >= 4 (the fragment is a waterfall)", peak)
	}
	// One burst, not four. This replaced an elapsed budget (3*delay): on a loaded
	// runner one correct wave can take longer than several sequential ones on an
	// idle one, so the budget measured the machine. The gate counts bursts.
	if waves := apiGate.waveCount(); waves != 1 {
		t.Fatalf("the %d reads arrived in %d waves, want 1: they are issued one after another", calls, waves)
	}
}

// TestStacksIndexReadsStacksAndServicesTogether: the index needs the stack list
// and the swarm service list, and neither depends on the other.
func TestStacksIndexReadsStacksAndServicesTogether(t *testing.T) {
	const delay = 80 * time.Millisecond
	engine, apiGate, _ := pageHarness(t, delay, false)

	doGet(t, engine, "/web/stacks")
	peak, calls := apiGate.snapshot()
	if peak < 2 {
		t.Fatalf("peak concurrent daemon reads = %d, want >= 2 (stacks and services ran in series)", peak)
	}
	// The wall-clock version of this assertion failed on a loaded CI runner with
	// "took 168ms for two 80ms reads: looks sequential" while both reads were in
	// flight — one descheduling event, read as a waterfall. Waves cannot lie.
	if waves := apiGate.waveCount(); waves != 1 {
		t.Fatalf("the %d reads arrived in %d waves, want 1: stacks and services are serialised", calls, waves)
	}
}

// TestObservabilityPageOverlapsHealthAndTabReads is the assertion that matters:
// the health block and the tab's telemetry were two waves, so a page cost the
// sum of both. A telemetry read must now be in flight at the same time as the
// daemon reads, and the whole page must finish inside roughly one wave.
func TestObservabilityPageOverlapsHealthAndTabReads(t *testing.T) {
	const delay = 60 * time.Millisecond
	engine, apiGate, telemetryGate := pageHarness(t, delay, true)

	doGet(t, engine, "/web/observability")
	_, apiCalls := apiGate.snapshot()
	_, telemetryCalls := telemetryGate.snapshot()

	if apiCalls < 5 || telemetryCalls < 3 {
		t.Fatalf("read graph too small to judge: %d api calls, %d telemetry calls", apiCalls, telemetryCalls)
	}
	// One wave means the health reads and the tab's telemetry reads are in
	// flight together: six daemon reads plus at least one telemetry read.
	if peak, _ := apiGate.shared.snapshot(); peak < 7 {
		t.Fatalf("peak simultaneous reads across both families = %d, want >= 7 (health and tab read in separate waves)", peak)
	}
	// The old two-wave shape was detected by a wall clock with an explicit render
	// budget, and it flaked on a loaded CI box (181ms against a 180ms budget with
	// the page correctly in one wave). Wave counts state the same thing without
	// measuring the runner: this page makes one burst of api reads and one burst of
	// telemetry reads, and both are in flight together (the shared peak above).
	if waves := telemetryGate.waveCount(); waves > 1 {
		t.Fatalf("the %d telemetry reads arrived in %d waves, want 1: the tab's reads are serialised", telemetryCalls, waves)
	}
	if waves := apiGate.waveCount(); waves > 2 {
		t.Fatalf("the %d api reads arrived in %d waves: the health block and the tab are in series", apiCalls, waves)
	}
}

// TestObservabilityMetricsTabReadsNodeAndContainerMetricsTogether: the metrics
// tab was two Parseable scans back to back.
func TestObservabilityMetricsTabReadsNodeAndContainerMetricsTogether(t *testing.T) {
	const delay = 80 * time.Millisecond
	engine, _, telemetryGate := pageHarness(t, delay, true)

	doGet(t, engine, "/web/observability?tab=metrics")
	peak, _ := telemetryGate.snapshot()
	if peak < 2 {
		t.Fatalf("peak concurrent metric reads = %d, want >= 2 (node then container metrics)", peak)
	}
}

// TestObservabilityLogsTabFetchesServiceNamesOnEveryTab is the regression guard
// for the wasted read: the distinct telemetry identities only feed the logs
// tab's service picker, so issuing them on the overview tab is a Parseable scan
// whose answer nothing renders.
func TestObservabilityServiceNamesOnlyForLogsTab(t *testing.T) {
	engine, _, telemetryGate := pageHarness(t, 10*time.Millisecond, true)

	doGet(t, engine, "/web/observability")
	_, overviewCalls := telemetryGate.snapshot()

	doGet(t, engine, "/web/observability?tab=logs")
	_, logsCalls := telemetryGate.snapshot()

	// The overview tab reads log volume, trace stats and the newest errors; the
	// logs tab additionally reads the service names and the evidence page.
	if logsCalls <= overviewCalls {
		t.Fatalf("logs tab made %d telemetry calls, overview made %d: expected the logs tab to add the service-name read", logsCalls, overviewCalls)
	}
	// The overview tab must not pay for the name list. Five telemetry reads on
	// the overview (volume, stats, errors) would be a regression.
	if overviewCalls > 4 {
		t.Fatalf("overview tab made %d telemetry calls, want <= 4", overviewCalls)
	}
}

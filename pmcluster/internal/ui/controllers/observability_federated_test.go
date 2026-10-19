package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/ui/pmapi"
)

type federatedTraceTelemetry struct {
	fleetTelemetryFixture
	secret string
}

func (f *federatedTraceTelemetry) Traces(_ context.Context, q observability.TraceQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable, Window: q.Window, Rows: []observability.Row{{
		"trace_id": "0123456789abcdef0123456789abcdef", "started": q.Window.End.UTC().Format(time.RFC3339Nano),
		"duration_ns": 1000, "spans": 1, "errors": 0,
		"root": f.secret + "-root", "service": f.secret + "-service",
	}}}, nil
}

func (f *federatedTraceTelemetry) Trace(_ context.Context, q observability.Window, _ string) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable, Window: q, Rows: []observability.Row{{
		"span_id": "span", "parent_id": "", "name": f.secret + "-name", "service": f.secret + "-service",
		"kind": f.secret + "-kind", "status": 1, "message": f.secret + "-message", "start_ns": 1, "duration_ns": 1000,
	}}}, nil
}

func federatedRequestContext(path string) *gin.Context {
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	return ctx
}

func TestFederatedMainObservabilityKeepsHostTelemetrySeparate(t *testing.T) {
	daemonA := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	daemonB := fleetDaemon(t, "beta", "beta-secret", 0, nil)
	defer daemonA.Close()
	defer daemonB.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{
		{Name: "alpha", Hostname: "alpha", DaemonURL: daemonA.URL, DaemonToken: "alpha-secret", ParseableURL: daemonA.URL, ParseableAuth: "Basic YWxwaGE="},
		{Name: "beta", Hostname: "beta", DaemonURL: daemonB.URL, DaemonToken: "beta-secret", ParseableURL: daemonB.URL, ParseableAuth: "Basic YmV0YQ=="},
	})
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha", container: "replica.alpha-secret"}
	fleet.hosts[1].telemetry = &fleetTelemetryFixture{host: "beta", container: "replica.beta-secret"}
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}

	d := obs.loadFederated(federatedRequestContext("/web/observability?tab=metrics&range=15m"), obsTabMetrics)
	if !d.FederatedAll || len(d.FederatedHosts) != 2 || d.FederatedOnline != 2 {
		t.Fatalf("all-host metrics view=%+v", d)
	}
	for _, host := range d.FederatedHosts {
		if host.Data.Node != host.Name || len(host.Data.Metrics.Services) != 1 {
			t.Fatalf("host identity/metrics lost: %+v", host)
		}
		label := host.Data.Metrics.Services[0].Label
		if strings.Contains(label, "alpha-secret") || strings.Contains(label, "beta-secret") {
			t.Fatalf("cross-host metric secret rendered: %q", label)
		}
	}
}

func TestFederatedIrrelevantServiceFilterKeepsInventoryCards(t *testing.T) {
	daemon := fleetDaemon(t, "crestline", "crestline-secret", 0, nil)
	defer daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{{Name: "crestline", Hostname: "crestline", DaemonURL: daemon.URL, DaemonToken: "crestline-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic Y3Jlc3RsaW5l"}})
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "crestline"}
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}
	missing := federatedServiceKey("missing/missing")

	for _, tab := range []string{obsTabOverview, obsTabMetrics} {
		t.Run(tab, func(t *testing.T) {
			d := obs.loadFederated(federatedRequestContext("/web/observability?tab="+tab+"&service="+missing+"&range=15m"), tab)
			if len(d.FederatedHosts) != 1 || !d.FederatedHosts[0].Online {
				t.Fatalf("host became unavailable for irrelevant service filter: %+v", d.FederatedHosts)
			}
			host := d.FederatedHosts[0].Data
			if host.ErrKey != "" || len(host.NodeCards) != 1 || host.Health.NodesKnown != true {
				t.Fatalf("inventory card was lost for %s: err=%q nodes=%d known=%t", tab, host.ErrKey, len(host.NodeCards), host.Health.NodesKnown)
			}
		})
	}
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/web/observability?tab=metrics&service="+missing+"&range=15m", nil)
	obs.Page(ctx)
	if strings.Contains(rr.Body.String(), "No nodes reported") {
		t.Fatalf("metrics render used the empty-inventory state for an irrelevant service filter: %s", rr.Body.String())
	}
}

func TestFederatedInvalidServiceFailsLogsBeforeTelemetry(t *testing.T) {
	daemon := fleetDaemon(t, "crestline", "crestline-secret", 0, nil)
	defer daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{{Name: "crestline", Hostname: "crestline", DaemonURL: daemon.URL, DaemonToken: "crestline-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic Y3Jlc3RsaW5l"}})
	fixture := &fleetTelemetryFixture{host: "crestline"}
	fleet.hosts[0].telemetry = fixture
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}

	d := obs.loadFederated(federatedRequestContext("/web/observability?tab=logs&service="+federatedServiceKey("missing/missing")+"&range=15m"), obsTabLogs)
	if len(d.FederatedHosts) != 1 || !d.FederatedHosts[0].Online {
		t.Fatalf("invalid service changed host availability: %+v", d.FederatedHosts)
	}
	host := d.FederatedHosts[0].Data
	if host.ErrKey != "observability.invalid_service" || !host.Evidence.Unavailable || fixture.queries.Load() != 0 {
		t.Fatalf("invalid log service was not failed closed: err=%q unavailable=%t telemetry_queries=%d", host.ErrKey, host.Evidence.Unavailable, fixture.queries.Load())
	}
}

func TestFederatedAuthAndHostnameGateTelemetry(t *testing.T) {
	daemon := fleetDaemon(t, "other", "daemon-secret", http.StatusUnauthorized, nil)
	defer daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{{Name: "alpha", Hostname: "alpha", DaemonURL: daemon.URL, DaemonToken: "daemon-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic YWxwaGE="}})
	fixture := &fleetTelemetryFixture{host: "alpha"}
	fleet.hosts[0].telemetry = fixture
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}

	d := obs.loadFederated(federatedRequestContext("/web/observability?tab=logs&range=15m"), obsTabLogs)
	if len(d.FederatedHosts) != 1 || d.FederatedHosts[0].Online || fixture.queries.Load() != 0 {
		t.Fatalf("unauthorized host queried telemetry: %+v queries=%d", d.FederatedHosts, fixture.queries.Load())
	}

	daemon.Close()
	daemon = fleetDaemon(t, "other", "daemon-secret", 0, nil)
	defer daemon.Close()
	fleet.hosts[0].api.SetBase(daemon.URL)
	d = obs.loadFederated(federatedRequestContext("/web/observability?tab=logs&range=15m"), obsTabLogs)
	if len(d.FederatedHosts) != 1 || d.FederatedHosts[0].StatusReason != "hostname" || fixture.queries.Load() != 0 {
		t.Fatalf("wrong-host gate queried telemetry: %+v queries=%d", d.FederatedHosts, fixture.queries.Load())
	}
}

func TestFederatedOfflineHostRetainsHealthyHost(t *testing.T) {
	healthy := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	deadURL := healthy.URL
	healthy.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{
		{Name: "alpha", Hostname: "alpha", DaemonURL: deadURL, DaemonToken: "alpha-secret", ParseableURL: deadURL, ParseableAuth: "Basic YWxwaGE="},
		{Name: "beta", Hostname: "beta", DaemonURL: deadURL, DaemonToken: "beta-secret", ParseableURL: deadURL, ParseableAuth: "Basic YmV0YQ=="},
	})
	fleet.hosts[1].config.DaemonURL = deadURL
	// Replace beta's endpoint with an independent healthy daemon after the
	// immutable clients were constructed, mirroring a host that recovers while
	// the other endpoint remains offline.
	working := fleetDaemon(t, "beta", "beta-secret", 0, nil)
	defer working.Close()
	fleet.hosts[1].api.SetBase(working.URL)
	fleet.hosts[1].telemetry = &fleetTelemetryFixture{host: "beta"}
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha"}
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}

	d := obs.loadFederated(federatedRequestContext("/web/observability?tab=metrics&range=15m"), obsTabMetrics)
	if d.FederatedOnline != 1 || !d.FederatedPartial || len(d.FederatedHosts) != 2 {
		t.Fatalf("offline host hid healthy host: %+v", d)
	}
}

func TestFederatedTraceDetailRedactsPeerSecret(t *testing.T) {
	daemon := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	defer daemon.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{{Name: "alpha", Hostname: "alpha", DaemonURL: daemon.URL, DaemonToken: "alpha-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic YWxwaGE="}, {Name: "beta", Hostname: "beta", DaemonURL: daemon.URL, DaemonToken: "beta-secret", ParseableURL: daemon.URL, ParseableAuth: "Basic YmV0YQ=="}})
	fleet.hosts[0].telemetry = &federatedTraceTelemetry{fleetTelemetryFixture: fleetTelemetryFixture{host: "alpha"}, secret: "beta-secret"}
	fleet.hosts[1].telemetry = &federatedTraceTelemetry{fleetTelemetryFixture: fleetTelemetryFixture{host: "beta"}, secret: "beta-secret"}
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}

	d := obs.loadFederated(federatedRequestContext("/web/observability?tab=traces&node=alpha&trace=0123456789abcdef0123456789abcdef&range=15m"), obsTabTraces)
	if d.FederatedAll || len(d.Traces.Spans) != 1 {
		t.Fatalf("selected trace detail missing: %+v", d.Traces)
	}
	for _, span := range d.Traces.Spans {
		if strings.Contains(span.Name, "beta-secret") || strings.Contains(span.Service, "beta-secret") || strings.Contains(span.Message, "beta-secret") {
			t.Fatalf("peer secret escaped trace detail: %+v", span)
		}
	}
}

func TestFederatedLogsNamespaceHostFragments(t *testing.T) {
	daemonA := fleetDaemon(t, "alpha", "alpha-secret", 0, nil)
	daemonB := fleetDaemon(t, "beta", "beta-secret", 0, nil)
	defer daemonA.Close()
	defer daemonB.Close()
	fleet := newFleetForTest(t, []FleetHostConfig{
		{Name: "alpha", Hostname: "alpha", DaemonURL: daemonA.URL, DaemonToken: "alpha-secret", ParseableURL: daemonA.URL, ParseableAuth: "Basic YWxwaGE="},
		{Name: "beta", Hostname: "beta", DaemonURL: daemonB.URL, DaemonToken: "beta-secret", ParseableURL: daemonB.URL, ParseableAuth: "Basic YmV0YQ=="},
	})
	fleet.hosts[0].telemetry = &fleetTelemetryFixture{host: "alpha"}
	fleet.hosts[1].telemetry = &fleetTelemetryFixture{host: "beta"}
	obs := Observability{Controller: fleet.Controller, Fleet: fleet}
	rr := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rr)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/web/observability?tab=logs&range=15m", nil)
	obs.Page(ctx)
	body := rr.Body.String()
	if !strings.Contains(body, `id="obs-logs-`+safePanelPart("alpha")+`"`) || !strings.Contains(body, `id="obs-logs-`+safePanelPart("beta")+`"`) {
		t.Fatalf("host log fragments were not namespaced: %s", body)
	}
	if strings.Contains(body, `id="obs-logs"`) {
		t.Fatalf("unscoped log panel remains in federated output")
	}
	if strings.Contains(body, daemonA.URL) || strings.Contains(body, daemonB.URL) || strings.Contains(body, "alpha-secret") || strings.Contains(body, "beta-secret") {
		t.Fatalf("federated page rendered endpoint or credential material")
	}
}

func TestFederatedPanelPartsAndServiceSelectorsStayDistinct(t *testing.T) {
	if safePanelPart("a.b") == safePanelPart("a-b") {
		t.Fatal("normalized host names must not share an HTMX panel id")
	}
	services := []pmapi.Service{{Name: "prod_peer-secret", Stack: "stack"}}
	options := federatedServiceOptions(services)
	if len(options) != 1 || strings.Contains(options[0].Value, "prod_peer-secret") || strings.Contains(options[0].Value, "secret") {
		t.Fatalf("service selector exposed inventory text: %+v", options)
	}
	resolved, err := resolveFederatedService(options[0].Value, services)
	if err != nil || resolved != services[0].Name {
		t.Fatalf("opaque service selector did not resolve: %q, %v", resolved, err)
	}
}

func TestFederatedRequestBoundsServicePage(t *testing.T) {
	request, data, _ := parseFederatedRequest(federatedRequestContext("/web/observability?tab=overview&page=7"), obsTabOverview)
	if request.page != 7 || data.StackPage.Page != 7 {
		t.Fatalf("federated page was not preserved: request=%+v data=%+v", request, data.StackPage)
	}
}

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/auth"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

type observabilityReaderStub struct{}

func (observabilityReaderStub) Status(context.Context, observability.Window) (observability.StatusResult, error) {
	return observability.StatusResult{Status: observability.StatusEmpty, InventoryStatus: observability.StatusAvailable, TelemetryStatus: observability.StatusEmpty}, nil
}
func (observabilityReaderStub) Logs(context.Context, observability.LogQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) NodeMetrics(context.Context, observability.MetricQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) ServiceMetrics(context.Context, observability.MetricQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) Traces(context.Context, observability.TraceQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) Trace(context.Context, observability.Window, string) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) LogVolume(context.Context, observability.MetricQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) TraceStats(context.Context, observability.MetricQuery) (observability.Result, error) {
	return observability.Result{Status: observability.StatusAvailable}, nil
}
func (observabilityReaderStub) ServiceNames(context.Context, observability.Window) ([]string, error) {
	return []string{"demo_web"}, nil
}

// observabilityPaths is every documented read. The authz table walks all of
// them, not just /logs: a new route mounted outside the viewer gate would
// otherwise be invisible to the test that exists to prevent exactly that.
var observabilityPaths = []string{
	"/api/observability/status?range=15m",
	"/api/observability/logs?range=15m",
	"/api/observability/metrics/nodes?range=15m",
	"/api/observability/metrics/services?range=15m",
	"/api/observability/volume?range=15m",
	"/api/observability/traces/stats?range=15m",
	"/api/observability/services?range=15m",
	"/api/observability/traces?range=15m",
	"/api/observability/traces/0123456789abcdef0123456789abcdef?range=15m",
}

func TestObservabilityAPIRequiresViewerAndUnscopedToken(t *testing.T) {
	lookup := &fakeLookup{users: map[string]*auth.User{
		"viewer-token":   {ID: 1, Name: "viewer", Role: auth.RoleViewer},
		"scoped-token":   {ID: 2, Name: "scoped", Role: auth.RoleViewer, Stack: "demo"},
		"operator-token": {ID: 3, Name: "operator", Role: auth.RoleOperator},
	}}
	srv := httptest.NewServer(New(Deps{Lookup: lookup, Observability: observabilityReaderStub{}}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing bearer", status: http.StatusUnauthorized},
		{name: "viewer", token: "viewer-token", status: http.StatusOK},
		{name: "operator", token: "operator-token", status: http.StatusOK},
		// Cluster telemetry is not a tenant surface: a token confined to one
		// stack must not be able to read every service's logs and metrics.
		{name: "stack scoped viewer", token: "scoped-token", status: http.StatusForbidden},
	} {
		for _, path := range observabilityPaths {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != tc.status {
					t.Fatalf("%s as %q = %d, want %d", path, tc.name, resp.StatusCode, tc.status)
				}
			})
		}
	}
}

func TestObservabilityAPIRejectsInvalidRangeBeforeReader(t *testing.T) {
	srv := httptest.NewServer(New(Deps{Lookup: &fakeLookup{users: map[string]*auth.User{
		"viewer-token": {ID: 1, Name: "viewer", Role: auth.RoleViewer},
	}}, Observability: observabilityReaderStub{}}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/observability/logs?range=48h", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer viewer-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

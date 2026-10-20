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
		{name: "stack scoped viewer", token: "scoped-token", status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/observability/logs?range=15m", nil)
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
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
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

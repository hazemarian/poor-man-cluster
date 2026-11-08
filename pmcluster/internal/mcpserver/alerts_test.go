package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	pmcdocs "github.com/hazemarian/poor-man-cluster/pmcluster/docs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

// alertsReader answers the alerts read and records the bound it was asked for.
// Every other Reader method is deliberately absent: the embedded nil interface
// panics if the handler reaches one, so a tool that strayed onto another route
// fails loudly instead of silently passing.
type alertsReader struct {
	observability.Reader
	calls     int32
	lastLimit int32
}

func (r *alertsReader) Alerts(_ context.Context, q observability.AlertQuery) (observability.Result, error) {
	atomic.AddInt32(&r.calls, 1)
	atomic.StoreInt32(&r.lastLimit, int32(q.Limit))
	return observability.Result{
		Status: observability.StatusAvailable,
		Rows:   []observability.Row{{"name": "disk-full", "state": "firing"}},
	}, nil
}

// daemonRouter is the real daemon observability handler, mounted at the path
// the spec documents (/api/observability/...), so a tool call exercises the
// actual bound enforcement rather than a stub.
func daemonRouter(r observability.Reader) http.Handler {
	router := chi.NewRouter()
	router.Route("/api", func(api chi.Router) {
		(&observability.HTTP{Svc: r}).Mount(api)
	})
	return router
}

// TestAlertsToolIsDerivedWithItsBound: the tools the server actually serves
// (not the catalog builder in isolation) must include
// pmc_observability_alerts, read-only, with limit capped at 500 in the schema
// an agent sees.
func TestAlertsToolIsDerivedWithItsBound(t *testing.T) {
	cs := connect(t, daemonRouter(&alertsReader{}), Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")

	var found bool
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("tools: %v", err)
		}
		if tool.Name != "pmc_observability_alerts" {
			continue
		}
		found = true

		var schema struct {
			Properties map[string]struct {
				Type    string   `json:"type"`
				Maximum *float64 `json:"maximum"`
				Minimum *float64 `json:"minimum"`
			} `json:"properties"`
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("re-encode input schema: %v", err)
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("parse input schema: %v", err)
		}
		limit, ok := schema.Properties["limit"]
		if !ok {
			t.Fatalf("alerts tool has no limit parameter: %s", raw)
		}
		if limit.Maximum == nil || *limit.Maximum != observability.MaxAlertPageSize {
			t.Errorf("limit maximum = %v, want %d", limit.Maximum, observability.MaxAlertPageSize)
		}
		if limit.Minimum == nil || *limit.Minimum != 1 {
			t.Errorf("limit minimum = %v, want 1", limit.Minimum)
		}
		if !strings.Contains(tool.Description, "alert") {
			t.Errorf("description does not say what the tool returns: %q", tool.Description)
		}
	}
	if !found {
		t.Fatal("pmc_observability_alerts is not served — the route is documented but no tool was derived from it")
	}
}

// TestAlertsToolRefusesOverCapLimit: the daemon refuses limit>500 (it does not
// silently clamp), so the tool must relay that refusal as an error rather than
// turning it into a success — and the source must not be touched.
func TestAlertsToolRefusesOverCapLimit(t *testing.T) {
	src := &alertsReader{}
	cs := connect(t, daemonRouter(src), Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")

	for _, limit := range []any{5000, float64(observability.MaxAlertPageSize + 1), 0, -3} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "pmc_observability_alerts",
			Arguments: map[string]any{"limit": limit},
		})
		if err != nil {
			t.Fatalf("CallTool(limit=%v): %v", limit, err)
		}
		if !res.IsError {
			t.Fatalf("limit %v was accepted: %s", limit, firstText(t, res))
		}
		text := firstText(t, res)
		if !strings.Contains(text, "HTTP 400") {
			t.Errorf("limit %v refusal = %q, want the daemon's 400 relayed", limit, text)
		}
		if !strings.Contains(text, "500") {
			t.Errorf("limit %v refusal = %q, want it to name the 500 cap", limit, text)
		}
	}
	if got := atomic.LoadInt32(&src.calls); got != 0 {
		t.Fatalf("source calls = %d after refused limits, want 0", got)
	}
}

// TestAlertsToolCallsTheBoundedRoute: a valid call reaches the daemon route
// with the caller's limit, and the daemon's documented envelope is what the
// agent receives.
func TestAlertsToolCallsTheBoundedRoute(t *testing.T) {
	for _, tc := range []struct {
		args      map[string]any
		wantLimit int32
	}{
		{args: map[string]any{"limit": 25}, wantLimit: 25},
		{args: map[string]any{"limit": observability.MaxAlertPageSize}, wantLimit: observability.MaxAlertPageSize},
		{args: map[string]any{}, wantLimit: observability.MaxAlertPageSize},
	} {
		src := &alertsReader{}
		cs := connect(t, daemonRouter(src), Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "pmc_observability_alerts",
			Arguments: tc.args,
		})
		if err != nil {
			t.Fatalf("CallTool(%v): %v", tc.args, err)
		}
		if res.IsError {
			t.Fatalf("CallTool(%v) failed: %s", tc.args, firstText(t, res))
		}
		if got := atomic.LoadInt32(&src.lastLimit); got != tc.wantLimit {
			t.Errorf("args %v reached the source with limit %d, want %d", tc.args, got, tc.wantLimit)
		}
		var body struct {
			Status string           `json:"status"`
			Alerts []map[string]any `json:"alerts"`
		}
		if err := json.Unmarshal([]byte(firstText(t, res)), &body); err != nil {
			t.Fatalf("tool result is not the documented JSON envelope: %v (%s)", err, firstText(t, res))
		}
		if body.Status != string(observability.StatusAvailable) {
			t.Errorf("status = %q, want available", body.Status)
		}
		if len(body.Alerts) != 1 || body.Alerts[0]["name"] != "disk-full" {
			t.Errorf("alerts = %v, want the one upstream alert", body.Alerts)
		}
	}
}

// TestAlertsToolRelaysAnUnavailableBackend: with no telemetry source wired the
// tool must answer the documented unavailable envelope (not an error, not an
// empty success), so an agent can tell "no alerts" from "cannot read alerts".
func TestAlertsToolRelaysAnUnavailableBackend(t *testing.T) {
	cs := connect(t, daemonRouter(nil), Options{Spec: pmcdocs.OpenAPI, AllowOps: map[string]bool{}}, "Bearer t")

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "pmc_observability_alerts",
		Arguments: map[string]any{"limit": 10},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("an unavailable backend is a documented answer, not a tool error: %s", firstText(t, res))
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(firstText(t, res)), &body); err != nil {
		t.Fatalf("tool result is not JSON: %v (%s)", err, firstText(t, res))
	}
	if body.Status != string(observability.StatusUnavailable) || body.Error == "" {
		t.Fatalf("result = %s, want status=unavailable with an error", firstText(t, res))
	}
}

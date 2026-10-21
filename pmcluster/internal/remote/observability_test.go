package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/observability"
)

func TestRemoteObservabilityBearerAndDTO(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.URL.Path != "/api/observability/logs" || req.URL.Query().Get("service") != "demo_web" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "available", "observed_at": now.Format(time.RFC3339Nano),
			"window": map[string]string{"start": now.Add(-time.Hour).Format(time.RFC3339Nano), "end": now.Format(time.RFC3339Nano)},
			"rows":   []map[string]any{{"service.name": "demo_web", "body": "line"}}, "truncated": true,
			"truncation_reasons": []string{"row_limit"}, "coverage": "bounded",
		})
	}))
	t.Cleanup(server.Close)
	a := NewObservability(New(server.URL, "tok", time.Second))
	w := observability.Window{Start: now.Add(-time.Hour), End: now}
	result, err := a.Logs(context.Background(), observability.LogQuery{Service: "demo_web", Window: w, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != observability.StatusAvailable || len(result.Rows) != 1 || !result.Truncated || result.Window != w {
		t.Fatalf("remote result = %#v, want DTO mapped result", result)
	}
}

func TestRemoteObservabilityUnavailableIsTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "unavailable", "window": map[string]string{"start": "2026-10-20T11:00:00Z", "end": "2026-10-20T12:00:00Z"}, "rows": []any{}, "error": "sanitized",
		})
	}))
	t.Cleanup(server.Close)
	a := NewObservability(New(server.URL, "tok", time.Second))
	w := observability.Window{Start: time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	result, err := a.Logs(context.Background(), observability.LogQuery{Window: w, Limit: 10})
	if err != observability.ErrUnavailable || result.Status != observability.StatusUnavailable {
		t.Fatalf("unavailable = %#v, %v; want typed unavailable", result, err)
	}
}

// TestRemoteTelemetryOverviewAndEvidenceOrder covers the console's adapter:
// the three overview reads, the service-name list envelope, and the one thing
// that is NOT a straight passthrough — the evidence slice is oldest-first for
// the page, while the public API pages newest-first.
func TestRemoteTelemetryOverviewAndEvidenceOrder(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch req.URL.Path {
		case "/api/observability/volume", "/api/observability/traces/stats":
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "available", "observed_at": now.Format(time.RFC3339Nano),
				"window": map[string]string{"start": now.Add(-time.Hour).Format(time.RFC3339Nano), "end": now.Format(time.RFC3339Nano)},
				"rows":   []map[string]any{{"level": "error", "n": 3}}, "truncated": false,
			})
		case "/api/observability/services":
			writeJSON(w, http.StatusOK, map[string]any{"status": "available", "services": []string{"demo_web", "shop_api"}})
		case "/api/observability/logs":
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "available", "observed_at": now.Format(time.RFC3339Nano),
				"window": map[string]string{"start": now.Add(-time.Hour).Format(time.RFC3339Nano), "end": now.Format(time.RFC3339Nano)},
				"rows":   []map[string]any{{"body": "newest"}, {"body": "oldest"}},
			})
		default:
			http.Error(w, "unexpected path "+req.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	a := NewTelemetry(server.URL, "tok", time.Second)
	w := observability.Window{Start: now.Add(-time.Hour), End: now}
	ctx := context.Background()

	vol, err := a.LogVolume(ctx, observability.MetricQuery{Window: w, Bucket: time.Minute})
	if err != nil || vol.Status != observability.StatusAvailable || len(vol.Rows) != 1 {
		t.Fatalf("LogVolume = %#v, %v", vol, err)
	}
	stats, err := a.TraceStats(ctx, observability.MetricQuery{Window: w, Bucket: time.Minute})
	if err != nil || stats.Status != observability.StatusAvailable {
		t.Fatalf("TraceStats = %#v, %v", stats, err)
	}
	names, err := a.ServiceNames(ctx, w)
	if err != nil || len(names) != 2 || names[1] != "shop_api" {
		t.Fatalf("ServiceNames = %v, %v", names, err)
	}

	// Source.Query is the console's evidence read: the page renders oldest-first
	// so a failure's preceding lines appear above it.
	ev, err := a.Query(ctx, observability.Query{Service: "demo_web", Window: w, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Rows) != 2 || ev.Rows[0]["body"] != "oldest" || ev.Rows[1]["body"] != "newest" {
		t.Fatalf("evidence rows = %#v, want oldest-first", ev.Rows)
	}
}

// TestRemoteTelemetryServiceNamesUnavailable proves an unavailable backend is
// reported as an error rather than as "no services", which the console shows as
// an empty filter and is indistinguishable from a healthy quiet cluster.
func TestRemoteTelemetryServiceNamesUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "unavailable", "services": []string{}})
	}))
	t.Cleanup(server.Close)
	now := time.Now().UTC()
	_, err := NewTelemetry(server.URL, "tok", time.Second).ServiceNames(context.Background(),
		observability.Window{Start: now.Add(-time.Hour), End: now})
	if err == nil {
		t.Fatal("want ErrUnavailable for a status=unavailable response")
	}
}

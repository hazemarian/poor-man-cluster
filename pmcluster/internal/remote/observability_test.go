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

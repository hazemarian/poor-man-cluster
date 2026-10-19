package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneObserverRoutesDefaultToObservability(t *testing.T) {
	app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true, StandaloneObserver: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })

	for _, path := range []string{"/web/", "/web/overview"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "Observability") {
			t.Errorf("GET %s did not render native observability page", path)
		}
		if strings.Contains(body, `href="/web/deploy"`) || strings.Contains(body, `href="/web/services"`) {
			t.Errorf("GET %s rendered Swarm deploy navigation in observer mode", path)
		}
	}
}

func TestStandaloneObserverKeepsLoginGate(t *testing.T) {
	app, err := NewApp(Config{
		DataDir:            ":memory:",
		EnvUser:            "admin",
		EnvPass:            "fixture-password",
		SessionSecret:      []byte("0123456789abcdef"),
		StandaloneObserver: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })

	req := httptest.NewRequest(http.MethodGet, "/web/", nil)
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/web/login" {
		t.Fatalf("unauth observer root = %d Location=%q, want 302 /web/login", rr.Code, rr.Header().Get("Location"))
	}
}

func TestStandaloneObserverFleetRoutesCombinedDashboard(t *testing.T) {
	const token = "fleet-daemon-token"
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/me":
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"id":1,"name":"fleet"}`))
		case "/api/nodes":
			_, _ = w.Write([]byte(`{"nodes":[{"hostname":"alpha","role":"manager","status":"ready","availability":"active","is_leader":true}]}`))
		case "/api/services":
			_, _ = w.Write([]byte(`{"services":[]}`))
		case "/api/v1/query":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()

	path := filepath.Join(t.TempDir(), "hosts.json")
	b, err := json.Marshal([]map[string]string{{
		"name": "alpha", "hostname": "alpha", "daemon_url": daemon.URL,
		"daemon_token": token, "parseable_url": daemon.URL, "parseable_auth": "Basic ZmxlZXQ=",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(Config{
		DataDir: ":memory:", LoginDisabled: true, StandaloneObserver: true,
		FleetConfigPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })

	for _, path := range []string{"/web/", "/web/overview", "/web/fleet", "/web/observability?tab=overview", "/web/observability?tab=logs", "/web/observability?tab=metrics", "/web/observability?tab=traces"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		app.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "observability-node") || !strings.Contains(rr.Body.String(), "alpha") {
			t.Errorf("GET %s did not render combined Observability with the configured node", path)
		}
	}
}

func TestNormalModeKeepsOverviewShell(t *testing.T) {
	app, err := NewApp(Config{DataDir: ":memory:", LoginDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })

	req := httptest.NewRequest(http.MethodGet, "/web/", nil)
	rr := httptest.NewRecorder()
	app.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("normal GET /web/ = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `hx-get="/web/overview"`) || !strings.Contains(body, `href="/web/deploy"`) {
		t.Fatalf("normal overview shell changed unexpectedly")
	}
}

package ui

import (
	"net/http"
	"net/http/httptest"
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

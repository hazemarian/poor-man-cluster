package controllers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestObservBridgeGate(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"anonymous is refused", nil, false},
		{"oauth2-proxy identity header is accepted", map[string]string{"X-Auth-Request-User": "ops@example.com"}, true},
		{"oauth2-proxy email header is accepted", map[string]string{"X-Auth-Request-Email": "ops@example.com"}, true},
		{"forwarded variants still tolerated", map[string]string{"X-Forwarded-User": "ops@example.com"}, true},
		{"basic auth is accepted (non-SSO installs)", map[string]string{"Authorization": "Basic YTpi"}, true},
		{"a bearer token is not", map[string]string{"Authorization": "Bearer abc"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/sso-bridge", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := observBridgeGate(r); got != tc.want {
				t.Errorf("observBridgeGate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestObservBridgePageSeedsTheSession pins the contract the page exists for:
// the observability SPA decides it is logged in from localStorage["authToken"],
// so the page must write exactly that key with the managed credential — the same
// base64 the router's injection middleware sends — and then land on the app root.
func TestObservBridgePageSeedsTheSession(t *testing.T) {
	basicAuth := "b3BzQGV4YW1wbGUuY29tOnB3" // base64("ops@example.com:pw")
	t.Setenv(observBasicAuthEnv, basicAuth)

	rec := httptest.NewRecorder()
	g, _ := gin.CreateTestContext(rec)
	g.Request = httptest.NewRequest("GET", "/sso-bridge", nil)
	g.Request.Host = "observ.example.com" // the route is only servable on the observability host
	g.Request.Header.Set("X-Forwarded-User", "ops@example.com")

	ObservBridge{Controller: &Controller{Domain: "example.com"}}.Page(g)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `const managed = "`+basicAuth+`"`) {
		t.Errorf("page must carry the managed credential; got:\n%s", body)
	}
	if !strings.Contains(body, `localStorage.setItem("authToken", chosen)`) {
		t.Errorf("page must write localStorage.authToken with the VERIFIED value; got:\n%s", body)
	}
	// It must check the credential against a real endpoint instead of assuming
	// which shape the SPA expects.
	if !strings.Contains(body, `fetch("/api/v1/about"`) {
		t.Error("page must verify the credential before storing it")
	}
	if !strings.Contains(body, `location.replace("/")`) {
		t.Error("page must land on the observability root, not the removed /web/ path")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page carries a credential and must not be cached")
	}
}

// TestObservBridgeRefusesTheConsoleHost is H1 of the 2026-11-21 audit: the catch-all
// `pmcluster-api` router on the CONSOLE host has no auth middleware, so a request to /sso-bridge
// there carries whatever identity headers the client invented — and the page hands out the managed
// observability credential. Host is the one thing a client cannot talk its way past Traefik.
func TestObservBridgeRefusesTheConsoleHost(t *testing.T) {
	t.Setenv(observBasicAuthEnv, "b3BzQGV4YW1wbGUuY29tOnB3")

	rec := httptest.NewRecorder()
	g, _ := gin.CreateTestContext(rec)
	g.Request = httptest.NewRequest("GET", "/sso-bridge", nil)
	g.Request.Host = "pmcluster.example.com"                // NOT the observability host
	g.Request.Header.Set("X-Auth-Request-User", "attacker") // supplied by the client, not by oauth2-proxy

	ObservBridge{Controller: &Controller{Domain: "example.com"}}.Page(g)

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 on the console host", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "authToken") {
		t.Error("the credential was served on a host where identity headers are not proof")
	}
}

// An anonymous caller must never be handed the credential.
func TestObservBridgePageRefusesAnonymous(t *testing.T) {
	t.Setenv(observBasicAuthEnv, "b3BzQGV4YW1wbGUuY29tOnB3")

	rec := httptest.NewRecorder()
	g, _ := gin.CreateTestContext(rec)
	g.Request = httptest.NewRequest("GET", "/sso-bridge", nil)
	g.Request.Host = "observ.example.com"

	ObservBridge{Controller: &Controller{Domain: "example.com"}}.Page(g)

	if rec.Code != 403 {
		t.Fatalf("anonymous status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "authToken") {
		t.Error("an anonymous response must not contain the session seeding")
	}
}

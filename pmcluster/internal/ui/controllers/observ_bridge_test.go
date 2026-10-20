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

// serveBridge runs the bridge handler for one request and returns the recorded
// response. The Host is set explicitly because httptest defaults it.
func serveBridge(t *testing.T, bridge ObservBridge, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	g, _ := gin.CreateTestContext(rec)
	g.Request = httptest.NewRequest("GET", "/sso-bridge", nil)
	g.Request.Host = host
	for k, v := range headers {
		g.Request.Header.Set(k, v)
	}
	bridge.Page(g)
	return rec
}

// TestObservBridgePageSeedsTheSession pins the contract the page exists for:
// the observability SPA decides it is logged in from localStorage["authToken"],
// so the page must write exactly that key with the managed credential — the same
// base64 the router's injection middleware sends — and then land on the app root.
func TestObservBridgePageSeedsTheSession(t *testing.T) {
	basicAuth := "b3BzQGV4YW1wbGUuY29tOnB3" // base64("ops@example.com:pw")
	t.Setenv(observBasicAuthEnv, basicAuth)

	bridge := ObservBridge{Controller: &Controller{Domain: "example.com"}}
	rec := serveBridge(t, bridge, "observ.example.com", map[string]string{"X-Forwarded-User": "ops@example.com"})

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

// An anonymous caller must never be handed the credential.
func TestObservBridgePageRefusesAnonymous(t *testing.T) {
	t.Setenv(observBasicAuthEnv, "b3BzQGV4YW1wbGUuY29tOnB3")

	bridge := ObservBridge{Controller: &Controller{Domain: "example.com"}}
	rec := serveBridge(t, bridge, "observ.example.com", nil)

	if rec.Code != 403 {
		t.Fatalf("anonymous status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "authToken") {
		t.Error("an anonymous response must not contain the session seeding")
	}
}

// The bridge hands out the managed admin credential, so it must answer only on
// observ.<domain>. Every other host is a bare 404 that never carries the
// credential, even when the caller presents valid Basic auth.
func TestObservBridgeServesOnlyTheObservHost(t *testing.T) {
	basicAuth := "b3BzQGV4YW1wbGUuY29tOnB3"
	t.Setenv(observBasicAuthEnv, basicAuth)
	basic := map[string]string{"Authorization": "Basic " + basicAuth}
	configured := ObservBridge{Controller: &Controller{Domain: "example.com"}}

	cases := []struct {
		name    string
		bridge  ObservBridge
		host    string
		headers map[string]string
		status  int
	}{
		{"console host with basic auth is 404", configured, "pmcluster.example.com", basic, 404},
		{"bare domain with basic auth is 404", configured, "example.com", basic, 404},
		{"host that merely ends in the domain is 404", configured, "evilobserv.example.com", basic, 404},
		{"forwarded host cannot be spoofed", configured, "pmcluster.example.com", merge(basic, map[string]string{"X-Forwarded-Host": "observ.example.com"}), 404},
		{"observ host with basic auth is served", configured, "observ.example.com", basic, 200},
		{"host is case-insensitive", configured, "OBSERV.Example.COM", basic, 200},
		{"port is ignored", configured, "observ.example.com:443", basic, 200},
		{"trailing dot is ignored", configured, "observ.example.com.", basic, 200},
		{"observ host with no auth is still refused", configured, "observ.example.com", nil, 403},
		{"unset domain fails closed", ObservBridge{Controller: &Controller{}}, "observ.example.com", basic, 404},
		{"nil controller fails closed", ObservBridge{}, "observ.example.com", basic, 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveBridge(t, tc.bridge, tc.host, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.status == 404 {
				if strings.Contains(rec.Body.String(), basicAuth) || strings.Contains(rec.Body.String(), "authToken") {
					t.Errorf("a refused host must not receive the credential or the session seeding")
				}
			}
		})
	}
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

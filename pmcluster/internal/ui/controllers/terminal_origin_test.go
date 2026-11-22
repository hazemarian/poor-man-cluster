package controllers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSameOrigin pins H4's socket half: a WebSocket upgrade from a page on another host must not be
// accepted, because the browser attaches the operator's console cookie to it. A client that sends no
// Origin (the CLI) is allowed — there is no ambient session to ride.
func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
		why          string
	}{
		{"", "pmcluster.example.com", true, "no Origin: a non-browser client"},
		{"https://pmcluster.example.com", "pmcluster.example.com", true, "the console itself"},
		{"https://evil.example.com", "pmcluster.example.com", false, "another app on the same domain"},
		{"https://pmcluster.example.com.evil.tld", "pmcluster.example.com", false, "a lookalike host"},
		{"https://pmcluster.example.com:443", "pmcluster.example.com:443", true, "an explicit port, matched"},
		{"http://pmcluster.example.com", "pmcluster.example.com:443", false, "different host:port strings"},
		{"not a url", "pmcluster.example.com", false, "unparseable Origin"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/web/stacks/x/shell", nil)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if got := sameOrigin(r); got != tc.want {
			t.Errorf("sameOrigin(origin=%q, host=%q) = %v, want %v (%s)", tc.origin, tc.host, got, tc.want, tc.why)
		}
	}
}

// TestShellPage_RefusesAGet — creating a shell on a GET is what made it reachable by a link, an <img>
// or a same-site GET from a compromised app. The handler refuses anything that is not a POST, even if
// a route were ever wired to it again.
func TestShellPage_RefusesAGet(t *testing.T) {
	term := Terminal{} // no API, no views: the method check must fire before either is touched
	rec := httptest.NewRecorder()
	g, _ := gin.CreateTestContext(rec)
	g.Request = httptest.NewRequest(http.MethodGet, "/web/stacks/demo/shell", nil)
	term.ShellPage(g)
	// gin buffers the status in a test context; the recorder only learns it on a flush.
	if got := g.Writer.Status(); got != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: a GET must not open a shell session", got)
	}
}

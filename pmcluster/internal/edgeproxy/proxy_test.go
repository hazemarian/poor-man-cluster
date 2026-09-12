package edgeproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstream captures what the fake daemon observed, for assertions.
type observed struct {
	gotXRealIP string
	gotXFF     string
	gotPath    string
	gotConn    string // Connection header arriving at upstream
}

// fakeDaemon returns an httptest server that records request headers and
// answers some /api/* and /webhook/* endpoints.
func fakeDaemon(t *testing.T, rec *observed) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.gotXRealIP = r.Header.Get("X-Real-IP")
		rec.gotXFF = r.Header.Get("X-Forwarded-For")
		rec.gotConn = r.Header.Get("Connection")
		rec.gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"path":"`+r.URL.Path+`"}`)
	}))
}

func TestNew_ProxiesAPIPathAndRealIP(t *testing.T) {
	var rec observed
	up := fakeDaemon(t, &rec)
	defer up.Close()

	cfg := FromEnv()
	cfg.Upstream = up.URL
	cfg.TrustedCIDRs = []string{"10.0.0.0/8"}
	h := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "http://pmcluster.example/api/stacks", nil)
	req.RemoteAddr = "10.0.3.4:54321" // Traefik (trusted)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.3.4")
	req.Header.Set("X-Forwarded-Proto", "https")
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if rec.gotXRealIP != "203.0.113.7" {
		t.Errorf("upstream X-Real-IP = %q, want 203.0.113.7", rec.gotXRealIP)
	}
	if rec.gotXFF != "203.0.113.7" {
		t.Errorf("upstream X-Forwarded-For = %q, want 203.0.113.7", rec.gotXFF)
	}
	if rec.gotPath != "/api/stacks" {
		t.Errorf("upstream path = %q, want /api/stacks", rec.gotPath)
	}
}

func TestNew_StripsHopByHopHeader(t *testing.T) {
	var rec observed
	up := fakeDaemon(t, &rec)
	defer up.Close()

	cfg := FromEnv()
	cfg.Upstream = up.URL
	cfg.TrustedCIDRs = []string{"10.0.0.0/8"}
	h := New(cfg)

	req := httptest.NewRequest(http.MethodPost, "http://x/webhook/github", strings.NewReader("{}"))
	req.RemoteAddr = "10.0.3.4:1"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 10.0.3.4")
	req.Header.Set("Connection", "keep-alive") // hop-by-hop, must not reach upstream
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rw.Code, rw.Body.String())
	}
	if rec.gotConn == "keep-alive" {
		t.Errorf("hop-by-hop Connection header leaked to upstream")
	}
	if rec.gotPath != "/webhook/github" {
		t.Errorf("upstream path = %q, want /webhook/github", rec.gotPath)
	}
}

func TestNew_WebhookRateLimitsPerIP(t *testing.T) {
	up := fakeDaemon(t, &observed{})
	defer up.Close()

	cfg := FromEnv()
	cfg.Upstream = up.URL
	cfg.TrustedCIDRs = []string{"10.0.0.0/8"}
	cfg.WebhookRate = 1
	cfg.WebhookBurst = 1
	h := New(cfg)

	do := func(rclientIP string) int {
		req := httptest.NewRequest(http.MethodPost, "http://x/webhook/github", strings.NewReader("{}"))
		req.RemoteAddr = "10.0.3.4:1"
		req.Header.Set("X-Forwarded-For", rclientIP+", 10.0.3.4")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		return rw.Code
	}

	if got := do("198.51.100.1"); got != http.StatusOK {
		t.Fatalf("first request = %d, want 200", got)
	}
	if got := do("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Errorf("second burst-exceeding request = %d, want 429", got)
	}
	// Different client IP is isolated.
	if got := do("198.51.100.2"); got != http.StatusOK {
		t.Errorf("different client IP request = %d, want 200", got)
	}
}

package edgeproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upstream captures what the fake daemon observed, for assertions.
type observed struct {
	gotXRealIP string
	gotXFF     string
	gotPath    string
	gotConn    string
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
	req.RemoteAddr = "10.0.3.4:54321"
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
	req.Header.Set("Connection", "keep-alive")
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

// TestNew_UpstreamUnavailableReturnsCleanJSON502 verifies that a dead upstream
// (the node's daemon API not listening) yields a stable, dependency-free JSON
// 503 instead of a bare Go transport "dial tcp ...: connection refused" body.
// 503, not 502: the API is genuinely unavailable, so the caller should retry.
func TestNew_UpstreamUnavailableReturnsCleanJSON503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upURL := srv.URL
	srv.Close() // the upstream now refuses connections

	cfg := FromEnv()
	cfg.Upstream = upURL
	cfg.TrustedCIDRs = []string{"10.0.0.0/8"}
	h := New(cfg)

	req := httptest.NewRequest(http.MethodGet, "http://pmcluster.example/api/stacks", nil)
	req.RemoteAddr = "10.0.3.4:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.3.4")
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rw.Code, rw.Body.String())
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if body := strings.TrimSpace(rw.Body.String()); !strings.Contains(body, `"error":"control plane unavailable"`) {
		t.Errorf("body = %q, want the unavailable-control-plane JSON body", body)
	}
}

// An upstream that accepts the request and never answers inside REQUEST_TIMEOUT
// is NOT an unavailable control plane: the daemon is there and busy. The proxy
// must say so, because the caller's log is where an operator looks first.
//
// Observed in production: a webhook whose deploy failed on a missing secret was
// reported to CI as {"error":"control plane unavailable"} because the receiver
// answered after the proxy's 30s ceiling.
func TestNew_UpstreamTimeoutReturnsGatewayTimeout(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)

	cfg := FromEnv()
	cfg.Upstream = up.URL
	cfg.TrustedCIDRs = []string{"10.0.0.0/8"}
	cfg.RequestTimeout = 150 * time.Millisecond
	h := New(cfg)

	req := httptest.NewRequest(http.MethodPost, "http://pmcluster.example/webhook/github-adam", nil)
	req.RemoteAddr = "10.0.3.4:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.3.4")
	rw := httptest.NewRecorder()

	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if strings.Contains(body, "control plane unavailable") {
		t.Errorf("body = %q — a timeout must not claim the control plane is unavailable", body)
	}
	if !strings.Contains(body, "did not answer within") {
		t.Errorf("body = %q, want the timeout named", body)
	}
}

// TestNew_RejectsOversizedBody verifies the LimitBody middleware is wired into
// the proxy chain: an oversized request body is rejected with 413 instead of
// streamed through to the upstream.
func TestNew_RejectsOversizedBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	cfg := FromEnv()
	cfg.Upstream = up.URL
	cfg.MaxBodyBytes = 16
	h := New(cfg)

	req := httptest.NewRequest(http.MethodPost, "http://pmcluster.example/api/stacks", strings.NewReader(strings.Repeat("a", 100)))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413; body=%s", rw.Code, rw.Body.String())
	}
	if body := strings.TrimSpace(rw.Body.String()); body != `{"error":"request body too large"}` {
		t.Errorf("body = %q, want the 413 JSON body", body)
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

	if got := do("198.51.100.2"); got != http.StatusOK {
		t.Errorf("different client IP request = %d, want 200", got)
	}
}

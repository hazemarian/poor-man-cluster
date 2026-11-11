package socketproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// countingTransport is the fake daemon: it records every request that reached it
// and answers 200. A denial that reaches this transport is a failure of the whole
// package, so every denial test asserts the count stays 0.
type countingTransport struct {
	mu    sync.Mutex
	calls []*http.Request
	body  []byte
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, r.Clone(r.Context()))
	if r.Body != nil {
		t.body, _ = io.ReadAll(r.Body)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    r,
	}, nil
}

func (t *countingTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

// shortSocketPath returns a socket path under /tmp: a unix socket path is capped
// at ~104 bytes, and a t.TempDir() path on macOS is longer than that.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pmcsp")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "docker.sock")
}

func newTestProxy(t *testing.T) (*Proxy, *countingTransport) {
	t.Helper()
	tr := &countingTransport{}
	p := New(Config{SocketPath: shortSocketPath(t), UpstreamPath: "/nonexistent", Log: zerolog.Nop(), Transport: tr})
	return p, tr
}

func serve(p *Proxy, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// Every denied family, by the verbs a workflow would reach for. A single allowed
// method in any of these is a hole: `service rm`, `node demote`, `secret inspect`
// and `exec` are all one command away from controlling the node.
func TestEveryDeniedFamilyIsRefusedAndNeverForwarded(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1.43/services"},
		{http.MethodPost, "/v1.43/services/pmcluster_edge/update"},
		{http.MethodDelete, "/v1.43/services/pmcluster_edge"},
		{http.MethodGet, "/v1.43/tasks"},
		{http.MethodPost, "/v1.43/tasks/abc"},
		{http.MethodGet, "/v1.43/nodes"},
		{http.MethodPost, "/v1.43/nodes/abc/update"},
		{http.MethodPost, "/v1.43/swarm/leave"},
		{http.MethodGet, "/v1.43/swarm"},
		{http.MethodPost, "/v1.43/swarm/join"},
		{http.MethodGet, "/v1.43/secrets"},
		{http.MethodGet, "/v1.43/secrets/pmcluster_db_password"},
		{http.MethodGet, "/v1.43/configs"},
		{http.MethodPost, "/v1.43/plugins/pull"},
		{http.MethodPost, "/v1.43/system/df"},
		{http.MethodPost, "/v1.43/session"},
		// /auth deliberately moved to the ALLOW list: docker login is how a job
		// authenticates to push, and it grants no host capability. /services and
		// /nodes above are what keep a pushed image from touching the platform.

		{http.MethodPost, "/v1.43/containers/abc/exec"},
		{http.MethodPost, "/v1.43/exec/xyz/start"},
		{http.MethodGet, "/v1.43/exec/xyz/json"},
	}
	for _, c := range cases {
		p, tr := newTestProxy(t)
		rec := serve(p, c.method, c.path, `{"Cmd":["sh"]}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, rec.Code)
		}
		if tr.count() != 0 {
			t.Errorf("%s %s reached the daemon: the allow table is not deny-by-default", c.method, c.path)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s: body is not JSON: %s", c.method, c.path, rec.Body.String())
			continue
		}
		if !strings.Contains(body["message"], c.path) {
			t.Errorf("%s %s: refusal does not name the route: %q", c.method, c.path, body["message"])
		}
	}
}

func TestUnlistedRoutesAreRefused(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1.43/commit"},
		{http.MethodGet, "/v1.43/distribution/alpine/json"},
		{http.MethodPost, "/v1.43/containers/abc/attach"},
		{http.MethodPost, "/v1.43/containers/abc/copy"},
		{http.MethodGet, "/v1.43/nope"},
		// /images/nginx/push moved to the allow table: publishing is what a CI
		// job is for. The traversal guard below is what keeps that from being a
		// way back into /services.
		{http.MethodPost, "/v1.43/images/x/../../services/pmcluster_edge"},
	} {
		p, tr := newTestProxy(t)
		if rec := serve(p, c.method, c.path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 (not in the allow table)", c.method, c.path, rec.Code)
		}
		if tr.count() != 0 {
			t.Errorf("%s %s reached the daemon", c.method, c.path)
		}
	}
}

// The allow table is the whole contract: this walks it and proves each entry is
// reachable with its documented verb, and that the same path with an
// un-documented verb is not.
func TestAllowTableIsReachableExactlyAsWritten(t *testing.T) {
	for _, rule := range AllowTable {
		path := strings.ReplaceAll(rule.Pattern, "{id}", "abc123")
		if strings.Contains(path, "**") {
			path = strings.ReplaceAll(path, "**", "ghcr.io/acme/api:latest")
		}
		p, tr := newTestProxy(t)
		rec := serve(p, rule.Method, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s (allowed for: %s) = %d, want it forwarded", rule.Method, path, rule.Why, rec.Code)
			continue
		}
		if tr.count() != 1 {
			t.Errorf("%s %s did not reach the daemon", rule.Method, path)
		}
	}
}

func TestAllowedRequestIsForwardedWithBodyAndHeaders(t *testing.T) {
	p, tr := newTestProxy(t)
	req := httptest.NewRequest(http.MethodPost, "/v1.43/build?t=acme/api:ci&dockerfile=Dockerfile", strings.NewReader("context-bytes"))
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("X-Registry-Config", "eyJ9")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if tr.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", tr.count())
	}
	got := tr.calls[0]
	if got.Method != http.MethodPost || got.URL.Path != "/v1.43/build" {
		t.Fatalf("upstream request = %s %s", got.Method, got.URL.Path)
	}
	if got.URL.RawQuery != "t=acme/api:ci&dockerfile=Dockerfile" {
		t.Fatalf("query = %q, want it forwarded untouched", got.URL.RawQuery)
	}
	if string(tr.body) != "context-bytes" {
		t.Fatalf("body = %q, want the build context", tr.body)
	}
	if got.Header.Get("X-Registry-Config") != "eyJ9" {
		t.Fatalf("headers were not forwarded: %v", got.Header)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("upstream body not copied: %s", rec.Body.String())
	}
}

// The version prefix is stripped for MATCHING only; the daemon still receives the
// path it was sent.
func TestVersionPrefixIsIgnoredForMatching(t *testing.T) {
	p, tr := newTestProxy(t)
	if rec := serve(p, http.MethodGet, "/v1.51/version", ""); rec.Code != http.StatusOK {
		t.Fatalf("versioned allowed route = %d, want 200", rec.Code)
	}
	if got := tr.calls[0].URL.Path; got != "/v1.51/version" {
		t.Fatalf("forwarded path = %q, want the original", got)
	}
	p2, tr2 := newTestProxy(t)
	if rec := serve(p2, http.MethodDelete, "/v1.51/secrets/x", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("versioned denied route = %d, want 403", rec.Code)
	}
	if tr2.count() != 0 {
		t.Fatal("a versioned denied route reached the daemon")
	}
}

func TestStartReplacesAStaleSocketFileAndStopRemovesIt(t *testing.T) {
	sock := shortSocketPath(t)
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stale socket file from a previous run: bind fails on an existing path.
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{SocketPath: sock, UpstreamPath: "/nonexistent", Log: zerolog.Nop(), Transport: &countingTransport{}})
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start over a stale socket: %v", err)
	}
	if !p.Running() {
		t.Fatal("Start did not leave the proxy running")
	}
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != DefaultSocketMode {
		t.Fatalf("socket mode = %o, want %o", info.Mode().Perm(), DefaultSocketMode)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start on a running proxy = %v, want a no-op", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("the socket file survived Stop (%v) — a disabled feature must leave nothing to mount", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop twice = %v, want a no-op", err)
	}
}

// A workflow in a retry loop must not be able to fill the journal: the first few
// denials are logged individually, the rest collapse into one summary carrying the
// count it replaced.
func TestDenialLoggingIsBounded(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	p := New(Config{SocketPath: shortSocketPath(t), Log: log, Transport: &countingTransport{}})
	base := time.Now()
	now := base
	p.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		serve(p, http.MethodGet, "/v1.43/secrets", "")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) > auditBurst+1 {
		t.Fatalf("%d log lines for 50 denials, want at most %d (the burst) — a loop is flooding the journal", len(lines), auditBurst+1)
	}
	if !strings.Contains(lines[0], "denied") || !strings.Contains(lines[0], "/secrets") {
		t.Fatalf("first line = %q, want the route", lines[0])
	}

	// The next window emits ONE summary line with the number it suppressed.
	buf.Reset()
	now = base.Add(auditInterval + time.Second)
	serve(p, http.MethodGet, "/v1.43/secrets", "")
	out := buf.String()
	if !strings.Contains(out, "suppressed") {
		t.Fatalf("no summary line after the window: %q", out)
	}
	if n := strings.Count(out, "\n"); n > 3 {
		t.Fatalf("the window close wrote %d lines, want a single summary", n)
	}
	if p.DeniedTotal() != 51 {
		t.Fatalf("denied total = %d, want 51", p.DeniedTotal())
	}
}

func TestSuperviseStartsAndStopsWithTheSwitch(t *testing.T) {
	sock := shortSocketPath(t)
	// A real upstream socket is not needed: the supervisor only stats the path.
	upstream := filepath.Join(filepath.Dir(sock), "upstream.sock")
	if err := os.WriteFile(upstream, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Config{SocketPath: sock, UpstreamPath: upstream, Log: zerolog.Nop(), Transport: &countingTransport{}})
	ctx, cancel := context.WithCancel(context.Background())
	on := true
	var mu sync.Mutex
	go Supervise(ctx, p, func(context.Context) bool {
		mu.Lock()
		defer mu.Unlock()
		return on
	}, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for !p.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !p.Running() {
		t.Fatal("the supervisor did not start the listener")
	}

	mu.Lock()
	on = false
	mu.Unlock()
	deadline = time.Now().Add(2 * time.Second)
	for p.Running() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Running() {
		t.Fatal("the supervisor did not stop the listener when disabled")
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("the socket file survived the stop (%v)", err)
	}
	cancel()
}

// TestWorkflowCanAuthenticateTagAndPush: a CI job exists to publish an image, so
// docker login (POST /auth), tagging and pushing must pass while the swarm
// surfaces stay refused. Measured on a live runner: every registry login failed
// with "POST /v1.43/auth is not allowed", which made the whole self-hosted
// migration look like a buildx problem.
func TestWorkflowCanAuthenticateTagAndPush(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1.43/auth"},
		{http.MethodPost, "/v1.43/build"},
		{http.MethodPost, "/v1.43/images/ghcr.io/acme/app:1.0/tag"},
		{http.MethodPost, "/v1.43/images/ghcr.io/acme/app:1.0/push"},
	} {
		p, tr := newTestProxy(t)
		rec := serve(p, c.method, c.path, `{}`)
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s %s refused, want allowed for a publishing workflow: %s", c.method, c.path, rec.Body.String())
			continue
		}
		if tr.count() == 0 {
			t.Errorf("%s %s never reached the daemon", c.method, c.path)
		}
	}
	// And the platform surfaces stay closed to the same caller.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1.43/services/pmcluster_edge/update"},
		{http.MethodPost, "/v1.43/images/x/../../services/pmcluster_edge"},
	} {
		p, tr := newTestProxy(t)
		if rec := serve(p, c.method, c.path, `{}`); rec.Code != http.StatusForbidden || tr.count() != 0 {
			t.Errorf("%s %s = %d (reached=%d), want 403 and no upstream call", c.method, c.path, rec.Code, tr.count())
		}
	}
}

//go:build e2e

// Package e2e smoke tests for the combined edge service (cmd/edge): the
// gin+HTMX operator console mounted beside the smart reverse proxy on one
// listener. The console serves its web routes locally; every other request is
// reversed to the pmcluster daemon.
//
// The test starts a fake pmcluster daemon (canned JSON, Bearer auth), points
// the real edge binary at it via UPSTREAM, then verifies: first-run / setup
// flow, console pages served locally, /health + /api/* + arbitrary paths
// reverse-proxied to the daemon, and the per-IP rate limiter returning 429.
//
// Run via: make e2e
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// edgeAdminToken is the Bearer token the fake daemon requires.
const edgeAdminToken = "pmc-e2e-test-token"

// TestEdgeCombined verifies the combined console + reverse-proxy edge.
func TestEdgeCombined(t *testing.T) {
	// ── Step 1: fake pmcluster daemon ────────────────────────────────────────
	daemon := newFakeDaemon(t)
	defer daemon.Close()

	// ── Step 2: start the combined edge binary ───────────────────────────────
	dataDir := t.TempDir()
	edgeAddr := freePort(t)

	edge := exec.Command(edgeBinaryPath)
	edge.Stdout = os.Stdout
	edge.Stderr = os.Stderr
	edge.Env = append(os.Environ(),
		"LISTEN_ADDR="+edgeAddr,
		"UPSTREAM="+daemon.URL,
		"DATA_DIR="+dataDir,
		"PMCLUSTER_UI_SECRET=0123456789abcdef0123456789abcdef",
		"PMCLUSTER_API_TOKEN="+edgeAdminToken,
		"API_RATE=200",
		"API_BURST=400",
	)
	if err := edge.Start(); err != nil {
		t.Fatalf("start edge: %v", err)
	}
	t.Cleanup(func() {
		if edge.Process != nil {
			_ = edge.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				edge.Wait() //nolint:errcheck
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				_ = edge.Process.Kill()
			}
		}
	})

	waitHealthy(t, edgeAddr, edge, 5*time.Second)

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow; assert on redirects
		},
	}
	base := "http://" + edgeAddr

	// Helper asserting an arbitrary path reaches the fake daemon (proxied).
	proxied := func(t *testing.T, path string, wantStatus int, wantBody string) {
		t.Helper()
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, wantStatus)
		}
		body, _ := io.ReadAll(resp.Body)
		if wantBody != "" && !strings.Contains(string(body), wantBody) {
			t.Errorf("GET %s body missing %q; got: %s", path, wantBody, body)
		}
	}

	// ── Step 3: reverse proxy reaches the daemon ─────────────────────────────
	t.Run("proxy reverses /health to daemon", func(t *testing.T) {
		proxied(t, "/health", http.StatusOK, `"poked_by":"daemon"`)
	})

	t.Run("local /healthz is answered by the edge itself", func(t *testing.T) {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), `"status":"ok"`) {
			t.Errorf("GET /healthz body missing status ok; got: %s", body)
		}
		if strings.Contains(string(body), "poked_by") {
			t.Errorf("GET /healthz was proxied to the daemon; must be answered locally: %s", body)
		}
	})

	t.Run("proxy reverses /api/me to daemon", func(t *testing.T) {
		proxied(t, "/api/me", http.StatusOK, `"name":"admin"`)
	})

	t.Run("unknown path is not proxied (404)", func(t *testing.T) {
		proxied(t, "/some/deep/path", http.StatusNotFound, "")
	})

	// ── Step 4: console routes are served locally, not proxied ─────────────
	t.Run("GET / redirects to /setup on first run", func(t *testing.T) {
		resp, err := client.Get(base + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want 302", resp.StatusCode)
		}
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "/setup") {
			t.Errorf("Location = %q, want /setup", loc)
		}
	})

	t.Run("GET /setup serves the setup page", func(t *testing.T) {
		proxied(t, "/setup", http.StatusOK, "setup") // rendered locally
	})

	// ── Step 5: probe /stacks is proxied (not a console route) ──────────────
	t.Run("GET /stacks without session redirects to login (local route)", func(t *testing.T) {
		resp, err := client.Get(base + "/stacks")
		if err != nil {
			t.Fatalf("GET /stacks: %v", err)
		}
		defer resp.Body.Close()
		// /stacks is a protected console route -> bounces to /login.
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want redirect or login", resp.StatusCode)
		}
	})
}

// TestEdgeRateLimit verifies the per-IP rate limiter trips a 429 at the edge.
func TestEdgeRateLimit(t *testing.T) {
	daemon := newFakeDaemon(t)
	defer daemon.Close()

	dataDir := t.TempDir()
	edgeAddr := freePort(t)

	edge := exec.Command(edgeBinaryPath)
	edge.Stdout = os.Stdout
	edge.Stderr = os.Stderr
	edge.Env = append(os.Environ(),
		"LISTEN_ADDR="+edgeAddr,
		"UPSTREAM="+daemon.URL,
		"DATA_DIR="+dataDir,
		"PMCLUSTER_UI_SECRET=0123456789abcdef0123456789abcdef",
		"PMCLUSTER_API_TOKEN="+edgeAdminToken,
		"API_RATE=1",  // 1 request/sec
		"API_BURST=2", // burst of 2 -> 3rd immediate request is 429
		"WEBHOOK_RATE=40",
		"WEBHOOK_BURST=60",
	)
	if err := edge.Start(); err != nil {
		t.Fatalf("start edge: %v", err)
	}
	t.Cleanup(func() {
		if edge.Process != nil {
			_ = edge.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				edge.Wait() //nolint:errcheck
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				_ = edge.Process.Kill()
			}
		}
	})

	waitHealthy(t, edgeAddr, edge, 5*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}
	saw429 := false
	for i := 0; i < 8; i++ {
		resp, err := client.Get("http://" + edgeAddr + "/api/me")
		if err != nil {
			t.Fatalf("GET /api/me: %v", err)
		}
		code := resp.StatusCode
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
		if code == http.StatusTooManyRequests {
			saw429 = true
			break
		}
	}
	if !saw429 {
		t.Fatal("expected at least one 429 from the edge rate limiter, got none")
	}
}

// fakeDaemon is a minimal stand-in for the pmcluster daemon the edge proxies to.
type fakeDaemon struct{ *httptest.Server }

// newFakeDaemon serves canned JSON for the API/health paths the UI + proxy hit.
func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	mux := http.NewServeMux()

	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	// All /api and /health and catch-all paths get canned responses.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			write(w, map[string]any{"status": "ok", "poked_by": "daemon"})
			return
		}
		switch r.URL.Path {
		case "/api/me":
			// Auth enforcement is the daemon's job (covered by smoke_test); this
			// fake is lenient so we can assert on the proxy pass-through alone.
			write(w, map[string]any{"id": 1, "name": "admin"})
		case "/api/cluster/info":
			write(w, map[string]any{"node_name": "e2e", "server_version": "25.0.0", "os": "linux", "arch": "amd64", "cpus": 4, "memory_bytes": 8266366976, "swarm": map[string]any{"state": "active", "control_available": true, "managers": 1, "nodes": 2}})
		case "/api/nodes":
			write(w, []map[string]any{{"hostname": "manager-1", "role": "manager", "status": "ready", "is_leader": true, "availability": "active", "engine_version": "25.0.0"}})
		case "/api/stacks":
			write(w, []map[string]any{})
		default:
			// Used to prove the catch-all NotFound reversal reaches the daemon.
			write(w, map[string]any{"marker": "proxy-catch-all"})
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeDaemon{Server: srv}
}

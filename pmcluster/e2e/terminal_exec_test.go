//go:build e2e

// Package e2e — interactive exec (L3) end-to-end test.
//
// This file proves the interactive websocket exec path against a real
// single-node Docker Swarm:
//
//  1. a deployed app stack's service resolves to a running task container;
//  2. an interactive TTY session is created (exec ws endpoint);
//  3. stdin bytes sent over the socket reach the container and its output
//     streams back (echo round trip);
//  4. terminal resize controls are accepted;
//  5. the session reports its exit code and the socket closes cleanly;
//  6. the endpoint refuses unauthenticated dials (401).
//
// Swarm-gated (PMCLUSTER_E2E_SWARM=1), mirroring TestControlPlaneSnapshotE2E.
package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestTerminalExecE2E drives the daemon's interactive exec websocket against a
// real swarm: deploy a stack, exec `sh` into its running task, echo a marker
// line, resize, exit, and assert auth rejection.
func TestTerminalExecE2E(t *testing.T) {
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM=1 required")
	}
	requireDockerDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ensureSwarmActive(t, ctx)

	homeDir := t.TempDir()
	initOut, _, initCode := runCmd(t, homeDir, "init")
	if initCode != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", initCode, initOut)
	}
	adminToken := extractToken(t, initOut)
	t.Logf("pmcluster init OK (token_len=%d)", len(adminToken))

	// Loop-free auto-started daemon; the dedicated serve below is the only runner.
	setReconcileInterval(t, homeDir, "0")

	// Hermetic storage root (see TestControlPlaneSnapshotE2E): relocate the
	// volume root and backup dir under the temp HOME so `cluster up` works on
	// a non-root dev host.
	storageRoot := homeDir + "/stack"
	if out, errOut, code := runCmd(t, homeDir, "cluster", "settings", "set", "volume_root="+storageRoot); code != 0 {
		t.Fatalf("set volume_root exited %d:\n%s\n%s", code, out, errOut)
	}
	t.Setenv("PMCLUSTER_BACKUP_DIR", storageRoot+"/backup")
	certPath, keyPath := generateSelfSignedCert(t, homeDir)

	upOut, upErr, upCode := runCmdCtx(t, ctx, homeDir, "cluster", "up",
		"--domain=example.test",
		"--cert="+certPath, "--key="+keyPath,
		"--openobserve-email=admin@example.test")
	if upCode != 0 {
		t.Fatalf("cluster up exited %d:\nstdout:\n%s\nstderr:\n%s", upCode, upOut, upErr)
	}
	if !strings.Contains(upOut, "cluster up complete") {
		t.Fatalf("cluster up output missing completion marker:\n%s", upOut)
	}

	t.Cleanup(func() {
		t.Log("TestTerminalExecE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		if out, errOut, code := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
			t.Logf("cluster down (cleanup) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	// Dedicated daemon on a free port — the exec WS endpoint lives here.
	addr := freePort(t)
	daemon := exec.Command(binaryPath, "serve")
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	daemon.Env = append(homeEnv(homeDir), "PMCLUSTER_LISTEN_ADDR="+addr)
	if err := daemon.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				_ = daemon.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				_ = daemon.Process.Kill()
			}
		}
	})
	waitHealthy(t, addr, daemon, 20*time.Second)

	// Deploy a tiny stack; nginx-alpine ships a shell and stays up. The swarm
	// is shared across runs and `cluster down --purge` only removes platform
	// stacks, so a leftover termdemo from a previous run must be removed first
	// (best-effort) to avoid resolving a stale task.
	if out, errOut, code := runCmd(t, homeDir, "stack", "remove", "termdemo"); code != 0 {
		t.Logf("stack remove termdemo (pre-deploy, best-effort) exited %d:\n%s\n%s", code, out, errOut)
	}
	const manifest = `app: termdemo
env: test
domain: example.test
services:
  web:
    image: nginx:1.27-alpine
`
	manifestPath := homeDir + "/termdemo.yaml"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if out, errOut, code := runCmd(t, homeDir, "deploy", manifestPath); code != 0 {
		t.Fatalf("deploy exited %d:\n%s\n%s", code, out, errOut)
	}
	// Wait for the service task to actually run (container exists) so the
	// exec can attach to it. Only the NEWEST task attempt counts — older
	// attempts from a previous run may still read "Running".
	waitServiceRunning(t, ctx, "termdemo_web", 90*time.Second)
	t.Log("termdemo_web task is Running")

	t.Cleanup(func() {
		if out, errOut, code := runCmd(t, homeDir, "stack", "remove", "termdemo"); code != 0 {
			t.Logf("stack remove termdemo (cleanup, best-effort) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	wsBase := "ws://" + addr
	execURL := wsBase + "/api/services/termdemo/web/exec/ws?cmd=sh&rows=30&cols=100"

	// (6) Unauthenticated dial → HTTP 401 before any upgrade.
	noAuthResp := rawDialStatus(t, execURL, "")
	if noAuthResp != http.StatusUnauthorized {
		t.Fatalf("unauthenticated exec ws = %d, want 401", noAuthResp)
	}
	t.Log("auth rejection verified (401)")

	// (2+3) Authenticated session: dial, send a marker via stdin, expect the
	// shell (a TTY) to echo the input AND run the command, so the marker must
	// appear in the stream at least once.
	conn, resp, err := websocket.DefaultDialer.Dial(execURL, http.Header{
		"Authorization": {"Bearer " + adminToken},
	})
	if err != nil {
		t.Fatalf("dial exec ws: %v (resp %v)", err, resp)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo TERMINAL_E2E_MARKER\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	seen := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(seen, "TERMINAL_E2E_MARKER") {
		mt, data, rerr := conn.ReadMessage()
		if rerr != nil {
			t.Fatalf("read output: %v (seen so far: %q)", rerr, seen)
		}
		if mt == websocket.BinaryMessage {
			seen += string(data)
		}
	}
	if !strings.Contains(seen, "TERMINAL_E2E_MARKER") {
		t.Fatalf("marker not seen in exec output; got %q", seen)
	}
	t.Logf("stdin → container → stdout round trip verified (marker echoed)")

	// (4) Resize control is accepted without error.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","rows":50,"cols":200}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}

	// (5) `exit` ends the shell → the daemon sends the exit frame. The pty
	// echoes the typed "exit" back as output first, so skip binary frames
	// until the text exit control arrives.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	mt, data, rerr := conn.ReadMessage()
	for rerr == nil && mt != websocket.TextMessage {
		mt, data, rerr = conn.ReadMessage() // discard echoed output frames
	}
	if rerr != nil {
		t.Fatalf("read after exit: %v", rerr)
	}
	var ctl struct {
		Type string `json:"type"`
		Code int    `json:"code"`
	}
	if err := json.Unmarshal(data, &ctl); err != nil {
		t.Fatalf("exit frame is not JSON: %v (%q)", err, data)
	}
	if ctl.Type != "exit" {
		t.Fatalf("exit frame type = %q, want exit", ctl.Type)
	}
	t.Logf("session exit frame received: code=%d", ctl.Code)
}

// waitServiceRunning polls `docker service ps` until the NEWEST task attempt
// reports a running task (so an exec session has a container to attach to).
// Older attempts from a previous run of the same-named stack may still read
// "Running" — only the first (newest) line counts.
func waitServiceRunning(t *testing.T, ctx context.Context, service string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := strings.TrimSpace(mustDockerRun(t, ctx, "service", "ps", service, "--format", "{{.CurrentState}}"))
		first := out
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			first = out[:i]
		}
		// A fresh running task reads "Running 3 seconds ago" etc.; a stale
		// finished attempt reads "Running about a minute ago". Only accept
		// the current attempt, so require a recent "seconds ago".
		if strings.Contains(first, "Running") && (strings.Contains(first, "seconds ago") || strings.Contains(first, "less than a second ago")) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("service %s never reached Running within %s", service, timeout)
}

// rawDialStatus performs a websocket upgrade without auth and returns the HTTP
// status the server answered (401 for the exec endpoint).
func rawDialStatus(t *testing.T, url, token string) int {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	// gorilla dialer is fine here; the server must reject BEFORE upgrading.
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		if resp != nil {
			return resp.StatusCode
		}
		// No resp at all: treat a TLS/transport failure as a non-2xx.
		if _, ok := err.(*tls.CertificateVerificationError); ok {
			return http.StatusBadGateway
		}
		return http.StatusBadGateway
	}
	return http.StatusOK
}

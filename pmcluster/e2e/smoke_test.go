//go:build e2e

// Package e2e contains smoke end-to-end tests for the pmcluster binary.
// These tests build the binary, exercise the full init → serve → API flow,
// and verify graceful shutdown.
//
// Run via: make e2e
//
//	(which executes: go test -timeout 10m -tags=e2e ./e2e/...)
//
// The test relies on the binary being buildable from source (it runs
// `go build` itself via TestMain), so no prior `make build` is required.
// If you want to skip the build step, set PMCLUSTER_BIN to an existing
// binary path.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// binaryPath is set by TestMain once the binary is built.
var binaryPath string

// edgeBinaryPath is set by TestMain once the cmd/edge binary is built.
var edgeBinaryPath string

// tokenLineRe matches the indented token line printed by pmcluster init / user create.
// The token is 43+ base64url chars on a line that starts with exactly 3 spaces.
var tokenLineRe = regexp.MustCompile(`^   ([A-Za-z0-9_-]{40,})$`)

func TestMain(m *testing.M) {
	// Allow overriding the binary via env (useful for local iteration).
	if p := os.Getenv("PMCLUSTER_BIN"); p != "" {
		binaryPath = p
	} else {
		// Build from source into a temp file.
		tmp, err := os.CreateTemp("", "pmcluster-e2e-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: create temp binary: %v\n", err)
			os.Exit(1)
		}
		tmp.Close()
		binaryPath = tmp.Name()

		// `go test` runs in pmcluster/e2e — go up one level so ./cmd/pmcluster
		// resolves correctly. Use a fresh exec.Cmd with Dir set explicitly.
		moduleRoot, err := findModuleRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: locate module root: %v\n", err)
			os.Exit(1)
		}
		buildCmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/pmcluster")
		buildCmd.Dir = moduleRoot
		out, err := buildCmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: go build failed: %v\n%s\n", err, out)
			os.Exit(1)
		}
	}

	// Build the combined edge binary the same way (used by edge_test.go).
	if p := os.Getenv("PMCLUSTER_EDGE_BIN"); p != "" {
		edgeBinaryPath = p
	} else {
		tmp, err := os.CreateTemp("", "pmcluster-edge-e2e-*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: create temp edge binary: %v\n", err)
			os.Exit(1)
		}
		tmp.Close()
		edgeBinaryPath = tmp.Name()

		moduleRoot, err := findModuleRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: locate module root: %v\n", err)
			os.Exit(1)
		}
		buildCmd := exec.Command("go", "build", "-o", edgeBinaryPath, "./cmd/edge")
		buildCmd.Dir = moduleRoot
		out, err := buildCmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e: edge go build failed: %v\n%s\n", err, out)
			os.Exit(1)
		}
	}

	code := m.Run()

	// Clean up built binaries only if we built them ourselves.
	if os.Getenv("PMCLUSTER_BIN") == "" {
		os.Remove(binaryPath)
	}
	if os.Getenv("PMCLUSTER_EDGE_BIN") == "" {
		os.Remove(edgeBinaryPath)
	}

	os.Exit(code)
}

// freePort grabs an ephemeral port, releases the listener, and returns the
// address. There is an inherent TOCTOU race but it is acceptable for a smoke
// test running on a dev/CI box.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// extractToken scans text for the indented token line and returns the token.
func extractToken(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if m := tokenLineRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	t.Fatalf("extractToken: no token found in output:\n%s", output)
	return ""
}

// runCmd runs a pmcluster subcommand with the given HOME dir and returns
// combined stdout+stderr as a string plus captured stdout separately.
func runCmd(t *testing.T, homeDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	cmd.Env = homeEnv(homeDir)

	err := cmd.Run()
	stdout = stdoutBuf.String()
	stderr = stderrBuf.String()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("cmd %v: unexpected error %v", args, err)
		}
	}
	return stdout, stderr, exitCode
}

// homeEnv returns an env slice with HOME set to homeDir, plus PATH and any
// DOCKER_* vars from the parent process so pmcluster subprocesses can find
// the Docker daemon (important on dev boxes where DOCKER_HOST points at a
// non-standard socket — e.g. colima, Docker Desktop, rootless setups).
func homeEnv(homeDir string) []string {
	env := []string{
		"HOME=" + homeDir,
		"PATH=" + os.Getenv("PATH"),
		// PMCLUSTER_LISTEN_ADDR is set per-process by callers that need it.
	}
	for _, k := range []string{
		"DOCKER_HOST",
		"DOCKER_TLS_VERIFY",
		"DOCKER_CERT_PATH",
		"DOCKER_API_VERSION",
		"DOCKER_CONTEXT",
	} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// waitHealthy polls GET /health until it returns 200 or the deadline elapses.
// It also returns early if the process has already died.
func waitHealthy(t *testing.T, addr string, proc *exec.Cmd, timeout time.Duration) {
	t.Helper()
	url := "http://" + addr + "/health"
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for time.Now().Before(deadline) {
		// Detect premature exit.
		if proc.ProcessState != nil && proc.ProcessState.Exited() {
			t.Fatalf("serve process exited prematurely before /health became ready")
		}

		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitHealthy: %s not ready after %s", url, timeout)
}

// TestSmoke is the single end-to-end smoke scenario.
func TestSmoke(t *testing.T) {
	homeDir := t.TempDir()

	// ── Step 1: pmcluster init ────────────────────────────────────────────────

	stdout, _, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d; stdout:\n%s", code, stdout)
	}
	adminToken := extractToken(t, stdout)
	t.Logf("admin token extracted (len=%d)", len(adminToken))

	// ── Step 2: start pmcluster serve ────────────────────────────────────────

	addr := freePort(t)

	var serveStdout bytes.Buffer
	serveCmd := exec.Command(binaryPath, "serve")
	serveCmd.Stdout = io.MultiWriter(&serveStdout, os.Stdout) // keep record + forward to test log
	serveCmd.Stderr = os.Stderr
	serveCmd.Env = append(homeEnv(homeDir), "PMCLUSTER_LISTEN_ADDR="+addr)

	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}

	// Ensure the serve process is always cleaned up, even on test failure.
	t.Cleanup(func() {
		if serveCmd.Process != nil {
			_ = serveCmd.Process.Signal(syscall.SIGTERM)
			// Give it a moment; if it doesn't exit, kill it.
			done := make(chan struct{})
			go func() {
				serveCmd.Wait() //nolint:errcheck
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				_ = serveCmd.Process.Kill()
			}
		}
	})

	waitHealthy(t, addr, serveCmd, 5*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + addr

	// ── Step 3: GET /health → 200 with JSON {status:"ok", version, commit} ──

	t.Run("health returns 200 and JSON fields", func(t *testing.T) {
		resp, err := client.Get(base + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status = %v, want ok", body["status"])
		}
		if _, ok := body["version"]; !ok {
			t.Error("missing field: version")
		}
		if _, ok := body["commit"]; !ok {
			t.Error("missing field: commit")
		}
	})

	// ── Step 4: GET /api/me without Authorization → 401 + WWW-Authenticate ──

	t.Run("api/me without token returns 401 with WWW-Authenticate", func(t *testing.T) {
		resp, err := client.Get(base + "/api/me")
		if err != nil {
			t.Fatalf("GET /api/me: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if !strings.Contains(wwwAuth, `Bearer realm="pmcluster"`) {
			t.Errorf("WWW-Authenticate = %q, want to contain Bearer realm", wwwAuth)
		}
	})

	// ── Step 5: GET /api/me with admin token → 200 {id:1, name:"admin"} ─────

	t.Run("api/me with admin token returns user", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/me: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if id, _ := body["id"].(float64); id != 1 {
			t.Errorf("id = %v, want 1", body["id"])
		}
		if body["name"] != "admin" {
			t.Errorf("name = %v, want admin", body["name"])
		}
	})

	// ── Step 6: GET /api/me with wrong token → 401 ───────────────────────────

	t.Run("api/me with wrong token returns 401", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer this-is-definitely-not-a-valid-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/me: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	// ── Step 7: pmcluster user create alice, then /api/me with alice's token ─

	t.Run("user create alice and authenticate", func(t *testing.T) {
		aliceOut, _, code := runCmd(t, homeDir, "user", "create", "alice")
		if code != 0 {
			t.Fatalf("user create alice exited %d; stdout:\n%s", code, aliceOut)
		}
		aliceToken := extractToken(t, aliceOut)
		t.Logf("alice token extracted (len=%d)", len(aliceToken))

		req, _ := http.NewRequest(http.MethodGet, base+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+aliceToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /api/me (alice): %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if id, _ := body["id"].(float64); id != 2 {
			t.Errorf("id = %v, want 2", body["id"])
		}
		if body["name"] != "alice" {
			t.Errorf("name = %v, want alice", body["name"])
		}
	})

	// ── Step 8: re-run pmcluster init → must refuse (exits non-zero) ─────────

	t.Run("re-running init is refused and mentions --force", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, homeDir, "init")
		if code == 0 {
			t.Fatal("expected non-zero exit from second pmcluster init, got 0")
		}
		combined := stdout + stderr
		if !strings.Contains(combined, "--force") {
			t.Errorf("expected mention of --force in output; got:\n%s", combined)
		}
	})

	// ── Step 9: SIGTERM → clean shutdown within 5 s ──────────────────────────

	t.Run("SIGTERM causes clean shutdown", func(t *testing.T) {
		if err := serveCmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("SIGTERM: %v", err)
		}

		done := make(chan error, 1)
		go func() { done <- serveCmd.Wait() }()

		select {
		case err := <-done:
			// A nil error or an ExitError with code 0 both indicate clean exit.
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					if exitErr.ExitCode() != 0 {
						t.Errorf("serve exited with code %d, want 0", exitErr.ExitCode())
					}
				} else {
					t.Errorf("serve Wait: %v", err)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("serve process did not exit within 5 s after SIGTERM")
		}

		out := serveStdout.String()
		if !strings.Contains(out, "stopped cleanly") {
			t.Errorf("expected 'stopped cleanly' in stdout; got:\n%s", out)
		}
	})
}

// TestSmoke_AliceToken exercises the sub-test separately so the table is readable.
// (Actual assertions are inlined above; this is a read-through of the token regex.)
func TestTokenRegex(t *testing.T) {
	cases := []struct {
		line  string
		want  string
		match bool
	}{
		{"   abc123DEF_-xyz_abc123DEF_-xyz_abc123DEF_-xy", "abc123DEF_-xyz_abc123DEF_-xyz_abc123DEF_-xy", true},
		{"  short", "", false},   // only 2 spaces
		{"    abc", "", false},   // 4 spaces (not 3)
		{"   abc", "", false},    // token too short
		{" token", "", false},    // 1 space
		{"no-indent", "", false}, // no indent
	}
	for _, tc := range cases {
		m := tokenLineRe.FindStringSubmatch(tc.line)
		if tc.match {
			if m == nil {
				t.Errorf("expected match for line %q", tc.line)
			} else if m[1] != tc.want {
				t.Errorf("captured %q, want %q", m[1], tc.want)
			}
		} else {
			if m != nil {
				t.Errorf("unexpected match for line %q: %q", tc.line, m[1])
			}
		}
	}
}

// findModuleRoot walks upward from the test working directory until it finds
// a go.mod, returning that directory. Test working dir is the package dir
// (pmcluster/e2e); the module root is one level up.
func findModuleRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("no go.mod found above %s", cwd)
}

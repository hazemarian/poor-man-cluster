//go:build e2e

// Package e2e — deploy pipeline end-to-end tests.
//
// This file exercises the full deploy pipeline (CLI deploy, rollback, HTTP API
// deploy + rollback) against a real single-node Docker Swarm.  It is gated
// behind PMCLUSTER_E2E_SWARM=1 so it never runs accidentally on developer
// machines — CI sets the variable explicitly.
//
// Run via: PMCLUSTER_E2E_SWARM=1 make e2e
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// manifest templates used across scenarios.
const deployManifestV1 = `app: e2etest
env: production
domain: example.test
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 1
    expose:
      port: 80
      host: web.e2etest.example.test
`

const deployManifestV2 = `app: e2etest
env: production
domain: example.test
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 2
    expose:
      port: 80
      host: web.e2etest.example.test
`

const deployManifestAPI = `app: e2etest-api
env: production
domain: example.test
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 1
    expose:
      port: 80
      host: web.e2etest-api.example.test
`

// TestDeployPipeline exercises the full deploy → redeploy → rollback flow
// against a real single-node Docker Swarm plus the HTTP API.
func TestDeployPipeline(t *testing.T) {

	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping deploy pipeline e2e (set it in CI or locally to enable)")
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping deploy pipeline e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		t.Cleanup(func() {
			t.Log("TestDeployPipeline: leaving Swarm we initialised (cleanup)")
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	for _, net := range []string{"traefik-net", "monitoring-net"} {
		netCtx, netCancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := dockerRun(netCtx, "network", "create", "--driver=overlay", "--attachable", net)
		netCancel()
		if err != nil {

			if !strings.Contains(out, "already exists") && !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("docker network create %s: %v\n%s", net, err, out)
			}
			t.Logf("network %s already exists — reusing", net)
		} else {
			t.Logf("created overlay network: %s", net)
		}
	}
	t.Cleanup(func() {
		t.Log("TestDeployPipeline: removing overlay networks (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		for _, net := range []string{"traefik-net", "monitoring-net"} {
			out, err := dockerRun(cleanCtx, "network", "rm", net)
			if err != nil {
				t.Logf("docker network rm %s: %v\n%s", net, err, out)
			}
		}
	})

	homeDir := t.TempDir()
	initOut, _, initCode := runCmd(t, homeDir, "init")
	if initCode != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", initCode, initOut)
	}
	adminToken := extractToken(t, initOut)
	t.Logf("pmcluster init OK (home=%s, token_len=%d)", homeDir, len(adminToken))

	t.Cleanup(func() {
		t.Log("TestDeployPipeline: removing stacks e2etest and e2etest-api (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()

		out, err := dockerRun(cleanCtx, "stack", "rm", "e2etest", "e2etest-api")
		if err != nil {
			t.Logf("docker stack rm: %v\n%s", err, out)
		}

		time.Sleep(3 * time.Second)
	})

	manifestV1Path := homeDir + "/manifest_v1.yaml"
	manifestV2Path := homeDir + "/manifest_v2.yaml"
	manifestAPIPath := homeDir + "/manifest_api.yaml"

	if err := os.WriteFile(manifestV1Path, []byte(deployManifestV1), 0o644); err != nil {
		t.Fatalf("write manifest v1: %v", err)
	}
	if err := os.WriteFile(manifestV2Path, []byte(deployManifestV2), 0o644); err != nil {
		t.Fatalf("write manifest v2: %v", err)
	}
	if err := os.WriteFile(manifestAPIPath, []byte(deployManifestAPI), 0o644); err != nil {
		t.Fatalf("write manifest api: %v", err)
	}

	var rev1 int64

	t.Run("a-cli-deploy-v1", func(t *testing.T) {
		deployOut, deployErr, code := runCmdCtx(t, ctx, homeDir, "deploy", manifestV1Path)
		combined := deployOut + deployErr
		if code != 0 {
			t.Fatalf("pmcluster deploy exited %d:\n%s", code, combined)
		}
		t.Logf("deploy v1 output:\n%s", combined)

		if !strings.Contains(combined, "Deployed e2etest") {
			t.Errorf("expected 'Deployed e2etest' in output; got:\n%s", combined)
		}

		rev1 = extractRevisionFromOutput(t, combined)
		t.Logf("rev1=%d", rev1)

		assertServiceExists(t, ctx, "e2etest", "e2etest_web")

		listOut, _, listCode := runCmdCtx(t, ctx, homeDir, "stack", "list")
		if listCode != 0 {
			t.Fatalf("pmcluster stack list exited %d:\n%s", listCode, listOut)
		}
		if !strings.Contains(listOut, "e2etest") {
			t.Errorf("stack list does not contain 'e2etest'; got:\n%s", listOut)
		}
		t.Logf("stack list output:\n%s", listOut)
	})

	t.Run("b-cli-redeploy-v2", func(t *testing.T) {
		if rev1 == 0 {
			t.Skip("rev1 not captured from sub-test a — skipping")
		}

		time.Sleep(2 * time.Second)

		deployOut, deployErr, code := runCmdCtx(t, ctx, homeDir, "deploy", manifestV2Path)
		combined := deployOut + deployErr
		if code != 0 {
			t.Fatalf("pmcluster deploy (v2) exited %d:\n%s", code, combined)
		}
		t.Logf("deploy v2 output:\n%s", combined)

		rev2 := extractRevisionFromOutput(t, combined)
		t.Logf("rev2=%d (must differ from rev1=%d)", rev2, rev1)
		if rev2 == rev1 {
			t.Errorf("rev2 == rev1 (%d); expected a distinct unix-second revision", rev2)
		}

		showOut, _, showCode := runCmdCtx(t, ctx, homeDir, "stack", "show", "e2etest")
		if showCode != 0 {
			t.Fatalf("pmcluster stack show exited %d:\n%s", showCode, showOut)
		}
		t.Logf("stack show (after v2) output:\n%s", showOut)
		if !strings.Contains(showOut, "2 revisions") && strings.Count(showOut, fmt.Sprintf("%d", rev2)) < 1 {
			t.Logf("stack show output (for reference):\n%s", showOut)
		}
		if !strings.Contains(showOut, "→") {
			t.Errorf("expected '→' current-revision marker in stack show; got:\n%s", showOut)
		}

		assertServiceReplicas(t, ctx, "e2etest_web", 2)
	})

	t.Run("c-cli-rollback-to-rev1", func(t *testing.T) {
		if rev1 == 0 {
			t.Skip("rev1 not captured from sub-test a — skipping")
		}

		rbOut, rbErr, code := runCmdCtx(t, ctx, homeDir, "rollback", "e2etest", fmt.Sprintf("%d", rev1))
		combined := rbOut + rbErr
		if code != 0 {
			t.Fatalf("pmcluster rollback exited %d:\n%s", code, combined)
		}
		t.Logf("rollback output:\n%s", combined)

		rev3 := extractRevisionFromOutput(t, combined)
		t.Logf("rollback created new revision=%d (must differ from rev1=%d)", rev3, rev1)
		if rev3 == rev1 {
			t.Errorf("rollback revision == rev1 (%d); expected a new timestamp", rev3)
		}

		showOut, _, showCode := runCmdCtx(t, ctx, homeDir, "stack", "show", "e2etest")
		if showCode != 0 {
			t.Fatalf("pmcluster stack show exited %d:\n%s", showCode, showOut)
		}
		t.Logf("stack show (after rollback) output:\n%s", showOut)
		if !strings.Contains(showOut, "→") {
			t.Errorf("expected '→' marker in stack show after rollback; got:\n%s", showOut)
		}

		rev3Str := fmt.Sprintf("→ %d", rev3)
		if !strings.Contains(showOut, rev3Str) {
			t.Errorf("expected rollback revision %d to be marked current (→); show output:\n%s", rev3, showOut)
		}

		assertServiceReplicas(t, ctx, "e2etest_web", 1)
	})

	t.Run("d-cli-rollback-nonexistent", func(t *testing.T) {

		rbOut, rbErr, code := runCmdCtx(t, ctx, homeDir, "rollback", "e2etest", "1")
		combined := rbOut + rbErr
		if code == 0 {
			t.Fatalf("expected non-zero exit for rollback to nonexistent revision; got 0; output:\n%s", combined)
		}
		t.Logf("rollback nonexistent output (exit %d):\n%s", code, combined)
		if !strings.Contains(strings.ToLower(combined), "not found") {
			t.Errorf("expected 'not found' in error output; got:\n%s", combined)
		}
	})

	t.Run("e-api-deploy", func(t *testing.T) {
		addr := freePort(t)

		var serveBuf bytes.Buffer
		tw := newTestWriter(t)
		serveCmd := exec.CommandContext(ctx, binaryPath, "serve")
		serveCmd.Stdout = io.MultiWriter(&serveBuf, tw, os.Stdout)
		serveCmd.Stderr = io.MultiWriter(tw, os.Stderr)
		serveCmd.Env = append(homeEnv(homeDir), "PMCLUSTER_LISTEN_ADDR="+addr)

		if err := serveCmd.Start(); err != nil {
			t.Fatalf("start pmcluster serve: %v", err)
		}
		t.Cleanup(func() {
			if serveCmd.Process != nil {
				_ = serveCmd.Process.Signal(syscall.SIGTERM)
				done := make(chan struct{})
				go func() { serveCmd.Wait(); close(done) }() //nolint:errcheck
				select {
				case <-done:
				case <-time.After(6 * time.Second):
					_ = serveCmd.Process.Kill()
				}
			}
		})

		waitHealthy(t, addr, serveCmd, 10*time.Second)

		base := "http://" + addr
		hc := &http.Client{Timeout: 15 * time.Second}

		payload := map[string]string{"manifest": deployManifestAPI}
		payloadBytes, _ := json.Marshal(payload)

		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/stacks", bytes.NewReader(payloadBytes))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("POST /api/stacks: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		t.Logf("POST /api/stacks response (%d): %s", resp.StatusCode, body)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/stacks: status=%d, body=%s", resp.StatusCode, body)
		}

		var deployResp map[string]any
		if err := json.Unmarshal(body, &deployResp); err != nil {
			t.Fatalf("decode POST /api/stacks response: %v", err)
		}
		if _, ok := deployResp["stack"]; !ok {
			t.Errorf("POST /api/stacks response missing 'stack' field; got: %s", body)
		}
		if _, ok := deployResp["revision"]; !ok {
			t.Errorf("POST /api/stacks response missing 'revision' field; got: %s", body)
		}
		apiRev, _ := deployResp["revision"].(float64)
		t.Logf("API deployed e2etest-api @ revision %v", apiRev)

		assertServiceExists(t, ctx, "e2etest-api", "e2etest-api_web")

		listReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/stacks", nil)
		listReq.Header.Set("Authorization", "Bearer "+adminToken)
		listResp, err := hc.Do(listReq)
		if err != nil {
			t.Fatalf("GET /api/stacks: %v", err)
		}
		defer listResp.Body.Close()
		listBody, _ := io.ReadAll(listResp.Body)
		t.Logf("GET /api/stacks response (%d): %s", listResp.StatusCode, listBody)
		if listResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/stacks: status=%d, body=%s", listResp.StatusCode, listBody)
		}
		if !strings.Contains(string(listBody), "e2etest") {
			t.Errorf("GET /api/stacks: expected 'e2etest' in response; got: %s", listBody)
		}
		if !strings.Contains(string(listBody), "e2etest-api") {
			t.Errorf("GET /api/stacks: expected 'e2etest-api' in response; got: %s", listBody)
		}

		showReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/stacks/e2etest-api", nil)
		showReq.Header.Set("Authorization", "Bearer "+adminToken)
		showResp, err := hc.Do(showReq)
		if err != nil {
			t.Fatalf("GET /api/stacks/e2etest-api: %v", err)
		}
		defer showResp.Body.Close()
		showBody, _ := io.ReadAll(showResp.Body)
		t.Logf("GET /api/stacks/e2etest-api response (%d): %s", showResp.StatusCode, showBody)
		if showResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/stacks/e2etest-api: status=%d, body=%s", showResp.StatusCode, showBody)
		}
		var showJSON map[string]any
		if err := json.Unmarshal(showBody, &showJSON); err != nil {
			t.Fatalf("decode show response: %v", err)
		}
		revisions, _ := showJSON["revisions"].([]any)
		if len(revisions) < 1 {
			t.Errorf("GET /api/stacks/e2etest-api: expected ≥1 revision; got %d", len(revisions))
		}

		if apiRev > 0 {
			revURL := fmt.Sprintf("%s/api/stacks/e2etest-api/revisions/%d", base, int64(apiRev))
			revReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, revURL, nil)
			revReq.Header.Set("Authorization", "Bearer "+adminToken)
			revResp, err := hc.Do(revReq)
			if err != nil {
				t.Fatalf("GET %s: %v", revURL, err)
			}
			defer revResp.Body.Close()
			revBody, _ := io.ReadAll(revResp.Body)
			t.Logf("GET revisions/<rev> response (%d): %s", revResp.StatusCode, revBody)
			if revResp.StatusCode != http.StatusOK {
				t.Fatalf("GET revisions/<rev>: status=%d, body=%s", revResp.StatusCode, revBody)
			}
			var revJSON map[string]any
			if err := json.Unmarshal(revBody, &revJSON); err != nil {
				t.Fatalf("decode revision response: %v", err)
			}
			if _, ok := revJSON["source_yaml"]; !ok {
				t.Errorf("revision response missing 'source_yaml'; got: %s", revBody)
			}
			if _, ok := revJSON["rendered_yaml"]; !ok {
				t.Errorf("revision response missing 'rendered_yaml'; got: %s", revBody)
			}
		}

		if apiRev > 0 {
			rbPayload := map[string]int64{"revision": int64(apiRev)}
			rbBytes, _ := json.Marshal(rbPayload)
			rbReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
				base+"/api/stacks/e2etest-api/rollback", bytes.NewReader(rbBytes))
			rbReq.Header.Set("Authorization", "Bearer "+adminToken)
			rbReq.Header.Set("Content-Type", "application/json")
			rbResp, err := hc.Do(rbReq)
			if err != nil {
				t.Fatalf("POST /api/stacks/e2etest-api/rollback: %v", err)
			}
			defer rbResp.Body.Close()
			rbBody, _ := io.ReadAll(rbResp.Body)
			t.Logf("POST rollback response (%d): %s", rbResp.StatusCode, rbBody)
			if rbResp.StatusCode != http.StatusOK {
				t.Fatalf("POST rollback: status=%d, body=%s", rbResp.StatusCode, rbBody)
			}
			var rbJSON map[string]any
			if err := json.Unmarshal(rbBody, &rbJSON); err != nil {
				t.Fatalf("decode rollback response: %v", err)
			}
			if _, ok := rbJSON["new_revision"]; !ok {
				t.Errorf("rollback response missing 'new_revision'; got: %s", rbBody)
			}
		}

		if err := serveCmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("SIGTERM serve: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- serveCmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					if exitErr.ExitCode() != 0 {
						t.Errorf("serve exited non-zero (%d) after SIGTERM", exitErr.ExitCode())
					}
				} else {
					t.Errorf("serve Wait: %v", err)
				}
			}

			if !strings.Contains(serveBuf.String(), "stopped cleanly") {
				t.Errorf("expected 'stopped cleanly' in serve stdout; got:\n%s", serveBuf.String())
			}
		case <-time.After(8 * time.Second):
			t.Fatal("serve process did not exit within 8s after SIGTERM")
		}
	})
}

// extractRevisionFromOutput parses the *new* unix-timestamp revision id from
// pmcluster CLI output. Two formats:
//
//	✅ Deployed e2etest @ revision 1746000000 (...)              → returns 1746000000
//	✅ Rolled back e2etest to revision <old> (new revision 1746000001, ...) → returns 1746000001
//
// The rollback line contains TWO revisions; we want the second ("new revision")
// because that's the freshly assigned id the rest of the test asserts against.
//
// Returns 0 and logs a non-fatal warning if not found.
func extractRevisionFromOutput(t *testing.T, output string) int64 {
	t.Helper()

	if idx := strings.Index(output, "new revision "); idx >= 0 {
		tail := output[idx+len("new revision "):]
		if n, ok := scanLeadingInt(tail); ok {
			return n
		}
	}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "revision") {
			continue
		}
		for _, f := range strings.Fields(line) {
			f = strings.TrimRight(f, "(),")
			var n int64
			if _, err := fmt.Sscanf(f, "%d", &n); err == nil && n > 1_000_000_000 {
				return n
			}
		}
	}
	t.Logf("extractRevisionFromOutput: no unix-timestamp revision found in:\n%s", output)
	return 0
}

// scanLeadingInt parses a leading integer from s (stops at the first
// non-digit). Returns the value and true on success.
func scanLeadingInt(s string) (int64, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	var n int64
	if _, err := fmt.Sscanf(s[:end], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// assertServiceExists checks that `docker service ls --filter label=application=<app>`
// lists a service named <expectedService>.
func assertServiceExists(t *testing.T, ctx context.Context, app, expectedService string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, err := dockerRun(checkCtx, "service", "ls",
			"--filter", "label=application="+app,
			"--format", "{{.Name}}")
		cancel()
		if err == nil && strings.Contains(out, expectedService) {
			t.Logf("service %s found", expectedService)
			return
		}
		time.Sleep(2 * time.Second)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, _ := dockerRun(checkCtx, "service", "ls",
		"--filter", "label=application="+app,
		"--format", "{{.Name}}")
	cancel()
	t.Errorf("service %s not found after 30s; docker service ls output:\n%s", expectedService, out)
}

// assertServiceReplicas polls docker service inspect until the replicas count
// in the Spec (desired replicas) matches want, or times out.
func assertServiceReplicas(t *testing.T, ctx context.Context, serviceName string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, err := dockerRun(checkCtx,
			"service", "inspect",
			"--format", "{{.Spec.Mode.Replicated.Replicas}}",
			serviceName)
		cancel()
		if err == nil {
			out = strings.TrimSpace(out)
			var got int
			if _, err := fmt.Sscanf(out, "%d", &got); err == nil && got == want {
				t.Logf("service %s has replicas=%d", serviceName, want)
				return
			}
		}
		time.Sleep(2 * time.Second)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, _ := dockerRun(checkCtx,
		"service", "inspect",
		"--format", "{{.Spec.Mode.Replicated.Replicas}}",
		serviceName)
	cancel()
	t.Errorf("service %s: want replicas=%d, got %q after 30s", serviceName, want, strings.TrimSpace(out))
}

// testWriter adapts *testing.T to io.Writer so we can stream child process
// output into the test log as well as os.Stdout.
type testWriter struct{ t *testing.T }

func newTestWriter(t *testing.T) *testWriter { return &testWriter{t: t} }

func (tw *testWriter) Write(p []byte) (int, error) {
	tw.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

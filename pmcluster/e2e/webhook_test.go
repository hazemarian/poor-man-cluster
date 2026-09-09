//go:build e2e

// Package e2e — webhook end-to-end tests.
//
// This file exercises the full webhook flow against a real Docker Swarm:
//
//	pmcluster init → pmcluster webhook add → pmcluster serve → POST signed payload
//	→ assert stack deployed → verify last_used_at updated → bad-sig / bad-source 401s.
//
// Gated by PMCLUSTER_E2E_SWARM=1. Run via: PMCLUSTER_E2E_SWARM=1 make e2e
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// secretLineRe matches the indented hex secret line printed by `pmcluster webhook add`.
// The secret is at least 60 lowercase hex characters, indented by 3 spaces.
var secretLineRe = regexp.MustCompile(`^   ([a-f0-9]{60,})$`)

// webhookManifest is a minimal valid DSL manifest for a "whoami-webhook" stack.
const webhookManifest = `app: whoami-webhook
env: production
domain: example.test
services:
  web:
    image: traefik/whoami:v1.10
    replicas: 1
    expose:
      port: 80
      host: web.whoami-webhook.example.test
`

// extractWebhookSecret scans output for the indented hex secret line and returns it.
func extractWebhookSecret(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if m := secretLineRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	t.Fatalf("extractWebhookSecret: no secret found in output:\n%s", output)
	return ""
}

// signPayload computes the HMAC-SHA256 signature for the given body using the
// secret as printed by `pmcluster webhook add`. The CLI stores the hex string
// itself (not the decoded bytes) as the HMAC key — so operators can paste the
// printed value verbatim into their CI's secret box and the openssl example
// (`openssl dgst -sha256 -hmac "$SECRET"`) just works. We mirror that here:
// the hex string IS the HMAC key, no decode step.
func signPayload(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postWebhook sends a signed POST to /webhook/{source} and returns the response.
func postWebhook(t *testing.T, baseURL, source, sig string, body []byte) *http.Response {
	t.Helper()
	url := baseURL + "/webhook/" + source
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("postWebhook: NewRequest: %v", err)
	}
	if sig != "" {
		req.Header.Set("X-Pmcluster-Signature", sig)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("postWebhook: Do: %v", err)
	}
	return resp
}

// TestWebhookE2E exercises the full webhook flow against a real Docker Swarm.
func TestWebhookE2E(t *testing.T) {
	// ── Guard: env var ────────────────────────────────────────────────────────
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping webhook e2e")
	}
	// ── Guard: docker binary ──────────────────────────────────────────────────
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping webhook e2e")
	}

	// Cap the whole test to 5 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ── Swarm bootstrap ───────────────────────────────────────────────────────
	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		t.Cleanup(func() {
			t.Log("TestWebhookE2E: leaving Swarm we initialised (cleanup)")
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	// ── Ensure shared overlay networks exist (normally created by cluster up) ─
	for _, net := range []string{"traefik-net", "monitoring-net"} {
		netCtx, netCancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := dockerRun(netCtx, "network", "create", "--driver=overlay", "--attachable", net)
		netCancel()
		if err != nil && !strings.Contains(out, "already exists") {
			t.Fatalf("docker network create %s: %v\n%s", net, err, out)
		}
	}
	t.Cleanup(func() {
		for _, net := range []string{"traefik-net", "monitoring-net"} {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = dockerRun(cleanCtx, "network", "rm", net)
			cleanCancel()
		}
	})

	// ── Step 1: pmcluster init ────────────────────────────────────────────────
	homeDir := t.TempDir()
	initOut, _, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", code, initOut)
	}
	t.Logf("pmcluster init OK (home=%s)", homeDir)

	// ── Step 2: pmcluster webhook add github-prod ─────────────────────────────
	webhookOut, _, code := runCmd(t, homeDir, "webhook", "add", "github-prod")
	if code != 0 {
		t.Fatalf("pmcluster webhook add exited %d:\n%s", code, webhookOut)
	}
	hexSecret := extractWebhookSecret(t, webhookOut)
	t.Logf("webhook secret extracted (len=%d)", len(hexSecret))

	// ── Step 3: start pmcluster serve ────────────────────────────────────────
	addr := freePort(t)
	serveCmd := exec.Command(binaryPath, "serve")
	serveCmd.Stdout = io.MultiWriter(os.Stdout)
	serveCmd.Stderr = os.Stderr
	serveCmd.Env = append(homeEnv(homeDir), "PMCLUSTER_LISTEN_ADDR="+addr)
	if err := serveCmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		if serveCmd.Process != nil {
			_ = serveCmd.Process.Signal(syscall.SIGTERM)
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

	waitHealthy(t, addr, serveCmd, 10*time.Second)
	baseURL := "http://" + addr
	t.Logf("pmcluster serve ready at %s", baseURL)

	// ── Step 4: build a signed valid payload ─────────────────────────────────
	payload := map[string]any{
		"manifest": webhookManifest,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal payload: %v", err)
	}
	validSig := signPayload(t, hexSecret, payloadBytes)

	// ── Step 5: POST with valid HMAC → 200 + JSON {stack, revision} ──────────
	t.Run("valid HMAC succeeds — 200 and stack+revision in response", func(t *testing.T) {
		resp := postWebhook(t, baseURL, "github-prod", validSig, payloadBytes)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
		}

		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if _, ok := result["stack"]; !ok {
			t.Error("response missing 'stack' field")
		}
		if _, ok := result["revision"]; !ok {
			t.Error("response missing 'revision' field")
		}
		if result["stack"] != "whoami-webhook" {
			t.Errorf("stack = %v, want 'whoami-webhook'", result["stack"])
		}
		t.Logf("deploy response: stack=%v revision=%v", result["stack"], result["revision"])
	})

	// Allow swarm a moment to settle.
	time.Sleep(3 * time.Second)

	// ── Step 6: assert docker service ls shows the deployed service ───────────
	t.Run("docker service ls shows whoami-webhook_web", func(t *testing.T) {
		svcCtx, svcCancel := context.WithTimeout(ctx, 30*time.Second)
		defer svcCancel()
		out, err := dockerRun(svcCtx, "service", "ls", "--format", "{{.Name}}")
		if err != nil {
			t.Fatalf("docker service ls: %v\n%s", err, out)
		}
		if !strings.Contains(out, "whoami-webhook") {
			t.Errorf("expected 'whoami-webhook' service in docker service ls; got:\n%s", out)
		}
	})

	// ── Step 7: POST with wrong signature → 401 ───────────────────────────────
	t.Run("wrong signature → 401", func(t *testing.T) {
		wrongSig := "sha256=" + strings.Repeat("00", 32)
		resp := postWebhook(t, baseURL, "github-prod", wrongSig, payloadBytes)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, body)
		}
	})

	// ── Step 8: POST to non-existent source with valid signature for github-prod → 401 ──
	t.Run("unknown source → 401", func(t *testing.T) {
		// The signature is valid for github-prod but the source name is wrong.
		resp := postWebhook(t, baseURL, "never-existed", validSig, payloadBytes)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, body)
		}
	})

	// ── Step 9: pmcluster webhook list → last_used_at not "—" ────────────────
	t.Run("webhook list shows last_used_at is set after successful POST", func(t *testing.T) {
		listOut, _, code := runCmd(t, homeDir, "webhook", "list")
		if code != 0 {
			t.Fatalf("pmcluster webhook list exited %d:\n%s", code, listOut)
		}
		t.Logf("webhook list output:\n%s", listOut)

		// The list shows github-prod. After a successful POST, last_used_at
		// should NOT be "—" (which is what the CLI prints for NULL).
		lines := strings.Split(listOut, "\n")
		found := false
		for _, line := range lines {
			if strings.Contains(line, "github-prod") {
				found = true
				if strings.Contains(line, "—") || strings.Contains(line, "-") {
					// If the line still contains the null placeholder, that's a problem.
					// However the CLI may use "—" (em dash) for NULL. We check for
					// a non-null timestamp format: at least 4 consecutive digits.
					hasTimestamp := false
					for i := 0; i+3 < len(line); i++ {
						if line[i] >= '0' && line[i] <= '9' &&
							line[i+1] >= '0' && line[i+1] <= '9' &&
							line[i+2] >= '0' && line[i+2] <= '9' &&
							line[i+3] >= '0' && line[i+3] <= '9' {
							hasTimestamp = true
							break
						}
					}
					if !hasTimestamp {
						t.Errorf("github-prod row in webhook list does not show a timestamp for last_used_at; line: %q", line)
					}
				}
				break
			}
		}
		if !found {
			t.Errorf("github-prod not found in webhook list output:\n%s", listOut)
		}
	})

	// ── Cleanup: remove the deployed stack ───────────────────────────────────
	t.Cleanup(func() {
		t.Log("TestWebhookE2E: removing whoami-webhook stack (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanCancel()
		out, err := dockerRun(cleanCtx, "stack", "rm", "whoami-webhook")
		if err != nil {
			t.Logf("docker stack rm whoami-webhook: %v\n%s", err, out)
		}
	})
}

// Ensure fmt is used (avoids unused import compile error if something changes).
var _ = fmt.Sprintf

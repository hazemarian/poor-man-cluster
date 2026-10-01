//go:build e2e

// Package e2e contains end-to-end tests for the pmcluster binary.
// This file covers three deployment topologies that the cluster-focused
// tests do not exercise:
//
//  1. TestSetupWizardNoSSO — the full `pmcluster setup` wizard → `cluster up`
//     flow with SSO DISABLED (swarm-gated). Verifies the sso stack is NOT
//     deployed, the Traefik dynamic config keeps the admin-auth basicAuth
//     middleware (no sso-auth forwardAuth), no sso_cookie_secret credential
//     exists, and a re-run hands off to `cluster update`.
//
//  2. TestRemoteCLIStandalone — the CLI running on a "user machine" (a HOME
//     with no cluster state) driving a real daemon over the network via
//     PMCLUSTER_API_URL + PMCLUSTER_API_TOKEN (remote mode). Verifies stack
//     list / cluster settings / usage / webhook deliveries work remotely.
//
//  3. TestEdgeOutsideCluster — the edge binary (operator console + reverse
//     proxy) running OUTSIDE the swarm, pointed at a real standalone daemon
//     via UPSTREAM. Verifies the console serves locally and daemon API calls
//     are proxied correctly.
//
// Tier A tests (2,3) always run; tier B (1) requires PMCLUSTER_E2E_SWARM=1.
package e2e

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSetupWizardNoSSO drives the full wizard → cluster up flow with SSO
// disabled against a real single-node Swarm. The mirror of
// TestSetupWizardClusterUp: the sso stack must NOT deploy, the Traefik gate
// must stay admin-auth (basicAuth), and no sso_cookie_secret may exist.
func TestSetupWizardNoSSO(t *testing.T) {
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping setup wizard no-SSO swarm e2e")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping setup wizard no-SSO swarm e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		t.Cleanup(func() {
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			if out, err := dockerRun(leaveCtx, "swarm", "leave", "--force"); err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	homeDir := t.TempDir()
	stdout, stderr, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}

	// Disable the control loop so the auto-started daemon does not race the
	// setup/cluster up assertions below.
	setReconcileInterval(t, homeDir, "0")

	certPath, keyPath := generateSelfSignedCert(t, homeDir)

	setupArgs := []string{
		"setup",
		"--domain", "example.test",
		"--cert", certPath,
		"--key", keyPath,
		"--openobserve-email", "admin@example.test",
		"--traefik-admin-user", "admin",
	}

	t.Log("Running `pmcluster setup` WITHOUT --sso-enabled (expects handoff to cluster up)")
	stdout, stderr, code = runCmd(t, homeDir, setupArgs...)
	if code != 0 {
		t.Fatalf("setup on fresh cluster exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Fresh install — running `pmcluster cluster up`") {
		t.Fatalf("expected 'Fresh install' handoff message.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stdout, "cluster up complete") {
		t.Fatalf("expected 'cluster up complete'.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// The sso stack must NOT be deployed.
	t.Log("Verifying sso stack is NOT deployed")
	stacksOut, stacksErr := dockerRun(ctx, "stack", "ls", "--format", "{{.Name}}")
	if stacksErr != nil {
		t.Fatalf("docker stack ls: %v", stacksErr)
	}
	if strings.Contains(stacksOut, "sso") {
		t.Fatalf("expected NO sso stack with SSO disabled, got:\n%s", stacksOut)
	}

	// Rendered Traefik dynamic config must keep admin-auth (basicAuth).
	t.Log("Verifying rendered Traefik dynamic config uses admin-auth")
	traefikCfg := dockerConfigIDByPrefix(t, ctx, "pmcluster_traefik_dynamic_v")
	if traefikCfg == "" {
		t.Fatal("pmcluster_traefik_dynamic_v* docker config not found")
	}
	dynOut := mustDockerRun(t, ctx, "config", "inspect", "--format", "{{json .Spec.Data}}", traefikCfg)
	if !strings.Contains(dynOut, "admin-auth") {
		t.Fatalf("expected admin-auth middleware in rendered traefik dynamic config:\n%s", dynOut)
	}
	if strings.Contains(dynOut, "sso-auth") {
		t.Fatalf("SSO disabled but sso-auth forwardAuth middleware present:\n%s", dynOut)
	}
	if strings.Contains(dynOut, "sso_oauth2-proxy") {
		t.Fatalf("SSO disabled but oauth2-proxy address referenced:\n%s", dynOut)
	}

	// No sso_cookie_secret credential.
	t.Log("Verifying sso_cookie_secret credential is absent")
	credsOut, _, credsCode := runCmd(t, homeDir, "credentials", "list")
	if credsCode != 0 {
		t.Fatalf("credentials list exited %d:\n%s", credsCode, credsOut)
	}
	if strings.Contains(credsOut, "sso_cookie_secret") {
		t.Fatalf("sso_cookie_secret present with SSO disabled:\n%s", credsOut)
	}

	// Re-running setup hands off to cluster update.
	t.Log("Re-running `pmcluster setup` on existing cluster (expects handoff to cluster update)")
	stdout, stderr, code = runCmd(t, homeDir, setupArgs...)
	if code != 0 {
		t.Fatalf("setup on existing cluster exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Cluster already initialised — running `pmcluster cluster update`") {
		t.Fatalf("expected 'Cluster already initialised' handoff message.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// Cleanup.
	t.Log("Tearing down cluster (cluster down --yes --purge)")
	stdout, stderr, code = runCmd(t, homeDir, "cluster", "down", "--yes", "--purge")
	if code != 0 {
		t.Fatalf("cluster down exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	finalLs, _ := dockerRunNoFail(ctx, "stack", "ls", "--format", "{{.Name}}")
	if strings.Contains(finalLs, "infra") || strings.Contains(finalLs, "sso") {
		t.Fatalf("expected infra/sso stacks removed after cluster down, got:\n%s", finalLs)
	}
}

// startStandaloneDaemon boots a real `pmcluster serve` on a free port using a
// fresh HOME, and returns its address plus a cleanup func. The daemon runs in
// standalone mode (Docker unreachable is fine — /api/cluster/info is simply
// disabled). Returns the token for remote-mode calls.
func startStandaloneDaemon(t *testing.T) (addr, token string) {
	t.Helper()

	daemonHome := t.TempDir()
	stdout, stderr, code := runCmd(t, daemonHome, "init")
	if code != 0 {
		t.Fatalf("daemon init exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	token = extractToken(t, stdout)

	addr = freePort(t)
	daemon := exec.Command(binaryPath, "serve")
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	daemon.Env = append(homeEnv(daemonHome),
		"PMCLUSTER_LISTEN_ADDR="+addr,
	)
	if err := daemon.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if daemon.Process != nil {
			_ = daemon.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() {
				daemon.Wait() //nolint:errcheck
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				_ = daemon.Process.Kill()
			}
		}
	})

	waitHealthy(t, addr, daemon, 10*time.Second)
	return addr, token
}

// TestRemoteCLIStandalone drives a locally-installed CLI (a HOME with no
// cluster state, exactly like an operator's laptop) against a real daemon
// over the network. This exercises the remote-mode switchpoint: the CLI must
// NOT fall back to a local store; every command goes through the daemon API.
func TestRemoteCLIStandalone(t *testing.T) {
	addr, token := startStandaloneDaemon(t)

	// A second HOME — the "user machine" with no cluster state.
	userHome := t.TempDir()
	remoteEnv := append(homeEnv(userHome),
		"PMCLUSTER_API_URL=http://"+addr,
		"PMCLUSTER_API_TOKEN="+token,
	)

	runRemote := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binaryPath, args...)
		cmd.Env = remoteEnv
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				t.Fatalf("remote cmd %v: %v", args, err)
			}
		}
		return string(out), code
	}

	t.Run("stack list works remotely", func(t *testing.T) {
		out, code := runRemote("stack", "list")
		if code != 0 {
			t.Fatalf("remote stack list exited %d:\n%s", code, out)
		}
		// A fresh standalone daemon has no stacks — the empty state still
		// proves remote mode reached the daemon (a local-only CLI would have
		// failed with "data directory not initialised").
		if !strings.Contains(out, "no stacks yet") && !strings.Contains(out, "NAME") {
			t.Fatalf("unexpected remote stack list output:\n%s", out)
		}
	})

	t.Run("cluster settings works remotely", func(t *testing.T) {
		out, code := runRemote("cluster", "settings")
		if code != 0 {
			t.Fatalf("remote cluster settings exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "KEY") || !strings.Contains(out, "VALUE") {
			t.Fatalf("remote cluster settings missing table header:\n%s", out)
		}
	})

	t.Run("usage works remotely", func(t *testing.T) {
		out, code := runRemote("usage")
		if code != 0 {
			t.Fatalf("remote usage exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "CONFIG") || !strings.Contains(out, "SECRET") {
			t.Fatalf("remote usage missing section headers:\n%s", out)
		}
	})

	t.Run("webhook deliveries works remotely", func(t *testing.T) {
		out, code := runRemote("webhook", "deliveries", "github-prod", "--limit", "5")
		if code != 0 {
			t.Fatalf("remote webhook deliveries exited %d:\n%s", code, out)
		}
		// Empty delivery history is fine — the command must still succeed and
		// prove remote mode (no local store).
		if !strings.Contains(out, "no deliveries recorded") && !strings.Contains(out, "STATUS") {
			t.Fatalf("unexpected remote webhook deliveries output:\n%s", out)
		}
	})

	t.Run("bad token is rejected", func(t *testing.T) {
		cmd := exec.Command(binaryPath, "stack", "list")
		cmd.Env = append(homeEnv(userHome),
			"PMCLUSTER_API_URL=http://"+addr,
			"PMCLUSTER_API_TOKEN=pmc_bad_token_that_is_long_enough_xxxx",
		)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected auth failure with a bad token, got success:\n%s", out)
		}
		if !strings.Contains(string(out), "401") {
			t.Fatalf("expected 401 in output, got:\n%s", out)
		}
	})
}

// TestEdgeOutsideCluster boots a real daemon and the real edge binary next to
// it, with the edge pointed at the daemon via UPSTREAM — the deployment shape
// of the operator console running on a machine that is NOT part of the swarm.
func TestEdgeOutsideCluster(t *testing.T) {
	addr, _ := startStandaloneDaemon(t)

	dataDir := t.TempDir()
	edgeAddr := freePort(t)

	edge := exec.Command(edgeBinaryPath)
	edge.Stdout = os.Stdout
	edge.Stderr = os.Stderr
	edge.Env = append(os.Environ(),
		"LISTEN_ADDR="+edgeAddr,
		"UPSTREAM=http://"+addr,
		"DATA_DIR="+dataDir,
		"PMCLUSTER_UI_SECRET=0123456789abcdef0123456789abcdef",
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

	waitHealthy(t, edgeAddr, edge, 10*time.Second)

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	base := "http://" + edgeAddr

	t.Run("local /healthz answered by the edge itself", func(t *testing.T) {
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
	})

	t.Run("daemon /health proxied through the edge", func(t *testing.T) {
		resp, err := client.Get(base + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /health status = %d, want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), `"status":"ok"`) {
			t.Errorf("GET /health body missing daemon status ok; got: %s", body)
		}
	})

	t.Run("console shell renders under /web/", func(t *testing.T) {
		resp, err := client.Get(base + "/web/")
		if err != nil {
			t.Fatalf("GET /web/: %v", err)
		}
		defer resp.Body.Close()
		// First run redirects to /setup; the console is served regardless.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
			t.Fatalf("GET /web/ status = %d, want 200 or 302", resp.StatusCode)
		}
	})

	t.Run("daemon API /api/stacks proxied (unauthorised → 401 from daemon)", func(t *testing.T) {
		resp, err := client.Get(base + "/api/stacks")
		if err != nil {
			t.Fatalf("GET /api/stacks: %v", err)
		}
		defer resp.Body.Close()
		// No Bearer token attached by the raw client → the real daemon's
		// auth middleware must reject with 401 (proving the request actually
		// reached the daemon rather than being answered locally).
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /api/stacks status = %d, want 401 (daemon auth)", resp.StatusCode)
		}
	})
}

//go:build e2e

// Package e2e contains end-to-end tests for the pmcluster binary.
// This file tests the interactive setup wizard (`pmcluster setup`) and its
// fallback from `cluster up`/`cluster update` when no provisioning data is
// present, plus the SSO feature-flag path (oauth2-proxy sidecar stack).
//
// Tier A (always runs): fallback + validation — fresh HOME, `cluster up` and
// `cluster update` with no data must fall back to the wizard and produce the
// wizard's validation errors in non-interactive mode; `setup` must reject
// missing domain / TLS mode / SSO client credentials.
//
// Tier B (PMCLUSTER_E2E_SWARM=1): full wizard → cluster up with
// --sso-enabled against a real single-node Swarm; verifies the sso stack
// deploys (sso_oauth2-proxy), the sso_cookie_secret credential exists, the
// rendered Traefik dynamic config carries the sso-auth forwardAuth middleware
// (not admin-auth), and a re-run of `setup` on the existing cluster hands off
// to `cluster update`.
package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSetupWizardFallback covers the non-swarm fallback + validation tier.
func TestSetupWizardFallback(t *testing.T) {
	homeDir := t.TempDir()

	// `pmcluster init` first — the wizard (and up/update fallback) requires
	// an initialised data directory.
	stdout, stderr, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	t.Logf("pmcluster init OK (home=%s)", homeDir)

	t.Run("cluster up without data falls back to wizard", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, homeDir, "cluster", "up")
		if code == 0 {
			t.Fatalf("cluster up with no data exited 0; want non-zero (falls into wizard, non-TTY)")
		}
		if !strings.Contains(stdout, "No cluster configuration found — starting the interactive setup wizard") &&
			!strings.Contains(stderr, "No cluster configuration found — starting the interactive setup wizard") {
			t.Fatalf("cluster up fallback message missing.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
		if !strings.Contains(stdout, "domain is required (interactive prompt or --domain)") &&
			!strings.Contains(stderr, "domain is required (interactive prompt or --domain)") {
			t.Fatalf("expected non-TTY wizard 'domain is required' error.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})

	t.Run("cluster update without data falls back to wizard", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, homeDir, "cluster", "update")
		if code == 0 {
			t.Fatalf("cluster update with no data exited 0; want non-zero")
		}
		if !strings.Contains(stdout, "No cluster configuration found — starting the interactive setup wizard") &&
			!strings.Contains(stderr, "No cluster configuration found — starting the interactive setup wizard") {
			t.Fatalf("cluster update fallback message missing.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})

	t.Run("setup without domain fails validation", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, homeDir, "setup", "--openobserve-email", "admin@example.test")
		if code == 0 {
			t.Fatalf("setup without domain exited 0; want non-zero")
		}
		if !strings.Contains(stdout, "domain is required") && !strings.Contains(stderr, "domain is required") {
			t.Fatalf("expected 'domain is required'.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})

	t.Run("setup without TLS mode fails validation", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, homeDir, "setup",
			"--domain", "example.test",
			"--openobserve-email", "admin@example.test")
		if code == 0 {
			t.Fatalf("setup without TLS mode exited 0; want non-zero")
		}
		if !strings.Contains(stdout, "TLS mode required") && !strings.Contains(stderr, "TLS mode required") {
			t.Fatalf("expected 'TLS mode required'.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})

	t.Run("setup sso-enabled without client id/secret fails validation", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, homeDir, "setup",
			"--domain", "example.test",
			"--acme-email", "ops@example.test",
			"--openobserve-email", "admin@example.test",
			"--sso-enabled")
		if code == 0 {
			t.Fatalf("setup --sso-enabled without client id/secret exited 0; want non-zero")
		}
		if !strings.Contains(stdout, "SSO enabled but client ID/secret missing") &&
			!strings.Contains(stderr, "SSO enabled but client ID/secret missing") {
			t.Fatalf("expected 'SSO enabled but client ID/secret missing'.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})
}

// TestSetupWizardClusterUp drives the full wizard → cluster up flow with SSO
// enabled against a real single-node Swarm (same gating as TestClusterUp).
func TestSetupWizardClusterUp(t *testing.T) {
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping setup wizard swarm e2e")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping setup wizard swarm e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		t.Cleanup(func() {
			t.Log("TestSetupWizardClusterUp: leaving Swarm we initialised (cleanup)")
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	homeDir := t.TempDir()
	stdout, stderr, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	t.Logf("pmcluster init OK (home=%s)", homeDir)

	certPath, keyPath := generateSelfSignedCert(t, homeDir)
	t.Logf("Generated self-signed cert: %s  key: %s", certPath, keyPath)

	const (
		domain     = "example.test"
		adminEmail = "admin@example.test"
		clientID   = "e2e-client-id"
		clientSec  = "e2e-client-secret"
	)

	setupArgs := []string{
		"setup",
		"--domain", domain,
		"--cert", certPath,
		"--key", keyPath,
		"--openobserve-email", adminEmail,
		"--traefik-admin-user", "admin",
		"--sso-enabled",
		"--sso-client-id", clientID,
		"--sso-client-secret", clientSec,
		"--sso-github-org", "nextrum-s",
	}

	// First run on a fresh cluster must hand off to `cluster up`.
	t.Log("Running `pmcluster setup` on a fresh cluster (expects handoff to cluster up)")
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
	if !strings.Contains(stdout, "BOOTSTRAP CREDENTIALS") {
		t.Fatalf("expected BOOTSTRAP CREDENTIALS block.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// SSO stack must be deployed.
	t.Log("Verifying sso stack + services")
	ssoStackOut, ssoStackErr := dockerRun(ctx, "stack", "ls", "--format", "{{.Name}}")
	if ssoStackErr != nil {
		t.Fatalf("docker stack ls: %v", ssoStackErr)
	}
	if !strings.Contains(ssoStackOut, "sso") {
		t.Fatalf("expected sso stack in docker stack ls, got:\n%s", ssoStackOut)
	}
	waitServiceHealthy(t, ctx, "sso_oauth2-proxy", 1, 90*time.Second)

	// sso_cookie_secret credential must exist in the store.
	t.Log("Verifying sso_cookie_secret credential")
	credsOut, _, credsCode := runCmd(t, homeDir, "credentials", "list")
	if credsCode != 0 {
		t.Fatalf("credentials list exited %d:\n%s", credsCode, credsOut)
	}
	if !strings.Contains(credsOut, "sso_cookie_secret") {
		t.Fatalf("expected sso_cookie_secret in credentials list, got:\n%s", credsOut)
	}

	// Rendered Traefik dynamic config must use the sso-auth forwardAuth
	// middleware (SSO enabled) and reference the oauth2-proxy address.
	t.Log("Verifying rendered Traefik dynamic config uses sso-auth forwardAuth")
	traefikCfg := dockerConfigIDByPrefix(t, ctx, "pmcluster_traefik_dynamic_v")
	if traefikCfg == "" {
		t.Fatal("pmcluster_traefik_dynamic_v* docker config not found")
	}
	dynOut := mustDockerRun(t, ctx, "config", "inspect", "--format", "{{json .Spec.Data}}", traefikCfg)
	if !strings.Contains(dynOut, "sso-auth") {
		t.Fatalf("expected sso-auth middleware in rendered traefik dynamic config:\n%s", dynOut)
	}
	if !strings.Contains(dynOut, "sso_oauth2-proxy:4180") {
		t.Fatalf("expected forwardAuth address sso_oauth2-proxy:4180:\n%s", dynOut)
	}
	if strings.Contains(dynOut, "admin_credentials") {
		t.Fatalf("SSO enabled but admin-auth basicAuth (admin_credentials) still referenced:\n%s", dynOut)
	}

	// Re-running setup on the existing cluster must hand off to `cluster update`.
	t.Log("Re-running `pmcluster setup` on existing cluster (expects handoff to cluster update)")
	stdout, stderr, code = runCmd(t, homeDir, setupArgs...)
	if code != 0 {
		t.Fatalf("setup on existing cluster exited %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Cluster already initialised — running `pmcluster cluster update`") {
		t.Fatalf("expected 'Cluster already initialised' handoff message.\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// Cleanup: tear the cluster down.
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

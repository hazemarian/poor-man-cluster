//go:build e2e

// Package e2e contains end-to-end tests for the pmcluster binary.
// This file tests `pmcluster cluster up` against a real single-node Docker
// Swarm. It is gated behind the PMCLUSTER_E2E_SWARM=1 env var so it never
// runs by accident on developer machines — CI sets the variable explicitly.
//
// Run via: PMCLUSTER_E2E_SWARM=1 make e2e
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestClusterUp exercises `pmcluster cluster up` against a real Docker Swarm.
//
// Skip conditions:
//   - PMCLUSTER_E2E_SWARM != "1"
//   - `docker` binary not on PATH
//
// The test initialises a single-node Swarm if one is not already active, then
// drives the full cluster up → idempotent re-run → cluster down --purge flow.
func TestClusterUp(t *testing.T) {
	// ── Guard: env var ────────────────────────────────────────────────────────
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM is not set to 1; skipping cluster up e2e (set it in CI or locally to enable)")
	}

	// ── Guard: docker binary ──────────────────────────────────────────────────
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping cluster up e2e")
	}

	// Cap the whole test to 5 minutes.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ── Swarm bootstrap ───────────────────────────────────────────────────────
	// We check whether Swarm is already active before touching it so running
	// this test on a real manager doesn't clobber the existing Swarm state.
	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		// Only leave the Swarm we created ourselves, not one that pre-existed.
		t.Cleanup(func() {
			t.Log("TestClusterUp: leaving Swarm we initialised (cleanup)")
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	// ── pmcluster init ────────────────────────────────────────────────────────
	homeDir := t.TempDir()
	stdout, _, code := runCmd(t, homeDir, "init")
	if code != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", code, stdout)
	}
	t.Logf("pmcluster init OK (home=%s)", homeDir)

	// ── Self-signed TLS cert + key ────────────────────────────────────────────
	certPath, keyPath := generateSelfSignedCert(t, homeDir)
	t.Logf("Generated self-signed cert: %s  key: %s", certPath, keyPath)

	// ── First run: cluster up ─────────────────────────────────────────────────
	const (
		domain     = "example.test"
		adminEmail = "admin@example.test"
	)
	upArgs := []string{
		"cluster", "up",
		"--domain=" + domain,
		"--cert=" + certPath,
		"--key=" + keyPath,
		"--openobserve-email=" + adminEmail,
	}

	t.Log("Running: pmcluster cluster up (first run)…")
	upOut, _, code := runCmdCtx(t, ctx, homeDir, upArgs...)
	if code != 0 {
		t.Fatalf("pmcluster cluster up exited %d:\n%s", code, upOut)
	}
	t.Logf("cluster up output:\n%s", upOut)

	// Ensure cluster is torn down even on test failure.
	t.Cleanup(func() {
		t.Log("TestClusterUp: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		out, _, _ := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge")
		t.Logf("cluster down output:\n%s", out)
	})

	// ── Assert first-run output ───────────────────────────────────────────────
	t.Run("first run output contains newly-created markers", func(t *testing.T) {
		// up.go prints "newly created (swarm secret …)" for each new cred.
		if !strings.Contains(upOut, "newly created") {
			t.Errorf("expected 'newly created' in first-run output; got:\n%s", upOut)
		}
		// printUpResult always emits this line.
		if !strings.Contains(upOut, "cluster up complete") {
			t.Errorf("expected 'cluster up complete' in output; got:\n%s", upOut)
		}
		// Bootstrap passwords block is shown for every cluster up (newly
		// created and pre-existing alike — see printUpResult).
		if !strings.Contains(upOut, "BOOTSTRAP CREDENTIALS") {
			t.Errorf("expected 'BOOTSTRAP CREDENTIALS' block in first-run output; got:\n%s", upOut)
		}
	})

	// ── Service health ────────────────────────────────────────────────────────
	// Each stack's services should reach desired replicas within 120 s.
	t.Run("all services are healthy after cluster up", func(t *testing.T) {
		for _, svc := range []struct {
			stack   string
			service string
			wantRep int // >= (global services may differ depending on node count)
		}{
			{stack: "infra", service: "traefik", wantRep: 1},
			{stack: "infra", service: "portainer", wantRep: 1},
			{stack: "observability", service: "openobserve", wantRep: 1},
			{stack: "observability", service: "otel-collector", wantRep: 1},
			{stack: "backup", service: "volume-backup", wantRep: 1},
		} {
			fullName := svc.stack + "_" + svc.service
			waitServiceHealthy(t, ctx, fullName, svc.wantRep, 120*time.Second)
		}
	})

	// ── Docker post-conditions ────────────────────────────────────────────────
	t.Run("docker stack ls shows infra, observability, backup", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "stack", "ls", "--format", "{{.Name}}")
		for _, stack := range []string{"infra", "observability", "backup"} {
			if !strings.Contains(out, stack) {
				t.Errorf("docker stack ls: stack %q not found; output:\n%s", stack, out)
			}
		}
	})

	t.Run("docker secret ls shows expected secrets", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "secret", "ls", "--format", "{{.Name}}")
		for _, secret := range []string{
			"admin_credentials",
			"cert",
			"key",
			"portainer_admin_password",
			"zo_root_user_password",
		} {
			if !strings.Contains(out, secret) {
				t.Errorf("docker secret ls: secret %q not found; output:\n%s", secret, out)
			}
		}
	})

	t.Run("docker config ls shows pmcluster configs", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "config", "ls", "--format", "{{.Name}}")
		// Configs are versioned (e.g. pmcluster_otel_config_v001).
		for _, prefix := range []string{"pmcluster_otel_config_v", "pmcluster_traefik_dynamic_v"} {
			if !strings.Contains(out, prefix) {
				t.Errorf("docker config ls: config with prefix %q not found; output:\n%s", prefix, out)
			}
		}
	})

	t.Run("docker network ls shows overlay networks", func(t *testing.T) {
		out := mustDockerRun(t, ctx, "network", "ls", "--filter", "driver=overlay", "--format", "{{.Name}}")
		for _, net := range []string{"traefik-net", "monitoring-net"} {
			if !strings.Contains(out, net) {
				t.Errorf("docker network ls: network %q not found; output:\n%s", net, out)
			}
		}
	})

	// ── credentials CLI ───────────────────────────────────────────────────────
	t.Run("credentials list shows three rows", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "credentials", "list")
		if code != 0 {
			t.Fatalf("pmcluster credentials list exited %d:\n%s", code, out)
		}
		// Header row + 3 data rows = at least 3 newlines after header.
		// Each data row contains a tab-separated name. We check for each name.
		for _, name := range []string{"traefik_dashboard", "portainer", "openobserve_admin"} {
			if !strings.Contains(out, name) {
				t.Errorf("credentials list: %q not found; output:\n%s", name, out)
			}
		}
	})

	t.Run("credentials show portainer prints non-empty password", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "credentials", "show", "portainer")
		if code != 0 {
			t.Fatalf("pmcluster credentials show portainer exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "password:") {
			t.Errorf("expected 'password:' line in credentials show output; got:\n%s", out)
		}
		// password: <value> — value must be non-empty
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "password:") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
					t.Errorf("password line is empty: %q", line)
				}
				break
			}
		}
	})

	// ── Config update: second run reuses identical configs ──────────────
	// Docker configs are immutable, but pmcluster is content-aware: a
	// second `cluster up` with unchanged inputs must reuse the same
	// versioned name (e.g. pmcluster_otel_config_v001) rather than churn
	// a new version every run. Configs/secrets carry a pmcluster.data_hash
	// label; EnsureConfig reuses the current version when the label hash
	// matches, and only mints _vNNN+1 when content actually changed.
	t.Run("second cluster up reuses unchanged configs and keeps services healthy", func(t *testing.T) {
		// Record current versioned config IDs before re-run (by prefix).
		configsBefore := map[string]string{}
		for _, prefix := range []string{"pmcluster_otel_config_v", "pmcluster_traefik_dynamic_v"} {
			id := dockerConfigIDByPrefix(t, ctx, prefix)
			if id == "" {
				t.Fatalf("config with prefix %q not found before re-run", prefix)
			}
			configsBefore[prefix] = id
			t.Logf("Before: config prefix=%s => ID=%s", prefix, id)
		}

		// Re-run cluster up.
		t.Log("Running: pmcluster cluster up (second run — config update)…")
		out2, _, code := runCmdCtx(t, ctx, homeDir, upArgs...)
		if code != 0 {
			t.Fatalf("pmcluster cluster up (second run) exited %d:\n%s", code, out2)
		}

		// Verify config IDs are REUSED (content-aware: unchanged input →
		// same version, no churn).
		for _, prefix := range []string{"pmcluster_otel_config_v", "pmcluster_traefik_dynamic_v"} {
			newID := dockerConfigIDByPrefix(t, ctx, prefix)
			if newID == "" {
				t.Fatalf("config with prefix %q not found after re-run", prefix)
			}
			if newID != configsBefore[prefix] {
				t.Errorf("config with prefix %q ID changed on unchanged re-run (old=%s, new=%s) — expected content-aware reuse", prefix, configsBefore[prefix], newID)
			}
			t.Logf("After:  config prefix=%s => ID=%s (reused)", prefix, newID)
		}

		// Services must still be healthy after config update + re-deploy.
		for _, svc := range []struct {
			stack   string
			service string
			wantRep int
		}{
			{stack: "infra", service: "traefik", wantRep: 1},
			{stack: "infra", service: "portainer", wantRep: 1},
			{stack: "observability", service: "openobserve", wantRep: 1},
			{stack: "observability", service: "otel-collector", wantRep: 1},
		} {
			fullName := svc.stack + "_" + svc.service
			waitServiceHealthy(t, ctx, fullName, svc.wantRep, 120*time.Second)
		}

		// No credential should be marked as "newly created" on re-run.
		if strings.Contains(out2, "newly created") {
			t.Errorf("expected NO 'newly created' on second run; got:\n%s", out2)
		}
		if !strings.Contains(out2, "cluster up complete") {
			t.Errorf("expected 'cluster up complete' on second run; got:\n%s", out2)
		}
	})

	// ── Config update: changed content mints a new version ───────────────────
	// The flip side of content-aware reuse: when an operator edits a config
	// template on disk (source of truth), the next `cluster up` must detect
	// the new content hash and mint a fresh versioned Docker config.
	t.Run("edited config file mints a new Docker config version", func(t *testing.T) {
		cfgPath := homeDir + "/.pmcluster/config/otel-collector-config.yml"
		old, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("read %s: %v", cfgPath, err)
		}

		before := dockerConfigIDByPrefix(t, ctx, "pmcluster_otel_config_v")
		if before == "" {
			t.Fatalf("otel config not found before edit")
		}

		// Append a harmless comment to change the content hash.
		edited := append([]byte("# operator edit — force a new version\n"), old...)
		if err := os.WriteFile(cfgPath, edited, 0o644); err != nil {
			t.Fatalf("write %s: %v", cfgPath, err)
		}

		out3, _, code := runCmdCtx(t, ctx, homeDir, upArgs...)
		if code != 0 {
			t.Fatalf("pmcluster cluster up after edit exited %d:\n%s", code, out3)
		}

		after := dockerConfigIDByPrefix(t, ctx, "pmcluster_otel_config_v")
		if after == "" {
			t.Fatalf("otel config not found after edit")
		}
		if after == before {
			t.Errorf("otel config ID did not change after content edit (old=%s, new=%s)", before, after)
		}
		t.Logf("otel config rotated: %s → %s", before, after)

		// Services must stay healthy after the config rotation + redeploy.
		waitServiceHealthy(t, ctx, "observability_otel-collector", 1, 120*time.Second)
		if !strings.Contains(out3, "cluster up complete") {
			t.Errorf("expected 'cluster up complete' after edit; got:\n%s", out3)
		}
	})

	// ── Cleanup: cluster down --purge ─────────────────────────────────────────
	// This is tested here explicitly (in addition to the t.Cleanup above) to
	// verify the command exits 0 before the deferred cleanup runs.
	t.Run("cluster down --yes --purge exits 0", func(t *testing.T) {
		downOut, _, code := runCmdCtx(t, ctx, homeDir, "cluster", "down", "--yes", "--purge")
		if code != 0 {
			t.Fatalf("pmcluster cluster down --yes --purge exited %d:\n%s", code, downOut)
		}
		t.Logf("cluster down output:\n%s", downOut)
		// After purge, stacks should be gone.
		// Give Swarm a moment to remove stacks before querying.
		time.Sleep(3 * time.Second)
		stackOut, _ := dockerRunNoFail(ctx, "stack", "ls", "--format", "{{.Name}}")
		for _, stack := range []string{"infra", "observability", "backup"} {
			if strings.Contains(stackOut, stack) {
				t.Errorf("stack %q still listed after cluster down --purge; stack ls output:\n%s", stack, stackOut)
			}
		}
	})
}

// ── helpers ───────────────────────────────────────────────────────────────────

// ensureSwarmActive checks whether Swarm is already active. If not, it runs
// `docker swarm init --advertise-addr 127.0.0.1` and returns true so the
// caller knows to leave the Swarm in t.Cleanup. It also ensures the backup
// destination directory exists inside the Docker VM (required on Docker
// Desktop where /var/backups is not shared from the macOS host).
func ensureSwarmActive(t *testing.T, ctx context.Context) (weInited bool) {
	t.Helper()
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 15*time.Second)
	defer inspectCancel()

	out, err := dockerRun(inspectCtx, "info", "--format", "{{.Swarm.LocalNodeState}}")
	if err == nil && strings.TrimSpace(out) == "active" {
		t.Log("Swarm already active; skipping docker swarm init")
	} else {
		t.Log("Swarm not active; running docker swarm init --advertise-addr 127.0.0.1")
		initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
		defer initCancel()

		initOut, initErr := dockerRun(initCtx, "swarm", "init", "--advertise-addr", "127.0.0.1")
		if initErr != nil {
			t.Fatalf("docker swarm init: %v\n%s", initErr, initOut)
		}
		t.Log("docker swarm init: OK")
		weInited = true
	}

	// Ensure the backup destination directory exists inside the Docker VM.
	// On Docker Desktop, /var/backups is not shared from macOS, so we create
	// it directly inside the VM via nsenter. On native Linux this is a no-op
	// if the directory already exists.
	dirCtx, dirCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dirCancel()
	// nsenter into the host PID namespace (Docker Desktop VM or real host)
	// and mkdir -p. If this fails (e.g., no --privileged support), we log
	// but don't fail — the backup service health check will catch it.
	mkdirOut, mkdirErr := dockerRun(dirCtx, "run", "--rm", "--privileged", "--pid=host",
		"alpine", "nsenter", "-t", "1", "-m", "-u", "-n", "-i",
		"mkdir", "-p", "/var/backups/docker-volumes")
	if mkdirErr != nil {
		t.Logf("Note: could not create /var/backups/docker-volumes (backup service may fail): %v\n%s", mkdirErr, mkdirOut)
	} else {
		t.Log("/var/backups/docker-volumes ensured inside Docker VM")
	}

	return weInited
}

// generateSelfSignedCert creates a self-signed RSA-2048 certificate + key in
// dir using Go stdlib crypto only (no openssl). The cert has a 1-year validity,
// CN=example.test, and SANs: *.example.test.
// Returns the paths to the written cert and key PEM files.
func generateSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "example.test",
		},
		DNSNames:              []string{"*.example.test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}

	certPath = fmt.Sprintf("%s/tls.crt", dir)
	keyPath = fmt.Sprintf("%s/tls.key", dir)

	// Write cert
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("pem.Encode cert: %v", err)
	}
	certFile.Close()

	// Write key
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "PRIVATE KEY", Bytes: privDER}); err != nil {
		t.Fatalf("pem.Encode key: %v", err)
	}
	keyFile.Close()

	return certPath, keyPath
}

// dockerRun executes a `docker <args...>` command with a timeout and returns
// combined stdout+stderr and any error.
func dockerRun(ctx context.Context, args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = io.MultiWriter(&buf, os.Stdout)
	cmd.Stderr = io.MultiWriter(&buf, os.Stdout)
	err := cmd.Run()
	return buf.String(), err
}

// dockerRunNoFail is like dockerRun but ignores the error (for post-condition
// queries where absence is acceptable).
func dockerRunNoFail(ctx context.Context, args ...string) (string, error) {
	return dockerRun(ctx, args...)
}

// waitServiceHealthy polls `docker service ls` for <stack>_<service> until
// it reaches at least wantReplicas running replicas or until deadline expires.
// Global services report 0/0 until scheduled, then N/N.
func waitServiceHealthy(t *testing.T, ctx context.Context, fullName string, wantRep int, deadline time.Duration) {
	t.Helper()
	queryCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for {
		select {
		case <-queryCtx.Done():
			t.Fatalf("service %s: did not reach %d running replicas within %v", fullName, wantRep, deadline)
		default:
		}

		out, err := dockerRunNoFail(ctx,
			"service", "ls",
			"--filter", "name="+fullName,
			"--format", "{{.Name}} {{.Replicas}}",
		)
		if err != nil {
			t.Logf("service %s: docker service ls failed (will retry): %v", fullName, err)
			time.Sleep(2 * time.Second)
			continue
		}

		replicas := parseServiceReplicas(t, out, fullName)
		if replicas.running >= wantRep {
			t.Logf("service %s: healthy (%d/%d replicas)", fullName, replicas.running, replicas.total)
			return
		}
		t.Logf("service %s: %d/%d replicas — waiting…", fullName, replicas.running, replicas.total)
		time.Sleep(3 * time.Second)
	}
}

// serviceReplicas holds the parsed running/total count from docker service ls.
type serviceReplicas struct{ running, total int }

func parseServiceReplicas(t *testing.T, out, fullName string) serviceReplicas {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "<name> <running>/<total>"
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != fullName {
			continue
		}
		parts := strings.SplitN(fields[1], "/", 2)
		if len(parts) != 2 {
			return serviceReplicas{}
		}
		var r serviceReplicas
		fmt.Sscanf(parts[0], "%d", &r.running)
		fmt.Sscanf(parts[1], "%d", &r.total)
		return r
	}
	return serviceReplicas{}
}

// dockerConfigID returns the Docker config object ID for a named config,
// or "" if the config does not exist.
func dockerConfigID(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := dockerRun(cmdCtx, "config", "inspect", "--format", "{{.ID}}", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// dockerConfigIDByPrefix finds the latest (highest version) Docker config
// with the given name prefix and returns its ID, or "" if none found.
func dockerConfigIDByPrefix(t *testing.T, ctx context.Context, prefix string) string {
	t.Helper()
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := dockerRun(cmdCtx, "config", "ls", "--format", "{{.Name}}", "--filter", "name="+prefix)
	if err != nil {
		return ""
	}
	// Find the highest version (alphabetically last).
	lines := strings.Split(strings.TrimSpace(out), "\n")
	best := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line > best {
			best = line
		}
	}
	if best == "" {
		return ""
	}
	return dockerConfigID(t, ctx, best)
}

// mustDockerRun runs docker and fails the test if it errors.
func mustDockerRun(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := dockerRun(cmdCtx, args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// runCmdCtx is like runCmd but accepts a context (so we can cap individual
// pmcluster invocations within the overall test timeout).
func runCmdCtx(t *testing.T, ctx context.Context, homeDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer

	// Stream to both the buffer and os.Stdout so CI logs are visible.
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Stdout = io.MultiWriter(&stdoutBuf, os.Stdout)
	cmd.Stderr = io.MultiWriter(&stderrBuf, os.Stderr)
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

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestControlLoopE2E proves the control loop (v0.2.132, L1) on a real swarm:
//  1. the daemon writes stack_status snapshots for every stack (badge reads the DB);
//  2. drift (a rendered-hash change with no deploy trigger) is auto-synced by the loop;
//  3. the badge reflects the DB snapshot (under-replication flips it to degraded);
//  4. the loop only runs on the leader.
//
// Swarm-gated (PMCLUSTER_E2E_SWARM=1), mirroring TestClusterUp's harness.
func TestControlLoopE2E(t *testing.T) {
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM=1 required")
	}
	requireDockerDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	weInited := ensureSwarmActive(t, ctx)
	_ = weInited

	homeDir := t.TempDir()
	runCmd(t, homeDir, "init")
	// Keep the auto-started daemon (systemd, started by cluster up) loop-free
	// so it cannot race this test; the dedicated serve subprocess below is
	// the only loop runner.
	setReconcileInterval(t, homeDir, "0")
	certPath, keyPath := generateSelfSignedCert(t, homeDir)

	// Full cluster up (platform stacks) — the harness baseline.
	upOut, upErr, upCode := runCmdCtx(t, ctx, homeDir,
		"cluster", "up",
		"--domain=example.test",
		"--cert="+certPath, "--key="+keyPath,
		"--openobserve-email=admin@example.test")
	if upCode != 0 {
		t.Fatalf("cluster up exited %d:\nstdout:\n%s\nstderr:\n%s", upCode, upOut, upErr)
	}
	if !strings.Contains(upOut, "cluster up complete") {
		t.Fatalf("cluster up output missing completion marker:\n%s", upOut)
	}

	// Best-effort cluster teardown on ANY failure path so a mid-test abort can
	// never leak platform stacks/networks into the next swarm test.
	t.Cleanup(func() {
		t.Log("TestControlLoopE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		if out, errOut, code := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
			t.Logf("cluster down (cleanup) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	// Fast reconcile so the test converges within seconds.
	if out, errOut, code := runCmd(t, homeDir, "cluster", "settings", "set", "reconcile_interval=1"); code != 0 {
		t.Fatalf("set reconcile_interval exited %d:\n%s\n%s", code, out, errOut)
	}

	// Start the daemon (serve) as a subprocess on a free port.
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
	base := "http://" + addr

	// Deploy a small app stack referencing a config value (drift source).
	const manifest = `app: loop-demo
env: test
domain: example.test
services:
  web:
    image: nginx:1.27-alpine
    env:
      GREETING: config(loop_greeting)
`
	manifestPath := homeDir + "/loop-demo.yaml"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if out, errOut, code := runCmd(t, homeDir, "config", "create", "loop_greeting", "--scope", "service", "--stack", "loop-demo", "--value", "hello"); code != 0 {
		t.Fatalf("config create exited %d:\n%s\n%s", code, out, errOut)
	}
	if out, errOut, code := runCmd(t, homeDir, "deploy", manifestPath); code != 0 {
		t.Fatalf("deploy exited %d:\n%s\n%s", code, out, errOut)
	}

	// (1) Loop writes stack_status → the DB-driven badge flips to healthy.
	waitBadgeStatus(t, base, "loop-demo", "healthy")
	t.Logf("badge after deploy: loop-demo healthy (DB snapshot)")

	// (2) Drift: edit the config value; the next loop pass re-renders, sees a
	// hash mismatch, and syncs the stack WITHOUT any deploy trigger.
	before := currentRevisionViaCmd(t, homeDir)
	if out, errOut, code := runCmd(t, homeDir, "config", "edit", "loop_greeting", "--value", "goodbye"); code != 0 {
		t.Fatalf("config edit exited %d:\n%s\n%s", code, out, errOut)
	}
	waitRevisionBump(t, homeDir, before, 60*time.Second)
	t.Logf("drift auto-synced: revision %d -> %d (no deploy trigger)", before, currentRevisionViaCmd(t, homeDir))

	// (3) Under-replication flips the badge to degraded (read from the DB).
	// We make the DESIRED state unhealthy by re-deploying a manifest whose
	// placement constraint can never be satisfied (Desired stays 1, Replicas
	// drops to 0 — scale-to-0 would make Desired=0, which derives healthy).
	// A manual `docker service update` is NOT used here: it would race the
	// loop's own force-update ("update out of sequence") and get reverted by
	// the next sync pass, since the loop converges to the stored source.
	const degradedManifest = `app: loop-demo
env: test
domain: example.test
services:
  web:
    image: nginx:1.27-alpine
    placement: no-such-node
    env:
      GREETING: config(loop_greeting)
`
	degradedPath := homeDir + "/loop-demo-degraded.yaml"
	if err := os.WriteFile(degradedPath, []byte(degradedManifest), 0o644); err != nil {
		t.Fatalf("write degraded manifest: %v", err)
	}
	if out, errOut, code := runCmd(t, homeDir, "deploy", degradedPath); code != 0 {
		t.Fatalf("deploy degraded manifest exited %d:\n%s\n%s", code, out, errOut)
	}
	waitBadgeStatus(t, base, "loop-demo", "degraded", "error")
	t.Logf("badge after unsatisfiable placement: loop-demo degraded/error")

	// Cleanup: remove the app stack and tear the cluster down.
	if out, errOut, code := runCmd(t, homeDir, "stack", "remove", "loop-demo"); code != 0 {
		t.Logf("stack remove exited %d (best-effort):\n%s\n%s", code, out, errOut)
	}
	if out, errOut, code := runCmdCtx(t, ctx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
		t.Fatalf("cluster down exited %d:\n%s\n%s", code, out, errOut)
	}
}

// waitBadgeStatus polls the public DB-driven badge endpoint until the stack
// reports one of the wanted statuses (the control loop writes stack_status, the
// badge only reads it — a 200 'unknown' means the snapshot hasn't landed yet).
func waitBadgeStatus(t *testing.T, base, stack string, want ...string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/api/public/badge/" + stack)
		if err == nil {
			body := make([]byte, 4096)
			n, _ := resp.Body.Read(body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				for _, w := range want {
					if strings.Contains(string(body[:n]), stack+": "+w) {
						return
					}
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("badge for %s did not reach %q within 60s", stack, want)
}

// waitRevisionBump polls `pmcluster stack show` until the current revision
// advances past the given value — proof the loop synced the drifted stack.
func waitRevisionBump(t *testing.T, homeDir string, before int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rev := currentRevisionViaCmd(t, homeDir); rev > before {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("stack revision did not advance past %d within %s", before, timeout)
}

func currentRevisionViaCmd(t *testing.T, homeDir string) int64 {
	t.Helper()
	out, _, code := runCmd(t, homeDir, "stack", "show", "loop-demo")
	if code != 0 {
		return 0
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// runStackShow prints "Current revision: N (RFC3339)".
		if strings.HasPrefix(line, "Revision:") || strings.HasPrefix(line, "Current revision:") {
			var rev int64
			val := strings.TrimSpace(strings.TrimPrefix(line, "Current revision:"))
			val = strings.TrimSpace(strings.TrimPrefix(val, "Revision:"))
			if _, err := fmt.Sscanf(val, "%d", &rev); err == nil {
				return rev
			}
		}
	}
	return 0
}

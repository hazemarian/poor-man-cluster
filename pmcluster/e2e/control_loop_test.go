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

// TestControlLoopE2E proves the control loop (v0.2.132, L1) on a real swarm.
//
// It runs as three INDEPENDENT phases against one cluster: a single `cluster up` is the expensive
// part (~40s of stack deploys), while each phase is a few seconds of behaviour. Sharing the cluster
// but not the state is the point:
//
//   - every phase uses its OWN stack name and its OWN config name, so a leak from one phase cannot
//     change another phase's answer (the shared-swarm leftovers that made a single 238-line test
//     report "config edit exited 1" with no output and no way to tell which step did it);
//   - every phase is a t.Run, so a failure names the phase and the remaining phases still run —
//     one broken assertion no longer hides the two behind it;
//   - every phase removes its own stack on the way out.
//
// Swarm-gated (PMCLUSTER_E2E_SWARM=1), mirroring TestClusterUp's harness.
func TestControlLoopE2E(t *testing.T) {
	cl := setupControlLoopCluster(t)

	t.Run("badge_reads_the_db_snapshot", func(t *testing.T) {
		cl.phase(t, "loop-badge", func(ctx context.Context, t *testing.T, app loopApp) {
			// (1) Loop writes stack_status → the DB-driven badge flips to healthy.
			waitBadgeStatus(t, cl.base, app.stack, "healthy")
			t.Logf("badge after deploy: %s healthy (DB snapshot)", app.stack)
		})
	})

	t.Run("drift_syncs_without_a_deploy", func(t *testing.T) {
		cl.phase(t, "loop-drift", func(ctx context.Context, t *testing.T, app loopApp) {
			// (2) Drift: edit the config value; the next loop pass re-renders, sees a hash
			// mismatch, and syncs the stack WITHOUT any deploy trigger.
			before := currentRevisionViaCmd(t, cl.homeDir, app.stack)
			if out, errOut, code := runCmd(t, cl.homeDir, "config", "edit", app.config, "--value", "goodbye"); code != 0 {
				t.Fatalf("config edit exited %d:\n%s\n%s", code, out, errOut)
			}
			waitRevisionBump(t, cl.homeDir, app.stack, before, 60*time.Second)
			t.Logf("drift auto-synced: revision %d -> %d (no deploy trigger)", before, currentRevisionViaCmd(t, cl.homeDir, app.stack))
		})
	})

	t.Run("the_badge_degrades_when_tasks_fail", func(t *testing.T) {
		cl.phase(t, "loop-degraded", func(ctx context.Context, t *testing.T, app loopApp) {
			// (3) Under-replication flips the badge to degraded (read from the DB).
			// The DESIRED state is made unhealthy by re-deploying a manifest whose image cannot be
			// pulled (a nonexistent tag) — the new task is rejected, so Desired stays 1 while
			// Replicas drops to 0 (scale-to-0 would make Desired=0, which derives healthy; an
			// unsatisfiable placement would not apply either — start-first keeps the old task
			// running at 1/1). The loop converges TO this state since it is the recorded manifest,
			// and it snapshots the live under-replication as degraded.
			degraded := strings.Replace(app.manifest, "nginx:1.27-alpine", "nginx:1.27-no-such-tag", 1)
			path := cl.homeDir + "/" + app.stack + "-degraded.yaml"
			if err := os.WriteFile(path, []byte(degraded), 0o644); err != nil {
				t.Fatalf("write degraded manifest: %v", err)
			}
			if out, errOut, code := runCmd(t, cl.homeDir, "deploy", path); code != 0 {
				t.Fatalf("deploy degraded manifest exited %d:\n%s\n%s", code, out, errOut)
			}
			waitBadgeStatus(t, cl.base, app.stack, "degraded", "error")
			t.Logf("badge after un-pullable image: %s degraded/error", app.stack)
		})
	})
}

// loopCluster is one running cluster shared by the phases.
type loopCluster struct {
	homeDir string
	base    string
	t       *testing.T
}

// loopApp is what a phase is handed: its own stack name, config name and manifest.
type loopApp struct {
	stack    string
	config   string
	manifest string
}

// setupControlLoopCluster brings up a cluster once: init, hermetic storage root, platform stacks,
// a dedicated daemon and the reconcile interval the loop needs to converge in seconds.
func setupControlLoopCluster(t *testing.T) *loopCluster {
	t.Helper()
	requireSwarm(t)
	requireDockerDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ensureSwarmActive(t, ctx)

	homeDir := t.TempDir()
	runCmd(t, homeDir, "init")
	// Keep the auto-started daemon (systemd, started by cluster up) loop-free so it cannot race
	// this test; the dedicated serve subprocess below is the only loop runner.
	setReconcileInterval(t, homeDir, "0")
	// Hermetic storage root (see TestControlPlaneSnapshotE2E): `cluster up` otherwise hardcodes the
	// root-owned /var/stack/{data,backup}, which a non-root dev host (macOS) cannot create.
	storageRoot := homeDir + "/stack"
	if out, errOut, code := runCmd(t, homeDir, "cluster", "settings", "set", "volume_root="+storageRoot); code != 0 {
		t.Fatalf("set volume_root exited %d:\n%s\n%s", code, out, errOut)
	}
	t.Setenv("PMCLUSTER_BACKUP_DIR", storageRoot+"/backup")
	certPath, keyPath := generateSelfSignedCert(t, homeDir)

	upOut, upErr, upCode := runCmdCtx(t, ctx, homeDir,
		"cluster", "up",
		"--domain=example.test",
		"--cert="+certPath, "--key="+keyPath,
		"--observability-admin-email=admin@example.test")
	if upCode != 0 {
		t.Fatalf("cluster up exited %d:\nstdout:\n%s\nstderr:\n%s", upCode, upOut, upErr)
	}
	if !strings.Contains(upOut, "cluster up complete") {
		t.Fatalf("cluster up output missing completion marker:\n%s", upOut)
	}

	// Best-effort teardown on ANY failure path so a mid-test abort can never leak platform
	// stacks/networks into the next swarm test.
	t.Cleanup(func() {
		t.Log("TestControlLoopE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		if out, errOut, code := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
			t.Logf("cluster down (cleanup) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	// Fast reconcile so each phase converges within seconds.
	if out, errOut, code := runCmd(t, homeDir, "cluster", "settings", "set", "reconcile_interval=1"); code != 0 {
		t.Fatalf("set reconcile_interval exited %d:\n%s\n%s", code, out, errOut)
	}

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

	return &loopCluster{homeDir: homeDir, base: "http://" + addr, t: t}
}

// phase deploys an app stack of its own and runs the phase body against it, removing the stack
// afterwards whatever happens. A phase that fails halfway leaves nothing for its siblings to trip
// over — that is the whole reason the phases were separated.
func (cl *loopCluster) phase(t *testing.T, stack string, body func(ctx context.Context, t *testing.T, app loopApp)) {
	t.Helper()
	ctx := context.Background()
	config := stack + "-greeting"
	manifest := fmt.Sprintf(`app: %s
env: test
domain: example.test
services:
  web:
    image: nginx:1.27-alpine
    env:
      GREETING: config(%s)
`, stack, config)

	path := cl.homeDir + "/" + stack + ".yaml"
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if out, errOut, code := runCmd(t, cl.homeDir, "config", "create", config, "--scope", "service", "--stack", stack, "--value", "hello"); code != 0 {
		t.Fatalf("config create exited %d:\n%s\n%s", code, out, errOut)
	}
	if out, errOut, code := runCmd(t, cl.homeDir, "deploy", path); code != 0 {
		t.Fatalf("deploy exited %d:\n%s\n%s", code, out, errOut)
	}
	t.Cleanup(func() {
		if out, errOut, code := runCmd(t, cl.homeDir, "stack", "remove", stack); code != 0 {
			t.Logf("stack remove %s exited %d (best-effort):\n%s\n%s", stack, code, out, errOut)
		}
	})

	body(ctx, t, loopApp{stack: stack, config: config, manifest: manifest})
}

// waitBadgeStatus polls the public DB-driven badge endpoint until the stack reports one of the
// wanted statuses (the control loop writes stack_status, the badge only reads it — a 200 'unknown'
// means the snapshot hasn't landed yet).
func waitBadgeStatus(t *testing.T, base, stack string, want ...string) {
	t.Helper()
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

// waitRevisionBump polls `pmcluster stack show` until the current revision advances past the given
// value — proof the loop synced the drifted stack.
func waitRevisionBump(t *testing.T, homeDir, stack string, before int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rev := currentRevisionViaCmd(t, homeDir, stack); rev > before {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("stack %s revision did not advance past %d within %s", stack, before, timeout)
}

func currentRevisionViaCmd(t *testing.T, homeDir, stack string) int64 {
	t.Helper()
	out, _, code := runCmd(t, homeDir, "stack", "show", stack)
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

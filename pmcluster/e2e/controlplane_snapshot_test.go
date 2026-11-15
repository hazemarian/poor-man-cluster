//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestControlPlaneKeyConfigE2E proves the LAST piece of the Docker-config channel
// (ADR 0005): the leader publishes the encryption KEY as its own Raft-replicated config,
// and publishes NO data kit at all — the mirror carries the cluster state now, and
// adoption copies nothing.
//
// It asserts:
//
//  1. a `pmcluster.kind=state-key` config appears, carrying exactly the 32-byte AES-GCM
//     key and nothing else;
//  2. NO `pmcluster.kind=state` config is ever created (the retired data kit), polled for
//     several publish intervals rather than asserted once;
//  3. prune keeps at most 2 key configs.
//
// Swarm-gated (PMCLUSTER_E2E_SWARM=1), mirroring TestControlLoopE2E's harness.

func TestControlPlaneKeyConfigE2E(t *testing.T) {
	requireSwarm(t)
	requireDockerDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ensureSwarmActive(t, ctx)

	homeDir := t.TempDir()
	runCmd(t, homeDir, "init")
	// Keep the auto-started daemon (started by cluster up) loop-free so it
	// cannot race this test; the dedicated serve subprocess below is the only
	// publish-loop runner with a short interval.
	setReconcileInterval(t, homeDir, "0")
	// Hermetic storage root: `cluster up` otherwise hardcodes the
	// root-owned /var/stack/{data,backup}, which a non-root dev host (macOS)
	// cannot create. Relocate both the volume root (via the volume_root
	// setting) and the backup archive dir (via PMCLUSTER_BACKUP_DIR, consumed
	// by every runCmd below) under the temp HOME.
	storageRoot := homeDir + "/stack"
	if out, errOut, code := runCmd(t, homeDir, "cluster", "settings", "set", "volume_root="+storageRoot); code != 0 {
		t.Fatalf("set volume_root exited %d:\n%s\n%s", code, out, errOut)
	}
	t.Setenv("PMCLUSTER_BACKUP_DIR", storageRoot+"/backup")
	certPath, keyPath := generateSelfSignedCert(t, homeDir)

	upOut, upErr, upCode := runCmdCtx(t, ctx, homeDir, "cluster", "up",
		"--domain=example.test",
		"--cert="+certPath, "--key="+keyPath,
		"--observability-admin-email=admin@example.test")
	if upCode != 0 {
		t.Fatalf("cluster up exited %d:\nstdout:\n%s\nstderr:\n%s", upCode, upOut, upErr)
	}
	if !strings.Contains(upOut, "cluster up complete") {
		t.Fatalf("cluster up output missing completion marker:\n%s", upOut)
	}

	t.Cleanup(func() {
		t.Log("TestControlPlaneKeyConfigE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		if out, errOut, code := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
			t.Logf("cluster down (cleanup) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	// Dedicated daemon with a 1-second publish loop so the assertions below
	// converge in seconds instead of the 5-minute production default.
	addr := freePort(t)
	daemon := exec.Command(binaryPath, "serve")
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	daemon.Env = append(homeEnv(homeDir),
		"PMCLUSTER_LISTEN_ADDR="+addr,
		"PMCLUSTER_CONTROLPLANE_SNAPSHOT_INTERVAL=1")
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

	// (1) The key config appears, and carries exactly the key.
	keyName := waitConfigKind(t, ctx, "state-key", 30*time.Second)
	keyData := dockerConfigData(t, ctx, keyName)
	if len(keyData) != 32 {
		t.Fatalf("key config %s data is %d bytes, want 32 (AES-GCM key)", keyName, len(keyData))
	}
	t.Logf("key config present: %s (%d bytes)", keyName, len(keyData))

	// (2) The data kit is retired: no `state` config may ever appear, across several
	// publish intervals. One assertion at t=0 would pass on a daemon that simply had not
	// got there yet.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if names := configNamesByLabel(t, ctx, "state"); len(names) > 0 {
			t.Fatalf("a control-plane DATA kit reappeared: %v — cluster state travels by the mirror now (ADR 0005)", names)
		}
		time.Sleep(time.Second)
	}
	t.Log("no data-kit config was created: the mirror is the channel")

	// (3) Prune keeps at most two key configs. Rotating the key is the only thing that
	// publishes a second one; here the assertion is simply that a long-running daemon
	// re-checking every second does not grow the family without bound.
	waitConfigCountAtMost(t, ctx, "state-key", 2, 30*time.Second)
	t.Log("key config count stayed bounded")
}

// configNamesByLabel lists pmcluster control-plane configs of the given kind
// (pmcluster.kind label), sorted ascending by name (unixnano suffix ⇒
// lexicographic order is chronological).
func configNamesByLabel(t *testing.T, ctx context.Context, kind string) []string {
	t.Helper()
	out := mustDockerRun(t, ctx, "config", "ls", "--format", "{{.Name}}",
		"--filter", "label=pmcluster.kind="+kind)
	names := []string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names
}

// waitConfigKind waits for at least one config of the kind and returns the
// newest name.
func waitConfigKind(t *testing.T, ctx context.Context, kind string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		names := configNamesByLabel(t, ctx, kind)
		if len(names) > 0 {
			return names[len(names)-1]
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no pmcluster.kind=%s config appeared within %s", kind, timeout)
	return ""
}

// waitConfigCountAtMost waits until at most max configs of the kind remain
// (prune settled).
func waitConfigCountAtMost(t *testing.T, ctx context.Context, kind string, max int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(configNamesByLabel(t, ctx, kind)) <= max {
			return
		}
		time.Sleep(time.Second)
	}
	names := configNamesByLabel(t, ctx, kind)
	t.Fatalf("expected at most %d pmcluster.kind=%s configs, got %d: %v", max, kind, len(names), names)
}

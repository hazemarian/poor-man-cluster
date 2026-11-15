//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestNoControlPlaneConfigE2E proves the platform writes NO Docker config at all: the data kit
// went with ADR 0005 and the key config with ADR 0006.
//
// It asserts:
//
//  1. the daemon's key lives at `<data dir>/.encryption_key` as 32 raw bytes — the file an
//     operator copies and the join route will deliver;
//  2. no `pmcluster.kind=state-key` config ever appears, polled across several seconds rather
//     than asserted once (a daemon that simply had not got there yet would pass a single check);
//  3. no `pmcluster.kind=state` data kit either.
//
// Swarm-gated (PMCLUSTER_E2E_SWARM=1), mirroring TestControlLoopE2E's harness.

func TestNoControlPlaneConfigE2E(t *testing.T) {
	requireSwarm(t)
	requireDockerDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ensureSwarmActive(t, ctx)

	homeDir := t.TempDir()
	runCmd(t, homeDir, "init")
	setReconcileInterval(t, homeDir, "0")
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

	t.Cleanup(func() {
		t.Log("TestNoControlPlaneConfigE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		if out, errOut, code := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
			t.Logf("cluster down (cleanup) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	addr := freePort(t)
	daemon := exec.Command(binaryPath, "serve")
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	daemon.Env = append(homeEnv(homeDir),
		"PMCLUSTER_LISTEN_ADDR="+addr)
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

	// (1) The key is a FILE, minted once at init, 32 bytes of AES-GCM material, in the data
	// dir the daemon was given ($HOME/.pmcluster — the same place /root/.pmcluster holds it
	// on a real host), not next to HOME itself.
	keyBytes, err := os.ReadFile(filepath.Join(homeDir, ".pmcluster", ".encryption_key"))
	if err != nil {
		t.Fatalf("no key file at <data dir>/.encryption_key: %v", err)
	}
	if len(keyBytes) != 32 {
		t.Fatalf("key file is %d bytes, want 32", len(keyBytes))
	}
	t.Logf("key file present: %d bytes at $HOME/.pmcluster/.encryption_key", len(keyBytes))

	// (2)+(3) No config of either kind may appear, across several seconds of a running
	// daemon. One assertion at t=0 would pass on a daemon that simply had not got there yet.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if names := configNamesByLabel(t, ctx, "state-key"); len(names) > 0 {
			t.Fatalf("a control-plane KEY config appeared: %v — the key is a file now (ADR 0006)", names)
		}
		if names := configNamesByLabel(t, ctx, "state"); len(names) > 0 {
			t.Fatalf("a control-plane DATA kit appeared: %v — cluster state travels by the mirror (ADR 0005)", names)
		}
		time.Sleep(time.Second)
	}
	t.Log("no pmcluster config was created: the platform writes none")
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

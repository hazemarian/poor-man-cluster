//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestControlPlaneSnapshotE2E proves L2 (control-plane survivor kit →
// Raft-replicated Docker configs) on a real swarm:
//
//  1. a serving leader snapshots pmcluster_state_* and pmcluster_state_key_*
//     Docker configs into the swarm (both families present);
//  2. the security split holds on live data: the state config carries
//     data.db but NEVER the .encryption_key bytes; the key config carries
//     exactly the 32-byte AES-GCM key and nothing else;
//  3. a store mutation is captured within one snapshot interval (a NEWER
//     state config name appears);
//  4. prune keeps at most 2 snapshots per family.
//
// Swarm-gated (PMCLUSTER_E2E_SWARM=1), mirroring TestControlLoopE2E's harness.
func TestControlPlaneSnapshotE2E(t *testing.T) {
	if os.Getenv("PMCLUSTER_E2E_SWARM") != "1" {
		t.Skip("PMCLUSTER_E2E_SWARM=1 required")
	}
	requireDockerDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ensureSwarmActive(t, ctx)

	homeDir := t.TempDir()
	runCmd(t, homeDir, "init")
	// Keep the auto-started daemon (started by cluster up) loop-free so it
	// cannot race this test; the dedicated serve subprocess below is the only
	// snapshot-loop runner with a short interval.
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
		"--openobserve-email=admin@example.test")
	if upCode != 0 {
		t.Fatalf("cluster up exited %d:\nstdout:\n%s\nstderr:\n%s", upCode, upOut, upErr)
	}
	if !strings.Contains(upOut, "cluster up complete") {
		t.Fatalf("cluster up output missing completion marker:\n%s", upOut)
	}

	t.Cleanup(func() {
		t.Log("TestControlPlaneSnapshotE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanCancel()
		if out, errOut, code := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge"); code != 0 {
			t.Logf("cluster down (cleanup) exited %d:\n%s\n%s", code, out, errOut)
		}
	})

	// Dedicated daemon with a 1-second snapshot loop so the assertions below
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

	// (1) Both config families appear within the timeout.
	stateName := waitConfigKind(t, ctx, "state", 30*time.Second)
	keyName := waitConfigKind(t, ctx, "state-key", 30*time.Second)
	t.Logf("snapshot configs present: %s, %s", stateName, keyName)

	// (2) Security split on live data. The state payload is a gzipped tar;
	// entry names are the real security boundary (rendered stack YAML inside
	// the kit may legally MENTION the key path in comments, so a byte search
	// for the string would be a false positive). Assert instead: the tar has
	// no .encryption_key entry, and the raw 32-byte key material never rides
	// in the state payload. The key config is exactly those 32 bytes.
	stateData := gunzipConfigData(t, ctx, stateName)
	names := map[string]bool{}
	foundDB := false
	tr := tar.NewReader(bytes.NewReader(stateData))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entries from state config %s: %v", stateName, err)
		}
		names[hdr.Name] = true
		if hdr.Name == "data.db" {
			foundDB = true
		}
	}
	if !foundDB {
		t.Fatalf("state config %s has no data.db entry; entries: %v", stateName, names)
	}
	if names[".encryption_key"] {
		t.Fatalf("security split violated: state config %s contains a .encryption_key entry", stateName)
	}
	keyData := dockerConfigData(t, ctx, keyName)
	if len(keyData) != 32 {
		t.Fatalf("key config %s data is %d bytes, want 32 (AES-GCM key)", keyName, len(keyData))
	}
	if bytes.Contains(stateData, []byte(keyData)) {
		t.Fatalf("security split violated: state config %s embeds the raw encryption key bytes", stateName)
	}
	t.Log("security split verified: the 32-byte key lives only in its own config")

	// (3) A store mutation is captured within one snapshot interval (a newer
	// state config name appears; the suffix is unixnano, so the greatest name
	// is the newest snapshot).
	if out, errOut, code := runCmd(t, homeDir, "config", "create", "snap_check", "--scope", "cluster", "--value", "hello"); code != 0 {
		t.Fatalf("config create exited %d:\n%s\n%s", code, out, errOut)
	}
	nextState := waitConfigNewer(t, ctx, "state", stateName, 30*time.Second)
	t.Logf("mutation captured: %s -> %s", stateName, nextState)

	// (4) Prune keeps at most 2 snapshots per family even across further
	// mutations (two more store writes → several more snapshots → both
	// families must settle back to ≤ 2).
	if out, errOut, code := runCmd(t, homeDir, "config", "create", "snap_check_2", "--scope", "cluster", "--value", "world"); code != 0 {
		t.Fatalf("config create #2 exited %d:\n%s\n%s", code, out, errOut)
	}
	if out, errOut, code := runCmd(t, homeDir, "config", "create", "snap_check_3", "--scope", "cluster", "--value", "prune"); code != 0 {
		t.Fatalf("config create #3 exited %d:\n%s\n%s", code, out, errOut)
	}
	waitConfigCountAtMost(t, ctx, "state", 2, 45*time.Second)
	waitConfigCountAtMost(t, ctx, "state-key", 2, 45*time.Second)
	t.Log("prune verified: at most 2 snapshots per family retained")
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

// waitConfigNewer waits until a config of the kind with a name greater than
// older appears (a newer snapshot) and returns it.
func waitConfigNewer(t *testing.T, ctx context.Context, kind, older string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		names := configNamesByLabel(t, ctx, kind)
		if len(names) > 0 && names[len(names)-1] > older {
			return names[len(names)-1]
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no pmcluster.kind=%s config newer than %s appeared within %s", kind, older, timeout)
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

// gunzipConfigData returns the gunzipped payload of a config (the state
// config data is a gzipped tar, so raw byte searches against .Spec.Data never
// match entry names).
func gunzipConfigData(t *testing.T, ctx context.Context, name string) []byte {
	t.Helper()
	raw := dockerConfigData(t, ctx, name)
	gz, err := gzip.NewReader(bytes.NewReader([]byte(raw)))
	if err != nil {
		t.Fatalf("config %s is not a gzip payload: %v", name, err)
	}
	defer gz.Close()
	flat, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzip config %s: %v", name, err)
	}
	return flat
}

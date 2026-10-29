//go:build e2e

// Package e2e — backup and restore end-to-end test.
//
// The restore decision logic is already unit-tested (internal/backups,
// internal/cli), with the fetch/expand side stubbed. Nothing exercised the seam
// between the two halves: that a backup the offen agent actually wrote can be
// put back into the live volume root, on a real Swarm, where the app's own
// volume is a bind of that directory.
//
// That seam is where a restore fails silently in the worst case — the command
// reports success while the app comes back with an empty database. This test
// asserts the observable outcome instead of the exit code: it seeds a file into
// a deployed stack's volume, backs up, overwrites the file, restores, and
// requires the original bytes back and the later file gone.
//
// It deliberately does not let the service write the marker. A container that
// writes on start would recreate the expected file during the restore, so the
// assertion could pass on a restart that has nothing to do with the restore.
//
// Gated on CAPABILITY (see requireSwarm): it runs wherever a Swarm-capable
// developer machine — the swarm-e2e workflow sets it explicitly. The hermetic
// volume root keeps it off /var/stack, which a non-root dev host cannot create.
//
// Run via: make e2e
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// restoreManifest is a minimal stateful stack: one service holding one named
// volume. The service's only job is to hold the volume open, so the volume is
// created by the deploy pipeline exactly as a customer's would be — placed by
// the platform, bind-mounted under the volume root.
const restoreManifest = `app: %s
env: production
domain: example.test
services:
  store:
    image: busybox:1.36
    replicas: 1
    command: ["sh", "-c", "exec sleep 3600"]
    volumes: [app_data:/data]
`

// clearAppState removes this test's stack and its named volume, best-effort.
//
// `cluster down --purge` removes the platform stacks only, so an app stack
// deployed here would outlive the test. `docker stack rm` in turn does not
// remove volumes — and a volume keeps the driver_opts it was CREATED with, so a
// volume left by a previous run makes the next run mount that run's (by then
// deleted) device path. The service then never starts and the failure reads as
// a code bug in the deploy path. Called before deploying and again in cleanup.
func clearAppState(t *testing.T, app, volume string) {
	t.Helper()
	ctx := context.Background()
	if out, err := dockerRun(ctx, "stack", "rm", app); err != nil {
		t.Logf("docker stack rm %s: %v\n%s", app, err, out)
	}
	// The volume can only be removed once no service references it, and
	// `stack rm` converges ASYNCHRONOUSLY: the CLI returns while the tasks are
	// still being stopped, and a volume held by a dying task refuses removal.
	//
	// The previous loop simply gave up after 30s and returned, leaving a stale
	// volume whose driver_opts point at a device path from THAT run. The next
	// run then mounts a deleted path, its service never starts, and the failure
	// reads as a bug in the deploy path — which is exactly how this presented.
	//
	// So: wait long enough (scaled for slow hosts), and when it still fails,
	// name what is holding the volume instead of leaving a landmine behind.
	waitFor := e2eTimeout(90 * time.Second)
	deadline := time.Now().Add(waitFor)
	var lastOut string
	for {
		out, err := dockerRun(ctx, "volume", "rm", volume)
		if err == nil {
			return
		}
		if !strings.Contains(out, "in use") {
			return // gone already, or not a condition retrying will fix
		}
		lastOut = out
		if time.Now().After(deadline) {
			holders, _ := dockerRun(ctx, "ps", "-a", "--filter", "volume="+volume,
				"--format", "{{.ID}} {{.Names}} {{.Status}} {{.Label \"com.docker.swarm.service.name\"}}")
			t.Logf("volume %s still in use after %s — containers holding it:\n%s\nlast error: %s",
				volume, waitFor, holders, strings.TrimSpace(lastOut))
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// waitBackupAgentsReady waits for the offen agent — and the in-cluster store it
// double-writes to, when that service exists — to be up before it is exec'd.
//
// `backup create` execs `backup` inside the agent container. Triggered before
// the agent has finished starting, that exec exits 1 with NO output at all
// ("docker exec <id> backup: exit status 1 ()"), and Trigger's single 10s retry
// does not cover a cold start. cluster_up_test.go waits on the same service for
// the same reason.
func waitBackupAgentsReady(t *testing.T, ctx context.Context) {
	t.Helper()
	waitServiceHealthy(t, ctx, "backup_volume-backup", 1, 120*time.Second)
	// The store is optional; when the stack ships it, the agent's WebDAV write
	// leg needs it up or the backup fails as a whole.
	if out := mustDockerRun(t, ctx, "service", "ls", "--filter", "name=backup_seaweedfs", "--format", "{{.Name}}"); strings.Contains(out, "backup_seaweedfs") {
		waitServiceHealthy(t, ctx, "backup_seaweedfs", 1, 120*time.Second)
	}
}

// TestBackupRestoreE2E proves a real backup round-trips through a real restore
// into a live volume root.
func TestBackupRestoreE2E(t *testing.T) {

	requireSwarm(t)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not on PATH; skipping backup/restore e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	weInitedSwarm := ensureSwarmActive(t, ctx)
	if weInitedSwarm {
		t.Cleanup(func() {
			t.Log("TestBackupRestoreE2E: leaving Swarm we initialised (cleanup)")
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer leaveCancel()
			out, err := dockerRun(leaveCtx, "swarm", "leave", "--force")
			if err != nil {
				t.Logf("docker swarm leave --force: %v\n%s", err, out)
			}
		})
	}

	homeDir := t.TempDir()
	if stdout, _, code := runCmd(t, homeDir, "init"); code != 0 {
		t.Fatalf("pmcluster init exited %d:\n%s", code, stdout)
	}

	// The control loop would reconcile the stacks underneath the assertions.
	setReconcileInterval(t, homeDir, "0")

	// The volume root must be a path the Docker VM can bind as a volume
	// DEVICE. t.TempDir() lives under /var/folders on macOS: it exists on the
	// host, but not at that path inside Docker Desktop's Linux VM, so a service
	// mounting a volume there never starts and the deploy times out waiting for
	// a replica. A directory under $HOME is shared at the same absolute path on
	// every platform. The pmcluster HOME stays in the temp dir — nothing under
	// it is mounted into a container.
	homeRoot, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home dir for the volume root: %v", err)
	}
	vmVisibleRoot, err := os.MkdirTemp(homeRoot, ".pmcluster-e2e-stack-")
	if err != nil {
		t.Fatalf("create volume-root parent: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(vmVisibleRoot) })
	storageRoot := filepath.Join(vmVisibleRoot, "stack")
	if out, errOut, code := runCmd(t, homeDir, "cluster", "settings", "set", "volume_root="+storageRoot); code != 0 {
		t.Fatalf("set volume_root exited %d:\n%s\n%s", code, out, errOut)
	}

	// Relocate BOTH the volume root and the archive dir under the temp HOME.
	// The agent honours PMCLUSTER_BACKUP_DIR (cluster.up), and the CLI resolves
	// archives against the same directory, so an archive written here is the
	// one the restore looks for.
	backupDir := storageRoot + "/backup"
	t.Setenv("PMCLUSTER_BACKUP_DIR", backupDir)

	certPath, keyPath := generateSelfSignedCert(t, homeDir)
	upOut, _, code := runCmdCtx(t, ctx, homeDir, "cluster", "up",
		"--domain=example.test",
		"--cert="+certPath,
		"--key="+keyPath,
		"--observability-admin-email=admin@example.test",
	)
	if code != 0 {
		t.Fatalf("pmcluster cluster up exited %d:\n%s", code, upOut)
	}

	t.Cleanup(func() {
		t.Log("TestBackupRestoreE2E: running cluster down --yes --purge (cleanup)")
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		out, _, _ := runCmdCtx(t, cleanCtx, homeDir, "cluster", "down", "--yes", "--purge")
		t.Logf("cluster down output:\n%s", out)
	})

	const (
		app      = "e2erestore"
		laterApp = "e2erestore2" // deployed AFTER the backup, on purpose
		volName  = "app_data"
	)

	writeManifest := func(name string) string {
		t.Helper()
		p := filepath.Join(homeDir, "restore-"+name+".yaml")
		if err := os.WriteFile(p, []byte(fmt.Sprintf(restoreManifest, name)), 0o644); err != nil {
			t.Fatalf("write manifest for %s: %v", name, err)
		}
		return p
	}
	manifestPath := writeManifest(app)
	laterManifestPath := writeManifest(laterApp)

	// Clear leftovers from a previous (possibly crashed) run first, then clean
	// up after this one. Registered after the cluster teardown so it runs
	// before it.
	for _, a := range []string{app, laterApp} {
		clearAppState(t, a, a+"_"+volName)
	}
	t.Cleanup(func() {
		for _, a := range []string{app, laterApp} {
			clearAppState(t, a, a+"_"+volName)
		}
	})

	const (
		markerOriginal = "original-data-v1\n"
		markerMutated  = "overwritten-after-backup\n"
		laterFile      = "post-backup.txt"
	)

	// The volume's host path. Every named volume is bind-mounted at
	// <volume_root>/<app>/<volume> (manifest/compose_writer), and that tree is
	// exactly what the agent archives — which is why this path is both the
	// thing to seed and the thing to assert on.
	volumeDir := filepath.Join(storageRoot, app, volName)
	markerPath := filepath.Join(volumeDir, "marker.txt")
	laterVolumeDir := filepath.Join(storageRoot, laterApp, volName)
	laterAppMarker := filepath.Join(laterVolumeDir, "marker.txt")

	t.Run("01-deploy-a-stateful-stack", func(t *testing.T) {
		out, errOut, code := runCmdCtx(t, ctx, homeDir, "deploy", manifestPath)
		if code != 0 {
			t.Fatalf("pmcluster deploy exited %d:\n%s\n%s", code, out, errOut)
		}
		waitServiceHealthy(t, ctx, app+"_store", 1, 120*time.Second)
	})

	t.Run("02-the-volume-lands-under-the-volume-root", func(t *testing.T) {
		if _, err := os.Stat(volumeDir); err != nil {
			t.Fatalf("volume dir %s not created by the deploy: %v", volumeDir, err)
		}
	})

	// Seed the app's data, then back it up. Written from the host on purpose:
	// see the file header on why the service must not write it.
	if err := os.WriteFile(markerPath, []byte(markerOriginal), 0o644); err != nil {
		t.Fatalf("seed %s: %v", markerPath, err)
	}

	var backupID int64
	t.Run("03-an-on-demand-backup-succeeds", func(t *testing.T) {
		waitBackupAgentsReady(t, ctx)
		out, errOut, code := runCmdCtx(t, ctx, homeDir, "backup", "create", "--timeout=5m")
		combined := out + errOut
		t.Logf("backup create output:\n%s", combined)
		if code != 0 {
			t.Fatalf("pmcluster backup create exited %d:\n%s", code, combined)
		}
		m := regexp.MustCompile(`id=(\d+)`).FindStringSubmatch(combined)
		if m == nil {
			t.Fatalf("no backup id in output:\n%s", combined)
		}
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatalf("parse backup id %q: %v", m[1], err)
		}
		backupID = id

		// The archive must exist on THIS node's archive dir, or a local-first
		// restore has nothing to read and the run silently depends on an
		// offsite fetch.
		entries, err := os.ReadDir(backupDir)
		if err != nil {
			t.Fatalf("read archive dir %s: %v", backupDir, err)
		}
		var archives []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tar.gz") {
				archives = append(archives, e.Name())
			}
		}
		if len(archives) == 0 {
			t.Fatalf("backup %d reported success but %s holds no .tar.gz archive", backupID, backupDir)
		}
		t.Logf("archive(s) on disk: %v", archives)
	})

	t.Run("04-the-run-is-recorded-as-succeeded-and-browsable", func(t *testing.T) {
		out, _, code := runCmd(t, homeDir, "backup", "list")
		if code != 0 {
			t.Fatalf("pmcluster backup list exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, strconv.FormatInt(backupID, 10)) {
			t.Errorf("backup list does not mention run %d:\n%s", backupID, out)
		}

		out, errOut, code := runCmd(t, homeDir, "backup", "browse", strconv.FormatInt(backupID, 10))
		if code != 0 {
			t.Fatalf("pmcluster backup browse exited %d:\n%s\n%s", code, out, errOut)
		}
		if !strings.Contains(out, "marker.txt") {
			t.Errorf("archive contents do not include the seeded marker.txt — the agent did not archive the volume:\n%s", out)
		}
	})

	// A restore REPLACES: the app is rewound to the snapshot, so the mutated
	// file comes back as it was AND anything written into the volume since is
	// gone. Both halves are asserted, because the second half is what makes a
	// database volume come back consistent rather than as a union of two
	// moments.
	//
	// It also asserts the safety half of the same rule: an app deployed AFTER
	// the backup was never in the snapshot, so a whole-run restore must leave
	// it alone. Without that bound, replacing would be a loaded gun pointed at
	// everything deployed since the backup.
	t.Run("05-restore-replaces-the-volume-and-spares-apps-the-snapshot-never-saw", func(t *testing.T) {
		if err := os.WriteFile(markerPath, []byte(markerMutated), 0o644); err != nil {
			t.Fatalf("overwrite %s: %v", markerPath, err)
		}
		laterPath := filepath.Join(volumeDir, laterFile)
		if err := os.WriteFile(laterPath, []byte("created after the backup\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", laterPath, err)
		}

		// A second stateful app, deployed after the backup, holding real data.
		deployOut, deployErr, code := runCmdCtx(t, ctx, homeDir, "deploy", laterManifestPath)
		if code != 0 {
			t.Fatalf("deploy %s exited %d:\n%s\n%s", laterApp, code, deployOut, deployErr)
		}
		waitServiceHealthy(t, ctx, laterApp+"_store", 1, 120*time.Second)
		if err := os.WriteFile(laterAppMarker, []byte("a newer app's data\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", laterAppMarker, err)
		}

		out, errOut, code := runCmdCtx(t, ctx, homeDir, "backup", "restore", strconv.FormatInt(backupID, 10))
		combined := out + errOut
		t.Logf("restore output:\n%s", combined)
		if code != 0 {
			t.Fatalf("pmcluster backup restore exited %d:\n%s", code, combined)
		}

		got, err := os.ReadFile(markerPath)
		if err != nil {
			t.Fatalf("read %s after restore: %v", markerPath, err)
		}
		if string(got) != markerOriginal {
			t.Errorf("marker.txt after restore = %q, want the archived bytes %q — the restore did not rewind the volume",
				string(got), markerOriginal)
		}
		if _, err := os.Stat(laterPath); err == nil {
			t.Errorf("%s still exists after the restore — the volume was merged rather than replaced, "+
				"so it is a union of two points in time instead of the state the snapshot captured", laterFile)
		}
		if _, err := os.Stat(laterAppMarker); err != nil {
			t.Errorf("%s' data was removed by a restore of a snapshot that never contained it: %v — "+
				"the replacement scope must come from the snapshot, not from the destination volume root", laterApp, err)
		}
	})
}

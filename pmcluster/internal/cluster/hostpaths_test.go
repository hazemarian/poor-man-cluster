package cluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withPlatformHostDirs swaps the workflow seam for the duration of a test and
// returns a pointer to the call count.
func withPlatformHostDirs(t *testing.T, fn func() error) *int {
	t.Helper()
	orig := ensurePlatformHostDirs
	t.Cleanup(func() { ensurePlatformHostDirs = orig })
	calls := 0
	ensurePlatformHostDirs = func() error {
		calls++
		return fn()
	}
	return &calls
}

// TestEnsureHostDir_CreatesAMissingDirectory is the whole point of the helper:
// Swarm rejects a bind whose source path does not exist, so the platform
// creates it.
func TestEnsureHostDir_CreatesAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nodestat", "nested")
	if err := ensureHostDir(dir); err != nil {
		t.Fatalf("ensureHostDir: %v", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat created dir: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

// TestEnsureHostDir_NoOpWhenPresent: an existing directory is left exactly as
// it is — no re-mkdir, no chmod. An operator's 0700 (or a directory another
// component owns) must not be rewritten by a reconcile.
func TestEnsureHostDir_NoOpWhenPresent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nodestat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	marker := filepath.Join(dir, NodeStatFileName)
	if err := os.WriteFile(marker, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := ensureHostDir(dir); err != nil {
		t.Fatalf("ensureHostDir on an existing dir: %v", err)
	}
	if err := ensureHostDir(dir); err != nil {
		t.Fatalf("ensureHostDir is not idempotent: %v", err)
	}

	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %o, want 0700 untouched", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("existing file under the dir was disturbed: %v", err)
	}
}

// TestEnsureHostDir_ReportsAnUnwritableParent: the error has to name the path
// and carry the reason, because the operator's fix (chown, remount rw) is
// chosen from it.
func TestEnsureHostDir_ReportsAnUnwritableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the failure cannot be provoked")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	dir := filepath.Join(parent, "nodestat")
	err := ensureHostDir(dir)
	if err == nil {
		t.Fatalf("ensureHostDir under a read-only parent returned nil, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, dir) {
		t.Errorf("error %q does not name the path %q", msg, dir)
	}
	if !strings.Contains(msg, "bind-mounts") || !strings.Contains(msg, "permission denied") {
		t.Errorf("error %q does not carry the reason (a bind mount needs it; permission denied)", msg)
	}
}

// TestEnsureHostDir_RefusesAFile: a path that exists but is not a directory is
// the one case a plain MkdirAll would report as success and Docker would still
// reject, so it is an error with its own sentence.
func TestEnsureHostDir_RefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodestat")
	if err := os.WriteFile(path, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	err := ensureHostDir(path)
	if err == nil {
		t.Fatal("ensureHostDir on a regular file returned nil")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %q, want the path and \"is not a directory\"", err)
	}
}

// TestEnsureHostDirs_ReportsEveryFailure: one unwritable path must not hide the
// others, and no errors means nil.
func TestEnsureHostDirs_ReportsEveryFailure(t *testing.T) {
	root := t.TempDir()
	orig := hostMkdirAll
	t.Cleanup(func() { hostMkdirAll = orig })
	hostMkdirAll = func(path string, _ os.FileMode) error {
		return fmt.Errorf("mkdir %s: read-only file system", path)
	}

	dirs := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
	err := ensureHostDirs(dirs)
	if err == nil {
		t.Fatal("ensureHostDirs returned nil with a failing mkdir")
	}
	for _, dir := range dirs {
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not name %q", err, dir)
		}
	}
	if !strings.Contains(err.Error(), "read-only file system") {
		t.Errorf("error %q does not carry the underlying reason", err)
	}

	if err := ensureHostDirs(nil); err != nil {
		t.Fatalf("ensureHostDirs(nil) = %v, want nil", err)
	}
}

// TestNodeStatHostDirIsTheDocumentedPath pins the value once: the path is in
// docs/observability.md and in the daemon's reader, so it is not a free knob.
func TestNodeStatHostDirIsTheDocumentedPath(t *testing.T) {
	if NodeStatHostDir != "/var/stack/nodestat" {
		t.Fatalf("NodeStatHostDir = %q, want /var/stack/nodestat", NodeStatHostDir)
	}
	if got, want := NodeStatFilePath(), "/var/stack/nodestat/nodestat.jsonl"; got != want {
		t.Fatalf("NodeStatFilePath() = %q, want %q", got, want)
	}
}

// TestUp_CreatesPlatformHostDirsBeforeTheObservabilityDeploy drives Up far
// enough to observe the call, and pins the ORDER: a bind source created after
// the deploy would be created too late to help.
func TestUp_CreatesPlatformHostDirsBeforeTheObservabilityDeploy(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, _, deployer := newUpDeps(t)

	var observabilityDeployed bool
	calls := withPlatformHostDirs(t, func() error {
		for _, s := range deployer.deployedStacks {
			if s.Name == string(StackObservability) {
				observabilityDeployed = true
			}
		}
		return nil
	})

	if _, err := Up(context.Background(), deps, UpInput{
		Domain:                  "test.example.com",
		CertPath:                certPath,
		KeyPath:                 keyPath,
		ObservabilityAdminEmail: "ops@example.com",
	}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("ensurePlatformHostDirs called %d times during Up, want 1", *calls)
	}
	if observabilityDeployed {
		t.Fatal("the observability stack was already deployed when the host dirs were ensured — the mkdir must come first")
	}
}

// TestUp_FailsWhenAPlatformHostDirCannotBeCreated: a fresh install has no
// healthy cluster to preserve, so the failure is fatal and names the path
// before anything is deployed.
func TestUp_FailsWhenAPlatformHostDirCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	certPath := writeTempFile(t, dir, "cert.pem", []byte("CERT"))
	keyPath := writeTempFile(t, dir, "key.pem", []byte("KEY"))

	deps, _, deployer := newUpDeps(t)
	withPlatformHostDirs(t, func() error {
		return fmt.Errorf("create host path %s (a platform container bind-mounts it): permission denied", NodeStatHostDir)
	})

	_, err := Up(context.Background(), deps, UpInput{
		Domain:                  "test.example.com",
		CertPath:                certPath,
		KeyPath:                 keyPath,
		ObservabilityAdminEmail: "ops@example.com",
	})
	if err == nil {
		t.Fatal("Up returned nil, want the host-path failure")
	}
	if !strings.Contains(err.Error(), NodeStatHostDir) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Up error = %q, want the path and the reason", err)
	}
	if len(deployer.deployedStacks) != 0 {
		t.Fatalf("Up deployed %v before failing on a host path", deployer.deployedStacks)
	}
}

// TestUpdate_CreatesPlatformHostDirs: an existing cluster gets the same mkdir
// on every reconcile, because the directory may have never existed (the
// collector code is newer than the node).
func TestUpdate_CreatesPlatformHostDirs(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	var out bytes.Buffer
	deps.Stdout = &out

	calls := withPlatformHostDirs(t, func() error { return nil })

	if _, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("ensurePlatformHostDirs called %d times during Update, want 1", *calls)
	}
	if !strings.Contains(out.String(), NodeStatHostDir+" ready") {
		t.Fatalf("update did not report the host dir as ready; stdout:\n%s", out.String())
	}
}

// TestUpdate_WarnsWhenAPlatformHostDirCannotBeCreated: the reconcile must not
// abort — every other stack is healthy — and the failure must reach the apply
// record, because the reconcile runs unattended.
func TestUpdate_WarnsWhenAPlatformHostDirCannotBeCreated(t *testing.T) {
	deps, cfgDir := seedUpdateState(t)
	var out bytes.Buffer
	deps.Stdout = &out

	withPlatformHostDirs(t, func() error {
		return fmt.Errorf("create host path %s (a platform container bind-mounts it): read-only file system", NodeStatHostDir)
	})

	if _, err := Update(context.Background(), deps, UpdateInput{ConfigDir: cfgDir, Version: "v0.3.0"}); err != nil {
		t.Fatalf("Update aborted on a host path it could not create: %v", err)
	}

	if !strings.Contains(out.String(), "⚠") || !strings.Contains(out.String(), NodeStatHostDir) {
		t.Fatalf("update printed no warning naming the path; stdout:\n%s", out.String())
	}

	rec := LoadApplyRecord(context.Background(), deps.Store)
	if rec == nil {
		t.Fatal("no apply record written")
	}
	joined := strings.Join(rec.Warnings, "\n")
	if !strings.Contains(joined, NodeStatHostDir) {
		t.Fatalf("apply warnings do not name %s: %#v", NodeStatHostDir, rec.Warnings)
	}
	if !strings.Contains(joined, "read-only file system") {
		t.Fatalf("apply warnings do not carry the reason: %#v", rec.Warnings)
	}
}

// TestEnsurePlatformHostDirsRealRunsThePlatformList keeps the seam honest: the
// workflow's mkdir must walk platformHostDirs() — the same list the drift test
// compares against the rendered bind source.
func TestEnsurePlatformHostDirsRealRunsThePlatformList(t *testing.T) {
	origMkdir := hostMkdirAll
	t.Cleanup(func() { hostMkdirAll = origMkdir })

	root := t.TempDir()
	platDirs := platformHostDirs()
	if len(platDirs) == 0 {
		t.Fatal("platformHostDirs() is empty — nothing would be created")
	}

	var made []string
	hostMkdirAll = func(path string, mode os.FileMode) error {
		made = append(made, path)
		if mode != hostPathMode {
			t.Errorf("mkdir %s with mode %o, want %o", path, mode, hostPathMode)
		}
		return os.MkdirAll(filepath.Join(root, filepath.Base(path)), mode)
	}
	if err := ensurePlatformHostDirsReal(); err != nil {
		t.Fatalf("ensurePlatformHostDirsReal: %v", err)
	}
	if len(made) != len(platDirs) || made[0] != platDirs[0] {
		t.Fatalf("the real helper created %v, want the platform list %v", made, platDirs)
	}
}

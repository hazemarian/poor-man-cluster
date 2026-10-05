package stacks

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeMoveTrigger is a test-local BackupTrigger returning canned paths.
type fakeMoveTrigger struct {
	paths []string
	err   error
}

func (f *fakeMoveTrigger) Trigger(context.Context) ([]string, error) { return f.paths, f.err }

// moveFakeDocker is a test-local runtime.Client exposing a canned NodeList.
type moveFakeDocker struct {
	runtime.Client
	nodes []runtime.Node
	err   error
}

func (d *moveFakeDocker) NodeList(context.Context) ([]runtime.Node, error) { return d.nodes, d.err }

// writeMoveArchive writes a tar.gz archive containing the given entries
// (name → content), all under the offen `backup/data` prefix style.
func writeMoveArchive(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
}

// moveDemoManifest is a stateful stack with NO explicit placement, so the
// PinResolver round-robin (or a stack pin) decides its node.
const moveDemoManifest = `
app: demo
env: production
domain: example.com
services:
  db:
    image: postgres:14-alpine
    volumes: [db_data:/var/lib/postgresql/data]
    env:
      POSTGRES_DB: demo
      POSTGRES_USER: user
`

const moveStatelessManifest = `
app: stateless-demo
env: production
domain: example.com
services:
  api:
    image: nginx:alpine
`

// moveTestSvc builds a Service wired for move tests: a fresh store, a
// recordingDeployer, round-robin over [node-a, node-b], temp VolumeRoot and
// BackupDir, and the given backup trigger.
func moveTestSvc(t *testing.T, trigger BackupTrigger, docker runtime.Client) (*Service, *recordingDeployer) {
	t.Helper()
	s := openTestStore(t)
	dep := &recordingDeployer{}
	svc := &Service{
		Store:      s,
		Deployer:   dep,
		Docker:     docker,
		Backup:     trigger,
		VolumeRoot: filepath.Join(t.TempDir(), "stack"),
		BackupDir:  t.TempDir(),
		Pins: &PinResolver{
			StorageNodes: []string{"node-a", "node-b"},
		},
		MkdirAll: func(string, os.FileMode) error { return nil },
	}
	return svc, dep
}

// TestMove_LocalRestore verifies the full local-restore path: Docker nil (CLI
// on this host) triggers a backup, extracts the <stack>/ subtree into the
// VolumeRoot, writes the stack_pin_<stack> setting and re-deploys.
func TestMove_LocalRestore(t *testing.T) {
	svc, dep := moveTestSvc(t, nil, nil)

	// Record a deployed revision for the stateful stack.
	if _, err := svc.Deploy(context.Background(), Payload{AppName: "demo", Manifest: moveDemoManifest}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Handcraft the archive the (fake) trigger reports.
	archive := filepath.Join(svc.BackupDir, "backup-node0-1234567890.tar.gz")
	writeMoveArchive(t, archive, map[string]string{
		"backup/data/demo/db_data/PG_VERSION": "15",
	})
	svc.Backup = &fakeMoveTrigger{paths: []string{"/archive/backup-node0-1234567890.tar.gz"}}

	// Pick a target that differs from the round-robin-resolved pin.
	pins, err := svc.StoragePinsForStack(context.Background(), "demo")
	if err != nil {
		t.Fatalf("pins: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected 1 pin, got %v", pins)
	}
	target := "node-a"
	if pins[0] == "node-a" {
		target = "node-b"
	}

	before := dep.callCount()
	if err := svc.Move(context.Background(), "demo", target); err != nil {
		t.Fatalf("move: %v", err)
	}

	// Pin setting persisted.
	if got := svc.Store.GetSettingDefault(context.Background(), StackPinKey("demo"), ""); got != target {
		t.Fatalf("pin = %q, want %q", got, target)
	}
	// Subtree extracted into the volume root.
	data, err := os.ReadFile(filepath.Join(svc.VolumeRoot, "demo", "db_data", "PG_VERSION"))
	if err != nil {
		t.Fatalf("restored file: %v", err)
	}
	if string(data) != "15" {
		t.Fatalf("restored content = %q", data)
	}
	// Re-deploy ran (new revision).
	if dep.callCount() <= before {
		t.Fatalf("expected redeploy, calls %d -> %d", before, dep.callCount())
	}
	// Backup row recorded as succeeded.
	rows, err := svc.Store.ListBackups(context.Background(), 5)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(rows) == 0 || rows[0].Status != "succeeded" {
		t.Fatalf("backup row missing/not succeeded: %+v", rows)
	}
}

// TestMove_LocalRestore_DockerOnLocalHost verifies the branch where Docker IS
// reachable but the target is this host: restore stays local (no mover).
func TestMove_LocalRestore_DockerOnLocalHost(t *testing.T) {
	host, _ := os.Hostname()
	svc, dep := moveTestSvc(t, nil, &moveFakeDocker{nodes: []runtime.Node{
		{Hostname: host, Status: "ready", Availability: "active", IsLeader: true},
		{Hostname: "node-a", Status: "ready", Availability: "active"},
	}})
	if _, err := svc.Deploy(context.Background(), Payload{AppName: "demo", Manifest: moveDemoManifest}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	archive := filepath.Join(svc.BackupDir, "backup-node0-1234567890.tar.gz")
	writeMoveArchive(t, archive, map[string]string{
		"backup/data/demo/db_data/PG_VERSION": "15",
	})
	svc.Backup = &fakeMoveTrigger{paths: []string{"/archive/backup-node0-1234567890.tar.gz"}}

	if err := svc.Move(context.Background(), "demo", host); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(svc.VolumeRoot, "demo", "db_data", "PG_VERSION")); err != nil {
		t.Fatalf("restored file: %v", err)
	}
	if dep.callCount() == 0 {
		t.Fatal("expected a redeploy")
	}
}

// TestMove_AlreadyOnTarget errors when the resolved pin already equals the
// target node.
func TestMove_AlreadyOnTarget(t *testing.T) {
	svc, _ := moveTestSvc(t, nil, nil)
	if _, err := svc.Deploy(context.Background(), Payload{AppName: "demo", Manifest: moveDemoManifest}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	svc.Backup = &fakeMoveTrigger{paths: []string{"/archive/backup-node0-1.tar.gz"}}
	if err := svc.Store.SetSetting(context.Background(), StackPinKey("demo"), "node-a"); err != nil {
		t.Fatalf("set pin: %v", err)
	}
	err := svc.Move(context.Background(), "demo", "node-a")
	if err == nil || !strings.Contains(err.Error(), "already on node-a") {
		t.Fatalf("err = %v, want already-on", err)
	}
}

// TestMove_NoStatefulStorage errors for a stack with no volume-holding
// services.
func TestMove_NoStatefulStorage(t *testing.T) {
	svc, _ := moveTestSvc(t, &fakeMoveTrigger{}, nil)
	if _, err := svc.Deploy(context.Background(), Payload{AppName: "stateless-demo", Manifest: moveStatelessManifest}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	err := svc.Move(context.Background(), "stateless-demo", "node-a")
	if err == nil || !strings.Contains(err.Error(), "no stateful storage") {
		t.Fatalf("err = %v, want no-stateful-storage", err)
	}
}

// TestMove_UnknownStack errors for a stack with no revisions.
func TestMove_UnknownStack(t *testing.T) {
	svc, _ := moveTestSvc(t, &fakeMoveTrigger{}, nil)
	err := svc.Move(context.Background(), "nope", "node-a")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found", err)
	}
}

// TestMove_EmptyTarget rejects an empty --to.
func TestMove_EmptyTarget(t *testing.T) {
	svc, _ := moveTestSvc(t, &fakeMoveTrigger{}, nil)
	if _, err := svc.Deploy(context.Background(), Payload{AppName: "demo", Manifest: moveDemoManifest}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	err := svc.Move(context.Background(), "demo", "")
	if err == nil || !strings.Contains(err.Error(), "target node is empty") {
		t.Fatalf("err = %v, want empty-target", err)
	}
}

// TestMove_NoBackupTrigger fails loudly when the Service has no Backup.
func TestMove_NoBackupTrigger(t *testing.T) {
	svc, _ := moveTestSvc(t, nil, nil)
	if _, err := svc.Deploy(context.Background(), Payload{AppName: "demo", Manifest: moveDemoManifest}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	err := svc.Move(context.Background(), "demo", "node-b")
	if err == nil || !strings.Contains(err.Error(), "no backup trigger") {
		t.Fatalf("err = %v, want no-backup-trigger", err)
	}
}

// TestMove_TargetNodeValidation rejects a target that is missing or not
// ready/active in the swarm.
func TestMove_TargetNodeValidation(t *testing.T) {
	cases := []struct {
		name  string
		nodes []runtime.Node
		want  string
	}{
		{
			name:  "missing",
			nodes: []runtime.Node{{Hostname: "node-a", Status: "ready", Availability: "active"}},
			want:  "not found in swarm",
		},
		{
			name:  "down",
			nodes: []runtime.Node{{Hostname: "node-b", Status: "down", Availability: "active"}, {Hostname: "node-a", Status: "ready", Availability: "active"}},
			want:  "not ready/active",
		},
		{
			name:  "drained",
			nodes: []runtime.Node{{Hostname: "node-b", Status: "ready", Availability: "drain"}, {Hostname: "node-a", Status: "ready", Availability: "active"}},
			want:  "not ready/active",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := moveTestSvc(t, &fakeMoveTrigger{}, &moveFakeDocker{nodes: tc.nodes})
			if _, err := svc.Deploy(context.Background(), Payload{AppName: "demo", Manifest: moveDemoManifest}); err != nil {
				t.Fatalf("deploy: %v", err)
			}
			svc.Backup = &fakeMoveTrigger{paths: []string{"/archive/backup-node0-1.tar.gz"}}
			err := svc.Move(context.Background(), "demo", "node-b")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestExtractStackSubtree covers the extraction helper directly: prefix
// stripping, prefix scoping, and path-escape rejection.
func TestExtractStackSubtree(t *testing.T) {
	volRoot := t.TempDir()
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	writeMoveArchive(t, archive, map[string]string{
		"backup/data/demo/db_data/PG_VERSION":  "15",
		"backup/data/demo/logs/app.log":        "hi",
		"backup/data/other/db_data/PG_VERSION": "14",
	})
	n, err := extractStackSubtree(archive, volRoot, "demo")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if n != 2 {
		t.Fatalf("restored %d files, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(volRoot, "demo", "db_data", "PG_VERSION")); err != nil {
		t.Fatalf("demo file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(volRoot, "other")); !os.IsNotExist(err) {
		t.Fatalf("other stack leaked into restore: %v", err)
	}

	// Path traversal: an entry that climbs above the volume root never
	// writes outside it — filepath.Clean collapses the ".." against the
	// archive root, and the stack-prefix include filter then skips it. The
	// guard is belt-and-suspenders on top.
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	writeMoveArchive(t, bad, map[string]string{
		"../../evil": "boom",
	})
	if _, err := extractStackSubtree(bad, volRoot, "demo"); err != nil {
		t.Fatalf("traversal entry must be skipped, got error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(volRoot), "evil")); !os.IsNotExist(err) {
		t.Fatalf("traversal escaped the volume root: %v", err)
	}
}

// TestResolveArchivePath prefers an existing host archive named by the
// trigger, falling back to a fresh file when the trigger names nothing.
func TestResolveArchivePath(t *testing.T) {
	svc, _ := moveTestSvc(t, nil, nil)
	before := time.Now().Unix() - 1
	named := filepath.Join(svc.BackupDir, "backup-node0-999.tar.gz")
	writeMoveArchive(t, named, map[string]string{"backup/data/demo/db_data/x": "1"})
	// Age the named archive so the mtime scan cannot confuse it with the
	// fresher file below (the named-path branch still finds it by basename).
	_ = os.Chtimes(named, time.Unix(before, 0), time.Unix(before, 0))
	// Named path exists → used even when an even newer file appeared.
	fresh := filepath.Join(svc.BackupDir, "backup-node0-1000.tar.gz")
	writeMoveArchive(t, fresh, map[string]string{"backup/data/demo/db_data/y": "2"})
	_ = os.Chtimes(fresh, time.Now(), time.Now())

	if got := svc.resolveArchivePath(context.Background(), before, []string{"/archive/backup-node0-999.tar.gz"}); got != named {
		t.Fatalf("got %q, want %q", got, named)
	}
	// No usable trigger path → newest created at/after `before`.
	if got := svc.resolveArchivePath(context.Background(), before, []string{"/archive/nope.txt"}); got != fresh {
		t.Fatalf("fallback got %q, want %q", got, fresh)
	}
}

func TestMoverCandidateURLs_AdvertiseFirstThenLocalIPs(t *testing.T) {
	urls := moverCandidateURLs("10.7.224.12:2377", 35001, "/move/abc/tc7db.tgz")
	if len(urls) == 0 {
		t.Fatal("expected at least the advertise address URL")
	}
	// Advertise address must come first (SplitHostPort strips :2377).
	if urls[0] != "http://10.7.224.12:35001/move/abc/tc7db.tgz" {
		t.Fatalf("first candidate = %q, want advertise address", urls[0])
	}
	// Every candidate must be a distinct host (dedup) and non-loopback.
	seen := map[string]bool{}
	for _, u := range urls {
		if seen[u] {
			t.Fatalf("duplicate candidate %q", u)
		}
		seen[u] = true
		if strings.Contains(u, "127.0.0.1") || strings.Contains(u, "::1") {
			t.Fatalf("loopback candidate leaked: %q", u)
		}
		if !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, "/move/abc/tc7db.tgz") {
			t.Fatalf("malformed candidate %q", u)
		}
	}
}

func TestMoverCandidateURLs_AdvertiseWithoutPort(t *testing.T) {
	urls := moverCandidateURLs("203.0.113.9", 40000, "/x")
	if len(urls) == 0 || urls[0] != "http://203.0.113.9:40000/x" {
		t.Fatalf("bare advertise address not handled: %v", urls)
	}
}

func TestMoverCandidateURLs_EmptyAdvertise(t *testing.T) {
	// Empty advertise: no crash; falls through to local interfaces (may be 0
	// in a sandboxed CI, which is fine — the loop must just not panic).
	urls := moverCandidateURLs("", 40000, "/x")
	if len(urls) > 0 && urls[0] == "" {
		t.Fatalf("empty candidate emitted")
	}
}

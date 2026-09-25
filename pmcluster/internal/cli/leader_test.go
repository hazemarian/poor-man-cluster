package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/config"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// leaderFake embeds runtime.Client so every method is satisfied, and lets the
// test override just NodeList (the only one the leader checks use).
type leaderFake struct {
	runtime.Client
	nodes   []runtime.Node
	nodeErr error
}

func (f *leaderFake) NodeList(context.Context) ([]runtime.Node, error) {
	return f.nodes, f.nodeErr
}

func TestIsSwarmLeader_HappyPath(t *testing.T) {
	f := &leaderFake{nodes: []runtime.Node{
		{ID: "n1", Hostname: "mgr1", IsLeader: false},
		{ID: "n2", Hostname: "mgr2", IsLeader: true},
	}}
	leader, err := isSwarmLeader(context.Background(), f, "mgr2")
	if err != nil {
		t.Fatal(err)
	}
	if !leader {
		t.Fatal("expected mgr2 to be leader")
	}
	notLeader, err := isSwarmLeader(context.Background(), f, "mgr1")
	if err != nil {
		t.Fatal(err)
	}
	if notLeader {
		t.Fatal("expected mgr1 to not be leader")
	}
}

func TestIsSwarmLeader_NodeNotFound(t *testing.T) {
	f := &leaderFake{nodes: []runtime.Node{
		{ID: "n1", Hostname: "mgr1", IsLeader: true},
	}}
	if _, err := isSwarmLeader(context.Background(), f, "ghost"); err == nil {
		t.Fatal("expected error for unknown hostname")
	}
}

// writeCtlplaneArchive drops a control-plane tarball into dir under the
// standard offen name so NewestControlPlaneArchive picks it up. The archive
// mirrors the offen layout: entries under backup/pmcluster/.
func writeCtlplaneArchive(t *testing.T, dir, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "backup/pmcluster", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	body := "sqlite-bytes"
	if err := tw.WriteHeader(&tar.Header{Name: "backup/pmcluster/data.db", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// withArchiveDir points the archive scan at dir for the duration of the test.
func withArchiveDir(t *testing.T, dir string) {
	t.Helper()
	old := controlPlaneArchiveDir
	controlPlaneArchiveDir = dir
	t.Cleanup(func() { controlPlaneArchiveDir = old })
}

func TestEnsureControlPlaneFresh_KeepsCurrentDB(t *testing.T) {
	// Shared-storage (LINSTOR) case: data.db on this node is newer than the
	// newest archive → no restore, existing DB must remain untouched.
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-24*time.Hour))

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data.db")
	if err := os.WriteFile(dbPath, []byte("live-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(dbPath, now, now) // DB newer than archive

	cfg := &config.Config{DataDir: dataDir}
	log := zerolog.Nop()
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("expected NO restore when local DB is current")
	}
	b, _ := os.ReadFile(dbPath)
	if string(b) != "live-db" {
		t.Fatal("current local DB was clobbered")
	}
}

func TestEnsureControlPlaneFresh_RestoresWhenStale(t *testing.T) {
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-2*time.Hour))

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "data.db")
	if err := os.WriteFile(dbPath, []byte("stale-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(dbPath, now.Add(-24*time.Hour), now.Add(-24*time.Hour)) // older than archive

	cfg := &config.Config{DataDir: dataDir}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore when local DB is older than newest archive")
	}
}

func TestEnsureControlPlaneFresh_RestoresWhenMissing(t *testing.T) {
	archiveDir := t.TempDir()
	withArchiveDir(t, archiveDir)
	now := time.Now()
	writeCtlplaneArchive(t, archiveDir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz", now.Add(-2*time.Hour))

	// Empty data dir — no data.db at all. Expect restore.
	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore when no local DB exists")
	}
}

func TestEnsureControlPlaneFresh_NoArchiveKeeps(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	restored, err := ensureControlPlaneFresh(context.Background(), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if restored {
		t.Fatal("expected no restore when no archive exists")
	}
}

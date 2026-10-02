package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTarGz writes a real tar.gz archive containing the given entries under
// an optional prefix (mirrors offen's baked-in `backup/data/` prefix).
func writeTarGz(t *testing.T, dir, name, prefix string, files map[string]string) {
	writeTarGzDirs(t, dir, name, prefix, files)
}

// writeTarGzDirs writes a tar.gz archive whose entries include the prefix
// root directory itself, then every file — matching the layout the offen
// agent produces.
func writeTarGzDirs(t *testing.T, dir, name, prefix string, files map[string]string) {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	// Root dir entry for the prefix.
	if err := tw.WriteHeader(&tar.Header{Name: prefix, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatalf("write prefix dir header: %v", err)
	}
	for rel, content := range files {
		entry := rel
		if prefix != "" {
			entry = prefix + "/" + rel
		}
		hdr := &tar.Header{Name: entry, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", entry, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write body %s: %v", entry, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

func TestRestore_StripsDataPrefix(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	destRoot := t.TempDir()

	// A scheduled whole-disk archive with offen's baked-in /backup/data prefix,
	// including the root dir entry itself (maps to "." — must not trip the
	// escape guard).
	writeTarGzDirs(t, dir, "backup-node1-2026-09-21T03-00-00.tar.gz", "backup/data", map[string]string{
		"abbas/abbas_data/abbas.db":     "db-bytes",
		"demoenv/demoenv_data/notes.md": "note",
	})
	if err := st.RecordDiscoveredBackup(ctx, "backup-node1-2026-09-21T03-00-00.tar.gz",
		filepath.Join(dir, "backup-node1-2026-09-21T03-00-00.tar.gz"), time.Now().Unix()); err != nil {
		t.Fatalf("RecordDiscoveredBackup: %v", err)
	}

	svc := &Local{Store: st}
	rows, err := svc.List(ctx, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].StackName != "" {
		t.Fatalf("whole-disk row must have empty StackName, got %q", rows[0].StackName)
	}

	n, err := svc.Restore(ctx, rows[0].ID, destRoot, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n != 2 {
		t.Fatalf("restored %d files, want 2", n)
	}
	// Files must land at destRoot/<app>/<vol>/... NOT destRoot/<app>/backup/data/...
	for _, want := range []string{
		filepath.Join(destRoot, "abbas/abbas_data/abbas.db"),
		filepath.Join(destRoot, "demoenv/demoenv_data/notes.md"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("restored file missing at %s: %v", want, err)
		}
	}
	bad := filepath.Join(destRoot, "abbas/backup/data/abbas/abbas_data/abbas.db")
	if _, err := os.Stat(bad); err == nil {
		t.Errorf("file must NOT exist under wrong path %s", bad)
	}
}

func TestRestore_RefusesControlPlaneArchive(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()
	destRoot := t.TempDir()

	writeTarGz(t, dir, "pmcluster-ctlplane-node1-2026-09-21T03-00-00.tar.gz", "backup/pmcluster", map[string]string{
		"data.db": "sqlite-bytes",
	})
	if err := st.RecordDiscoveredBackup(ctx, "pmcluster-ctlplane-node1-2026-09-21T03-00-00.tar.gz",
		filepath.Join(dir, "pmcluster-ctlplane-node1-2026-09-21T03-00-00.tar.gz"), time.Now().Unix()); err != nil {
		t.Fatalf("RecordDiscoveredBackup: %v", err)
	}

	svc := &Local{Store: st}
	rows, err := svc.List(ctx, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := svc.Restore(ctx, rows[0].ID, destRoot, RestoreOptions{}); err == nil {
		t.Fatal("Restore of a control-plane archive must be refused")
	}
}

func TestLocalPruneRemovesOldRowsAndFiles(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()

	// Two archives with different mtimes.
	oldName := "backup-old-2026-08-01T03-00-00.tar.gz"
	writeTarGz(t, dir, oldName, "backup/data", map[string]string{"app/data/x": "old"})
	_ = os.Chtimes(filepath.Join(dir, oldName), time.Now().Add(-20*24*time.Hour), time.Now().Add(-20*24*time.Hour))

	newName := "backup-new-2026-09-21T03-00-00.tar.gz"
	writeTarGz(t, dir, newName, "backup/data", map[string]string{"app/data/y": "new"})

	if err := st.RecordDiscoveredBackup(ctx, oldName, filepath.Join(dir, oldName), time.Now().Add(-20*24*time.Hour).Unix()); err != nil {
		t.Fatalf("record old: %v", err)
	}
	if err := st.RecordDiscoveredBackup(ctx, newName, filepath.Join(dir, newName), time.Now().Unix()); err != nil {
		t.Fatalf("record new: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: dir, RetentionDays: 15}
	if _, err := svc.List(ctx, 50); err != nil {
		t.Fatalf("List: %v", err)
	}

	rows, err := st.ListBackups(ctx, 50)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows after prune, want 1 (old one removed)", len(rows))
	}
	if rows[0].StartedAt <= time.Now().Add(-24*time.Hour).Unix() {
		t.Errorf("surviving row should be the new archive (started_at=%d)", rows[0].StartedAt)
	}
	// The old archive file must be gone from disk too.
	if _, err := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Errorf("old archive file should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, newName)); err != nil {
		t.Errorf("new archive file must survive, err = %v", err)
	}
}

func TestRestoreControlPlane_StripsPrefix(t *testing.T) {
	dir := t.TempDir()
	// Control-plane archive: offen mounts ${DATA_DIR}:/backup/pmcluster, so
	// entries carry the backup/pmcluster/ prefix.
	writeTarGzDirs(t, dir, "pmcluster-ctlplane-node1-2026-09-25T03-00-00.tar.gz", "backup/pmcluster", map[string]string{
		"data.db":            "sqlite-bytes",
		".encryption_key":    "key-material",
		"config/config.yaml": "listen_addr: 0.0.0.0:9090",
	})
	newest, err := NewestControlPlaneArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if newest == "" {
		t.Fatal("expected a control-plane archive to be found")
	}
	dataDir := t.TempDir()
	n, err := RestoreControlPlane(newest, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("restored %d files, want 3", n)
	}
	// Files must land at dataDir/<rel> — no backup/pmcluster/ prefix.
	for rel, want := range map[string]string{
		"data.db":            "sqlite-bytes",
		".encryption_key":    "key-material",
		"config/config.yaml": "listen_addr: 0.0.0.0:9090",
	} {
		b, err := os.ReadFile(filepath.Join(dataDir, rel))
		if err != nil {
			t.Fatalf("read restored %s: %v", rel, err)
		}
		if string(b) != want {
			t.Fatalf("restored %s = %q, want %q", rel, b, want)
		}
	}
}

func TestNewestControlPlaneArchive_PicksNewest(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "pmcluster-ctlplane-n1-2026-09-24T03-00-00.tar.gz")
	newf := filepath.Join(dir, "pmcluster-ctlplane-n1-2026-09-25T03-00-00.tar.gz")
	for _, p := range []string{old, newf} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Force the old one to be strictly older.
	os.Chtimes(old, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	got, err := NewestControlPlaneArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != newf {
		t.Fatalf("picked %q, want %q", got, newf)
	}
	// Empty dir → empty result, no error.
	empty := t.TempDir()
	got, err = NewestControlPlaneArchive(empty)
	if err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if got != "" {
		t.Fatalf("empty dir returned %q, want ''", got)
	}
}

// TestBrowse_TarGzListsEntries verifies listing a tar.gz archive produced by
// the offen agent (with the baked-in backup/data/ prefix) returns each entry
// relative to the archive root, with sizes and dir flags.
func TestBrowse_TarGzListsEntries(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	writeTarGzDirs(t, dir, "run.tar.gz", "backup/data", map[string]string{
		"abbas/abbas_data/abbas.db": "sqlite",
		"abbas/config.yaml":         "config",
	})
	svc := &Local{Store: st}
	id, err := st.CreateBackup(context.Background(), "abbas", 1)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(context.Background(), id, "succeeded", filepath.Join(dir, "run.tar.gz"), ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	run, files, err := svc.Browse(context.Background(), id)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if run.Status != "succeeded" {
		t.Errorf("run.Status = %q, want succeeded", run.Status)
	}
	// Root dir entry + 2 files.
	if len(files) != 3 {
		t.Fatalf("files = %d, want 3 (root dir + 2 files)", len(files))
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Path] = true
		if f.IsDir && f.Path != "backup/data" {
			t.Errorf("dir entry %q: only the prefix root should be a dir", f.Path)
		}
	}
	if !got["backup/data/abbas/abbas_data/abbas.db"] || !got["backup/data/abbas/config.yaml"] {
		t.Errorf("missing expected archive entries, got %v", got)
	}
}

// TestBrowse_PlainDirWalksTree verifies listing a raw directory archive
// (dir backup runs) walks the tree and reports relative paths.
func TestBrowse_PlainDirWalksTree(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "raw-dir")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	svc := &Local{Store: st}
	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", root, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	_, files, err := svc.Browse(context.Background(), id)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	found := map[string]bool{}
	for _, f := range files {
		found[f.Path] = true
	}
	if !found["sub"] || !found["sub/file.txt"] {
		t.Errorf("dir walk = %v, want sub + sub/file.txt", found)
	}
}

// TestBrowse_MissingArchiveSurfacesErrorEntry verifies a missing archive file
// yields a Size=-1 error entry rather than failing the whole browse.
func TestBrowse_MissingArchiveSurfacesErrorEntry(t *testing.T) {
	st := newTestStore(t)
	svc := &Local{Store: st}
	id, _ := st.CreateBackup(context.Background(), "demo", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", "/var/stack/backup/ghost.tar.gz", ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	_, files, err := svc.Browse(context.Background(), id)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(files) != 1 || files[0].Size != -1 || files[0].Path != "/var/stack/backup/ghost.tar.gz" {
		t.Errorf("missing archive entry = %+v, want Size=-1", files)
	}
}

// TestBrowse_PendingRunReturnsNoFiles verifies a pending (in-flight) run
// browses to an empty file list.
func TestBrowse_PendingRunReturnsNoFiles(t *testing.T) {
	st := newTestStore(t)
	svc := &Local{Store: st}
	id, _ := st.CreateBackup(context.Background(), "demo", 1) // status defaults to pending
	run, files, err := svc.Browse(context.Background(), id)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if run.Status != "pending" || len(files) != 0 {
		t.Errorf("pending browse = status %q, %d files; want pending + 0", run.Status, len(files))
	}
}

// TestRestore_CopyTreePreservesTree verifies restoring a raw directory
// archive (copyTree path) recreates the nested structure under destRoot.
func TestRestore_CopyTreePreservesTree(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "raw-stack")
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "data", "app.db"), []byte("db"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	svc := &Local{Store: st}
	id, _ := st.CreateBackup(context.Background(), "raw-stack", 1)
	if err := st.FinishBackup(context.Background(), id, "succeeded", root, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	destRoot := t.TempDir()
	n, err := svc.Restore(context.Background(), id, destRoot, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n < 1 {
		t.Errorf("restored files = %d, want >= 1", n)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "data", "app.db")); err != nil {
		t.Errorf("restored app.db missing under %s/data: %v", destRoot, err)
	}
}

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

	n, err := svc.Restore(ctx, rows[0].ID, destRoot)
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
	if _, err := svc.Restore(ctx, rows[0].ID, destRoot); err == nil {
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

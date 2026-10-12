package backups

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An on-demand run records the archive path exactly as the agent printed it:
// the CONTAINER path /archive/<name>. That path does not exist on the host —
// the bytes are in the archive dir — so a local-first restore used to look for
// a file that was never going to be there and report the archive as missing,
// on the node that was holding it. The e2e tier caught this; these pin it.
func TestRestore_ResolvesTheContainerArchivePath(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	archiveDir := t.TempDir()
	destRoot := t.TempDir()

	const name = "backup-node1-2026-10-12T18-48-39.tar.gz"
	writeTarGzDirs(t, archiveDir, name, "backup/data", map[string]string{
		"e2erestore/app_data/marker.txt": "original-data-v1\n",
	})

	id, err := st.CreateBackup(ctx, "", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	// Exactly what parseArchivePaths yields from the agent's stdout.
	if err := st.FinishBackup(ctx, id, StatusSucceeded, "/archive/"+name, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: archiveDir}
	n, err := svc.Restore(ctx, id, destRoot, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v — the archive is in ArchiveDir, so this must not be reported as missing", err)
	}
	if n == 0 {
		t.Fatalf("Restore restored 0 files")
	}

	got, err := os.ReadFile(filepath.Join(destRoot, "e2erestore", "app_data", "marker.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != "original-data-v1\n" {
		t.Errorf("restored content = %q, want %q", string(got), "original-data-v1\n")
	}
}

// Browsing must resolve the same way, or a run whose archive is right there
// lists as a single "missing" entry (size -1) with no contents.
func TestBrowse_ResolvesTheContainerArchivePath(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	archiveDir := t.TempDir()

	const name = "backup-node1-2026-10-12T18-48-39.tar.gz"
	writeTarGzDirs(t, archiveDir, name, "backup/data", map[string]string{
		"e2erestore/app_data/marker.txt": "original-data-v1\n",
	})

	id, err := st.CreateBackup(ctx, "", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, StatusSucceeded, "/archive/"+name, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: archiveDir}
	_, files, err := svc.Browse(ctx, id)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	for _, f := range files {
		if f.Size < 0 {
			t.Errorf("browse reported %q as missing (size %d); the archive is present in ArchiveDir", f.Path, f.Size)
		}
	}
	var sawMarker bool
	for _, f := range files {
		if strings.Contains(f.Path, "marker.txt") {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Errorf("browse did not list marker.txt; got %d entries: %v", len(files), files)
	}
}

// A run can finish as succeeded without recording any archive path (the
// agent's stdout yielded nothing and no archive was found in the archive dir).
// Restoring it restored nothing and returned success, so a script, a CI job or
// an operator mid-incident read the same "✅ Restored 0 file(s)" as a real
// restore. Refusing is the only safe answer.
func TestRestore_RefusesARunWithNoArchivePath(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	destRoot := t.TempDir()

	id, err := st.CreateBackup(ctx, "", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, StatusSucceeded, "", ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: t.TempDir()}
	n, err := svc.Restore(ctx, id, destRoot, RestoreOptions{})
	if err == nil {
		t.Fatalf("Restore of a run with no recorded archive returned success (restored %d files); "+
			"a silent no-op must not look like a restore", n)
	}
	if !strings.Contains(err.Error(), "no archive path recorded") {
		t.Errorf("error should say there is nothing to restore from, got: %v", err)
	}
}

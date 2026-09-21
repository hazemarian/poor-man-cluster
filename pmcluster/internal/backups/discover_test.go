package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDiscoverRecordsScheduledArchives(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	dir := t.TempDir()
	// Two scheduled offen archives on disk (never seen by Trigger).
	writeArchive(t, dir, "backup-node1-2026-09-21T03-00-00.tar.gz")
	writeArchive(t, dir, "pmcluster-ctlplane-node1-2026-09-21T03-00-00.tar.gz")

	svc := &Local{Store: st, ArchiveDir: dir}

	rows, err := svc.List(ctx, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("List returned %d rows, want 2 (scheduled archives discovered)", len(rows))
	}
	for _, r := range rows {
		if r.Status != StatusSucceeded {
			t.Errorf("row %d status = %q, want succeeded", r.ID, r.Status)
		}
		if len(r.ArchivePaths) != 1 || r.ArchivePaths[0] != filepath.Join(dir, filepath.Base(r.ArchivePaths[0])) {
			// ArchivePaths must point at the real file on disk.
			if _, err := os.Stat(r.ArchivePaths[0]); err != nil {
				t.Errorf("row %d archive path %q not on disk: %v", r.ID, r.ArchivePaths[0], err)
			}
		}
		if r.StartedAt == 0 || r.FinishedAt == 0 {
			t.Errorf("row %d missing timestamps", r.ID)
		}
	}
}

func TestLocalDiscoverIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	dir := t.TempDir()
	writeArchive(t, dir, "backup-node1-2026-09-21T03-00-00.tar.gz")

	svc := &Local{Store: st, ArchiveDir: dir}
	if _, err := svc.List(ctx, 50); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if _, err := svc.List(ctx, 50); err != nil {
		t.Fatalf("second List: %v", err)
	}

	rows, err := st.ListBackups(ctx, 50)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("after two Lists got %d rows, want 1 (idempotent)", len(rows))
	}
}

func TestLocalTriggerFallsBackToDiskArchive(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	dir := t.TempDir()
	// The trigger's stdout parser found no /archive token (offen v2 prints
	// "in `/archive`"), but the tarball did land on disk.
	svc := &Local{Store: st, ArchiveDir: dir, Run: func(_ context.Context) ([]string, error) {
		writeArchive(t, dir, "backup-node1-2026-09-21T20-02-56.tar.gz")
		return nil, nil
	}}

	id, paths, err := svc.Trigger(ctx, "", 0)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if len(paths) != 1 || paths[0] != dir+"/backup-node1-2026-09-21T20-02-56.tar.gz" {
		t.Fatalf("paths = %v, want the on-disk archive", paths)
	}
	row, err := st.GetBackup(ctx, id)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if row.ArchivePaths != paths[0] {
		t.Errorf("row.ArchivePaths = %q, want %q", row.ArchivePaths, paths[0])
	}

	// A subsequent List must not double-record the same tarball.
	if _, err := svc.List(ctx, 50); err != nil {
		t.Fatalf("List: %v", err)
	}
	rows, err := st.ListBackups(ctx, 50)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("after Trigger+List got %d rows, want 1", len(rows))
	}
}

func TestLocalDiscoverSkipsNonArchivesAndMissingDir(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	dir := t.TempDir()
	writeArchive(t, dir, "backup-node1-2026-09-21T03-00-00.tar.gz")
	// A non-tarball file must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: dir}
	rows, err := svc.List(ctx, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (only the tarball)", len(rows))
	}

	// Missing dir: discovery must be a silent no-op, List still works.
	svc.ArchiveDir = filepath.Join(dir, "does-not-exist")
	if _, err := svc.List(ctx, 50); err != nil {
		t.Fatalf("List with missing dir: %v", err)
	}
}

func writeArchive(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("fake-tarball"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

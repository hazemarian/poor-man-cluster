package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// A restore REPLACES: the volume ends up as the snapshot captured it, not as a
// union of the snapshot and everything that happened since. The case that
// matters is a database, where a lock file or an append-only log written after
// the snapshot describes a different moment than the data pages beside it.
//
// The scope is the safety property: only the apps the snapshot covered are
// rewound. An app deployed after the backup is not in the archive, so it is not
// a scope root and a whole-disk restore cannot reach it.
func TestRestore_ReplacesTheVolumeRatherThanMergingIt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	archiveDir := t.TempDir()
	destRoot := t.TempDir()

	writeTarGzDirs(t, archiveDir, "backup-node1-snap.tar.gz", "backup/data", map[string]string{
		"e2erestore/app_data/marker.txt": "original-data-v1\n",
	})

	// Live volume: the archived file was modified after the snapshot, a new
	// file appeared in it, and a DIFFERENT app holds data that the snapshot
	// never saw.
	mustWrite(t, filepath.Join(destRoot, "e2erestore", "app_data", "marker.txt"), "overwritten-after-backup\n")
	mustWrite(t, filepath.Join(destRoot, "e2erestore", "app_data", "post-backup.txt"), "created after the backup\n")
	mustWrite(t, filepath.Join(destRoot, "otherapp", "other_data", "keep.txt"), "not in the snapshot\n")

	id := finishedRun(t, st, filepath.Join(archiveDir, "backup-node1-snap.tar.gz"))

	svc := &Local{Store: st, ArchiveDir: archiveDir}
	if _, err := svc.Restore(ctx, id, destRoot, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destRoot, "e2erestore", "app_data", "marker.txt"))
	if err != nil {
		t.Fatalf("read restored marker: %v", err)
	}
	if string(got) != "original-data-v1\n" {
		t.Errorf("marker.txt = %q, want the archived bytes %q", string(got), "original-data-v1\n")
	}

	if _, err := os.Stat(filepath.Join(destRoot, "e2erestore", "app_data", "post-backup.txt")); err == nil {
		t.Errorf("post-backup.txt survived the restore — the volume was merged, not replaced")
	}

	// The safety property: a whole-run restore rewinds the apps the snapshot
	// covered, and only those.
	if _, err := os.Stat(filepath.Join(destRoot, "otherapp", "other_data", "keep.txt")); err != nil {
		t.Errorf("otherapp's data was removed by a restore that never covered it: %v — "+
			"the prune scope must come from the snapshot, not from the destination", err)
	}
}

// With --volume the restore is confined to that volume, so a sibling volume of
// the SAME app must survive. This is the case where a naive "prune everything
// not in the archive" would delete a neighbouring database.
func TestRestore_VolumeScopeLeavesASiblingVolumeAlone(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	archiveDir := t.TempDir()
	destRoot := t.TempDir()

	writeTarGzDirs(t, archiveDir, "backup-node1-snap.tar.gz", "backup/data", map[string]string{
		"e2erestore/app_data/marker.txt": "original-data-v1\n",
	})
	mustWrite(t, filepath.Join(destRoot, "e2erestore", "app_data", "marker.txt"), "overwritten\n")
	mustWrite(t, filepath.Join(destRoot, "e2erestore", "app_data", "post-backup.txt"), "created after\n")
	mustWrite(t, filepath.Join(destRoot, "e2erestore", "other_data", "sibling.txt"), "sibling volume\n")

	id := finishedRun(t, st, filepath.Join(archiveDir, "backup-node1-snap.tar.gz"))

	svc := &Local{Store: st, ArchiveDir: archiveDir}
	if _, err := svc.Restore(ctx, id, destRoot, RestoreOptions{Volume: "e2erestore/app_data"}); err != nil {
		t.Fatalf("Restore --volume: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destRoot, "e2erestore", "app_data", "post-backup.txt")); err == nil {
		t.Errorf("post-backup.txt survived a --volume restore of its own volume")
	}
	if _, err := os.Stat(filepath.Join(destRoot, "e2erestore", "other_data", "sibling.txt")); err != nil {
		t.Errorf("a sibling volume of the same app was removed by a --volume restore of app_data: %v", err)
	}
}

// The ordering guarantee: the snapshot is read before anything is written and
// everything is extracted before anything is removed. A restore that cannot
// read its archive must therefore leave the destination untouched rather than
// half-emptied.
func TestRestore_DoesNotPruneWhenTheArchiveIsMissing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	archiveDir := t.TempDir()
	destRoot := t.TempDir()

	mustWrite(t, filepath.Join(destRoot, "e2erestore", "app_data", "live.txt"), "still needed\n")

	id := finishedRun(t, st, filepath.Join(archiveDir, "does-not-exist.tar.gz"))

	svc := &Local{Store: st, ArchiveDir: archiveDir}
	if _, err := svc.Restore(ctx, id, destRoot, RestoreOptions{}); err == nil {
		t.Fatalf("Restore of a missing archive returned success")
	}

	if _, err := os.Stat(filepath.Join(destRoot, "e2erestore", "app_data", "live.txt")); err != nil {
		t.Errorf("the destination was pruned even though the archive could not be read: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// finishedRun records a whole-disk run that succeeded with one archive path.
func finishedRun(t *testing.T, st *store.Store, archivePath string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateBackup(ctx, "", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if err := st.FinishBackup(ctx, id, StatusSucceeded, archivePath, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	return id
}

package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reading the backups list must never delete anything.
//
// It used to: Local.List and ListForStack both called prune(), which cut every
// row and archive older than RetentionDays with a flat age query — ignoring
// KeepLast (a stack's newest copies) and IncludeLegacy (whole-node archives,
// which planPrune spares on purpose because they are the only backups of a node
// that has not been migrated). Opening the console's backups page therefore
// destroyed archives, and deleted rows for artifacts on other nodes whose files
// the leader cannot even see.
func TestListingBackupsNeverDeletesAnything(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := newTestStore(t)

	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	name := ArtifactName("shop", "node1", old)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if err := st.RecordDiscoveredBackupArtifact(ctx, name, path, old.Unix(), StatusSucceeded, "note", "shop"); err != nil {
		t.Fatalf("record artifact: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: dir, RetentionDays: 3}
	if _, err := svc.List(ctx, 10); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := svc.ListForStack(ctx, "shop"); err != nil {
		t.Fatalf("ListForStack: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("listing deleted the archive: %v\na read must not delete", err)
	}
	if ok, err := st.BackupExistsByPath(ctx, path); err != nil || !ok {
		t.Errorf("listing deleted the audit row (exists=%v, err=%v)", ok, err)
	}
}

// Prune is the explicit path, and it must obey the plan it claims to use:
// keep the newest copies of a stack, leave whole-node archives alone unless
// asked, and remove audit rows only for the artifacts it deleted.
func TestPruneObeysKeepLastAndSparesLegacyArchives(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := newTestStore(t)
	now := time.Now().UTC()

	// Four artifacts of one stack, all outside the 3-day window.
	var stackPaths []string
	for i, age := range []int{10, 9, 8, 7} {
		at := now.Add(-time.Duration(age) * 24 * time.Hour)
		name := ArtifactName("shop", "node1", at)
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write artifact %d: %v", i, err)
		}
		if err := st.RecordDiscoveredBackupArtifact(ctx, name, p, at.Unix(), StatusSucceeded, "", "shop"); err != nil {
			t.Fatalf("record artifact %d: %v", i, err)
		}
		stackPaths = append(stackPaths, p)
	}

	// A whole-node (legacy) archive, also outside the window.
	legacyAt := now.Add(-30 * 24 * time.Hour)
	legacyName := "backup-legacynode001-" + legacyAt.Format(ArtifactTimeLayout) + ArtifactSuffix
	legacyPath := filepath.Join(dir, legacyName)
	if err := os.WriteFile(legacyPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	if err := st.RecordDiscoveredBackupArtifact(ctx, legacyName, legacyPath, legacyAt.Unix(), StatusSucceeded, "", ""); err != nil {
		t.Fatalf("record legacy: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: dir, RetentionDays: 3}
	res, err := svc.Prune(ctx, Retention{KeepHours: 48, KeepDays: 3, KeepLast: 2}, false)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.Deleted) != 2 {
		t.Fatalf("deleted %d artifacts (%v), want the 2 oldest of the stack", len(res.Deleted), res.Deleted)
	}

	for i, p := range stackPaths {
		_, err := os.Stat(p)
		gone := os.IsNotExist(err)
		wantGone := i < 2 // written oldest-first: indexes 0,1 are the oldest two
		if gone != wantGone {
			t.Errorf("artifact %d: gone=%v want %v (%s)", i, gone, wantGone, filepath.Base(p))
		}
		exists, _ := st.BackupExistsByPath(ctx, p)
		if gone && exists {
			t.Errorf("artifact %d was deleted but its audit row survived", i)
		}
		if !gone && !exists {
			t.Errorf("artifact %d survived but its audit row was deleted", i)
		}
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Errorf("a whole-node archive was pruned without --legacy: %v", err)
	}
	if ok, _ := st.BackupExistsByPath(ctx, legacyPath); !ok {
		t.Error("audit row for the whole-node archive was deleted")
	}
}

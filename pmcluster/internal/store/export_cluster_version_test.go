package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportCarriesTheClusterVersion is the property the control-plane kit depends
// on: a promoted leader's counter must describe the data it adopted, not restart at
// the migration's seed. Without the replace selector the copy aborts on the seeded
// row (see the next test), and with it the version travels.
func TestExportCarriesTheClusterVersion(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := src.SetSetting(ctx, fmt.Sprintf("k%d", i), "v"); err != nil {
			t.Fatalf("set setting: %v", err)
		}
	}
	wantVersion, wantGeneratedAt, err := src.ClusterVersion(ctx)
	if err != nil {
		t.Fatalf("ClusterVersion: %v", err)
	}
	if wantVersion <= 1 {
		t.Fatalf("version = %d after three writes, want > 1 (the test would pass on the seed alone)", wantVersion)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	if _, err := Export(srcPath, dstPath, []CopyPlan{
		{Table: "cluster_version", Selector: CopyReplace},
		{Table: "cluster_settings"},
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	exported, err := Open(dstPath)
	if err != nil {
		t.Fatalf("open export: %v", err)
	}
	defer func() { _ = exported.Close() }()

	gotVersion, gotGeneratedAt, err := exported.ClusterVersion(ctx)
	if err != nil {
		t.Fatalf("ClusterVersion on the export: %v", err)
	}
	if gotVersion != wantVersion {
		t.Errorf("exported version = %d, want %d", gotVersion, wantVersion)
	}
	// The stamp is part of the version: a promoted leader must report the SAME
	// generated_at, or every follower reads the freshly adopted data as newer than
	// its mirror for a reason nobody changed.
	if gotGeneratedAt != wantGeneratedAt {
		t.Errorf("exported generated_at = %d, want %d", gotGeneratedAt, wantGeneratedAt)
	}
	if n := countIn(t, exported.DB(), "cluster_settings"); n != 3 {
		t.Errorf("exported cluster_settings = %d, want 3", n)
	}
}

// TestExportPlainCopyCollidesOnTheSeededVersionRow pins WHY CopyReplace exists: the
// destination is created by running the migrations, which seed id = 1, so a verbatim
// copy cannot land. If that ever stops being true, the selector can be deleted — the
// test is the question, not the folklore.
func TestExportPlainCopyCollidesOnTheSeededVersionRow(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	_, err = Export(srcPath, dstPath, []CopyPlan{{Table: "cluster_version"}})
	if err == nil {
		t.Fatal("a plain copy of cluster_version succeeded; the seeded row should collide, which is the reason CopyReplace exists")
	}
	if !strings.Contains(err.Error(), "cluster_version") {
		t.Errorf("error does not name the table: %v", err)
	}
}

// TestExportReplaceIsScopedToTheSeededTable: the selector is narrow on purpose. An
// export that could overwrite any destination table would make "the source wins"
// silently true everywhere.
func TestExportReplaceIsScopedToTheSeededTable(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	_, err = Export(srcPath, filepath.Join(t.TempDir(), "dst.db"), []CopyPlan{{Table: "users", Selector: CopyReplace}})
	if err == nil {
		t.Fatal("replace-copy was accepted for a table that the migrations do not pre-seed")
	}
}

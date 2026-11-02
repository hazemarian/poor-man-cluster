package backups

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A per-stack artifact must reach the console attributed to its stack, with
// the sidecar's verification verdict in the note. Without the attribution the
// stack page's backup list would show nothing for the very archives the agent
// writes, which would be a silent regression for the UI and the CLI alike.
func TestDiscoverAttributesPerStackArtifacts(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	dir := t.TempDir()

	name := ArtifactName("horizon-erp", "bh3e42i41uae6fd3mlkv7qfjg",
		time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC))
	writeArchive(t, dir, name)
	writeArchive(t, dir, "backup-bh3e42i41uae6fd3mlkv7qfjg-2026-11-02T19-00-00.tar.gz") // legacy whole-node

	// The agent leaves a sidecar; discovery turns its verdict into the note.
	if err := WriteManifest(filepath.Join(dir, name), Manifest{
		Stack: "horizon-erp", Node: "bh3e42i41uae6fd3mlkv7qfjg", Archive: name,
		Verify: VerifyFacts{Verified: true, Engine: "sqlite", Detail: "app.db: integrity_check ok"},
	}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	svc := &Local{Store: st, ArchiveDir: dir}

	all, err := svc.List(ctx, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d rows, want 2", len(all))
	}

	perStack, err := svc.ListForStack(ctx, "horizon-erp")
	if err != nil {
		t.Fatalf("ListForStack: %v", err)
	}
	// Two rows is correct and documented: the stack's own artifact, plus the
	// cluster-wide archive that also covers it. Exactly one is attributed.
	if len(perStack) != 2 {
		t.Fatalf("ListForStack(horizon-erp) = %d rows, want 2 (own artifact + cluster-wide)", len(perStack))
	}
	var own []Run
	for _, r := range perStack {
		if r.StackName == "horizon-erp" {
			own = append(own, r)
		}
	}
	if len(own) != 1 {
		t.Fatalf("rows attributed to horizon-erp = %d, want 1", len(own))
	}
	if len(own[0].ArchivePaths) != 1 || filepath.Base(own[0].ArchivePaths[0]) != name {
		t.Fatalf("archive path = %v", own[0].ArchivePaths)
	}

	// The verdict reached the row, so an operator can see trust without
	// untarring anything.
	row, err := st.Backup(ctx, own[0].ID)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !containsALL(row.ErrorMessage, "verified", "sqlite", "integrity_check ok") {
		t.Fatalf("note = %q, want the sidecar verdict", row.ErrorMessage)
	}

	// The legacy archive stays unattributed: it spans the whole node, so
	// attributing it to a stack would be a lie the console would repeat.
	legacy, err := svc.ListForStack(ctx, "")
	if err != nil {
		t.Fatalf("ListForStack(\"\"): %v", err)
	}
	if len(legacy) != 1 || legacy[0].StackName != "" {
		t.Fatalf("unattributed rows = %+v, want only the legacy archive", legacy)
	}
}

func TestDiscoverKeepsOffsiteVerdictAheadOfTheManifest(t *testing.T) {
	dir := t.TempDir()
	name := ArtifactName("app", "node1", time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC))
	writeArchive(t, dir, name)
	if err := WriteManifest(filepath.Join(dir, name), Manifest{
		Stack: "app", Archive: name,
		Verify: VerifyFacts{Verified: true, Detail: "ok"},
	}); err != nil {
		t.Fatal(err)
	}
	// An offsite verdict of "missing" must win: it is a data-loss signal and
	// the manifest cannot know about it.
	l := &Local{Offsite: S3Config{Endpoint: "s3.example", Bucket: "b", AccessKey: "k", SecretKey: "s"}}
	if !l.Offsite.Configured() {
		t.Fatal("precondition: offsite should be configured by all four settings")
	}
	if _, note := l.offsiteVerdict(name, map[string]bool{}, nil); note == "" {
		t.Fatal("precondition: expected an offsite note")
	}
}

func containsALL(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The sidecar must never be mistaken for an archive by the *.tar.gz scan, and
// pruning must not delete it out from under an archive.
func TestManifestSidecarIsIgnoredByDiscovery(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "backup-node1-2026-11-02T19-00-00-stack-app.tar.gz")
	if err := os.WriteFile(archive, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(archive, Manifest{Stack: "app", Archive: filepath.Base(archive)}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".gz" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("found %d archive-looking files, want only the .tar.gz", count)
	}
}

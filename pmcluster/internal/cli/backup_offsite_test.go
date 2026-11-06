package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An offsite check only means something if it looks at the right files: the
// archives a node wrote recently, not last month's, and never the .json
// sidecars (which are not artifacts).
func TestRecentArtifactsSelectsRecentArchivesOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	write("backup-n1-2026-11-06T04-00-00-stack-app.tar.gz", time.Hour)
	write("backup-n1-2026-11-06T04-00-00-stack-app.tar.gz.json", time.Hour)
	write("backup-n1-2026-10-01T04-00-00-stack-app.tar.gz", 40*24*time.Hour)

	got, err := recentArtifacts(dir, 1)
	if err != nil {
		t.Fatalf("recentArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d file(s), want 1: %v", len(got), got)
	}
	if filepath.Ext(got[0]) != ".gz" {
		t.Errorf("selected something that is not an archive: %s", got[0])
	}
}

// A missing archive dir is "nothing to check yet", not a failure — the timer
// runs on nodes that have never produced a backup.
func TestRecentArtifactsToleratesAMissingDirectory(t *testing.T) {
	got, err := recentArtifacts(filepath.Join(t.TempDir(), "nope"), 1)
	if err != nil {
		t.Fatalf("recentArtifacts: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d file(s) from a missing dir", len(got))
	}
}

// The status file is the only durable evidence the check ran, and only the last
// two weeks are kept.
func TestRecordOffsiteCheckKeepsTheLastTwoWeeks(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < offsiteHistoryKeep+3; i++ {
		rec := offsiteCheckRecord{At: time.Now().UTC().Add(time.Duration(i) * time.Hour), Target: "s3:bucket", Checked: 1, Present: 1, OK: true}
		if err := recordOffsiteCheck(dir, rec); err != nil {
			t.Fatalf("recordOffsiteCheck: %v", err)
		}
	}
	var doc offsiteStatusFile
	b, err := os.ReadFile(offsiteStatusPath(dir))
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if len(doc.History) != offsiteHistoryKeep {
		t.Errorf("history = %d entries, want %d", len(doc.History), offsiteHistoryKeep)
	}
	if doc.Last == nil {
		t.Fatal("last record missing")
	}
}

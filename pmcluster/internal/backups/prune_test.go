package backups

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeArtifact creates an artifact + sidecar of a given size and time.
func writeArtifact(t *testing.T, dir, stack string, at time.Time, size int) string {
	t.Helper()
	name := ArtifactName(stack, "node-a", at)
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(`{"m":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func names(res PruneResult) map[string]bool {
	out := map[string]bool{}
	for _, d := range res.Deleted {
		out[d] = true
	}
	return out
}

// The dense window is kept in full; beyond it, one artifact per day survives.
func TestPruneKeepsRecentHourlyAndOnePerDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	policy := Retention{KeepHours: 48, KeepDays: 15, KeepLast: 2}

	var kept, dropped []string
	// 2 hours ago: inside the dense window, must survive.
	kept = append(kept, writeArtifact(t, dir, "app", now.Add(-2*time.Hour), 10))
	// 5 days ago: two artifacts the same day — the newer survives.
	dayA := now.Add(-5 * 24 * time.Hour)
	dropped = append(dropped, writeArtifact(t, dir, "app", dayA, 10))
	kept = append(kept, writeArtifact(t, dir, "app", dayA.Add(3*time.Hour), 10))
	// 40 days ago: outside the sparse window entirely.
	dropped = append(dropped, writeArtifact(t, dir, "app", now.Add(-40*24*time.Hour), 10))

	res, err := PruneArtifacts(dir, policy, now, false)
	if err != nil {
		t.Fatal(err)
	}
	got := names(res)
	for _, n := range kept {
		if got[n] {
			t.Fatalf("deleted %s, which is inside retention", n)
		}
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s should still exist: %v", n, err)
		}
	}
	for _, n := range dropped {
		if !got[n] {
			t.Fatalf("kept %s, which is outside retention", n)
		}
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", n)
		}
		if _, err := os.Stat(filepath.Join(dir, n+".json")); !os.IsNotExist(err) {
			t.Fatalf("%s sidecar survived its archive", n)
		}
	}
}

// A stack whose newest artifact is ancient must still keep copies: retention is
// about age, and "the only backup you have" is not old data.
func TestPruneNeverDeletesTheLastCopyOfAStack(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	// One artifact, a year old.
	name := writeArtifact(t, dir, "quiet-app", now.Add(-365*24*time.Hour), 10)
	if _, err := PruneArtifacts(dir, Retention{KeepHours: 1, KeepDays: 1, KeepLast: 1}, now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Fatalf("the only artifact of a stack was deleted: %v", err)
	}
}

// Whole-node archives belong to the previous producer. Until every node is
// migrated they are the only backups that exist, so they are not touched by
// default.
func TestPruneLeavesLegacyArchivesAloneUnlessAsked(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	// Real grammar: the timestamp uses dashes, not colons.
	legacy := "backup-node-a-2025-08-01T00-00-00.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, legacy), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, dir, "app", now.Add(-30*24*time.Hour), 10)

	res, err := PruneArtifacts(dir, Retention{KeepHours: 1, KeepDays: 1, KeepLast: 1}, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedOld == 0 {
		t.Fatal("the legacy archive was not reported as skipped")
	}
	if _, err := os.Stat(filepath.Join(dir, legacy)); err != nil {
		t.Fatalf("legacy whole-node archive was deleted: %v", err)
	}

	// Explicitly asked, and with a newer sibling so it is not the last copy:
	// now it goes.
	newer := "backup-node-a-2026-11-03T00-00-00.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, newer), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneArtifacts(dir, Retention{KeepHours: 1, KeepDays: 1, KeepLast: 1, IncludeLegacy: true}, now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacy)); !os.IsNotExist(err) {
		t.Fatal("legacy archive survived an explicit IncludeLegacy prune")
	}
	// The newer whole-node archive is the last copy of that node's data, so the
	// KeepLast floor protects it even though it is outside the age window.
	if _, err := os.Stat(filepath.Join(dir, newer)); err != nil {
		t.Fatalf("the last whole-node archive was deleted: %v", err)
	}
}

func TestPruneDryRunDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	// Two copies, so the old one is not the stack's last and is genuinely prunable.
	old := writeArtifact(t, dir, "app", now.Add(-90*24*time.Hour), 42)
	writeArtifact(t, dir, "app", now.Add(-1*time.Hour), 42)
	res, err := PruneArtifacts(dir, Retention{KeepHours: 1, KeepDays: 1, KeepLast: 1}, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Deleted) == 0 {
		t.Fatalf("dry run reported nothing to delete: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, old)); err != nil {
		t.Fatalf("dry run deleted a file: %v", err)
	}
}

// Stacks are judged separately: a long history on one must not make another's
// single recent artifact look old.
func TestPruneJudgesEachStackSeparately(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for d := 1; d <= 30; d++ {
		writeArtifact(t, dir, "busy", now.Add(-time.Duration(d)*24*time.Hour), 5)
	}
	recent := writeArtifact(t, dir, "new", now.Add(-1*time.Hour), 5)
	if _, err := PruneArtifacts(dir, Retention{KeepHours: 48, KeepDays: 7, KeepLast: 1}, now, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, recent)); err != nil {
		t.Fatalf("a recent artifact of a new stack was pruned because another stack was busy: %v", err)
	}
}

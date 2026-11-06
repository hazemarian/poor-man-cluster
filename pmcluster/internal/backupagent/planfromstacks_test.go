package backupagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A directory cannot know that its stack was removed; the control plane can.
// PlanFromStacks is that knowledge, and it reports the leftovers rather than
// dropping them silently.
func TestPlanFromStacksSeparatesOrphansFromLiveStacks(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"shop", "blog", "dep-s2", "lost+found"} {
		if err := os.MkdirAll(filepath.Join(root, name, "data"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}

	plan, err := PlanFromStacks(root, map[string]bool{"shop": true, "blog": true})
	if err != nil {
		t.Fatalf("PlanFromStacks: %v", err)
	}
	names := plan.StackNames()
	if len(names) != 2 || names[0] != "blog" || names[1] != "shop" {
		t.Errorf("stacks = %v, want the two deployed ones", names)
	}
	if len(plan.Orphans) != 1 || plan.Orphans[0] != "dep-s2" {
		t.Errorf("orphans = %v, want [dep-s2]", plan.Orphans)
	}
	if plan.GeneratedAt == 0 {
		t.Error("GeneratedAt is unset — the agent's mtime guard depends on it")
	}
	if plan.VolumeRoot != root {
		t.Errorf("VolumeRoot = %q, want %q", plan.VolumeRoot, root)
	}
}

// OrphanDirs is the other direction: what is on disk that the plan does not cover.
func TestOrphanDirsListsWhatThePlanMisses(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"shop", "ghost", "lost+found"} {
		_ = os.MkdirAll(filepath.Join(root, name), 0o755)
	}
	got, err := OrphanDirs(root, Plan{Stacks: []StackPlan{{Name: "shop"}}})
	if err != nil {
		t.Fatalf("OrphanDirs: %v", err)
	}
	if len(got) != 1 || got[0] != "ghost" {
		t.Errorf("orphans = %v, want [ghost] (lost+found is not a stack)", got)
	}
}

// NewestMtime is the guard that keeps a stale plan from stopping live backups.
func TestNewestMtimeSeesWritesDeepInTheTree(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "shop", "data", "nested")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	// Backdate the whole subtree: creating a directory stamps it, so only a tree
	// where nothing was written has an old newest-mtime.
	_ = filepath.Walk(filepath.Join(root, "shop"), func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
	if got := NewestMtime(filepath.Join(root, "shop")); got > old.Unix()+60 {
		t.Errorf("NewestMtime = %d, want the old timestamp %d", got, old.Unix())
	}
	f := filepath.Join(deep, "x.db")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := NewestMtime(filepath.Join(root, "shop")); got < time.Now().Add(-time.Minute).Unix() {
		t.Errorf("NewestMtime = %d, want the file just written", got)
	}
}

package backupagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// A worker cannot ask the swarm what exists, and (measured) its policy file has
// no S3 credentials — but its own containers know what they mount. That is the
// live signal: a directory something mounts is a stack; a quiet directory nothing
// mounts or has backed up recently is a leftover.
func TestPlanFromMountsSplitsLiveDirectoriesFromLeftovers(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-5 * 24 * time.Hour)
	mk := func(name string, when time.Time) {
		p := filepath.Join(root, name, "data")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_ = filepath.Walk(filepath.Join(root, name), func(q string, _ os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chtimes(q, when, when)
			}
			return nil
		})
	}
	mk("shop", old)     // mounted below → live
	mk("blog", old)     // not mounted, but archived recently → live
	mk("ghost", old)    // nothing at all → orphan
	mk("brandnew", now) // just deployed, container not up yet → live

	mounts := []runtime.HostMount{
		{Source: filepath.Join(root, "shop", "data"), Destination: "/data", Stack: "shop"},
		{Source: "/etc/localtime", Destination: "/etc/localtime"}, // outside the root: ignored
	}
	recent := map[string]bool{"blog": true}

	plan, err := PlanFromMounts(root, mounts, recent, now)
	if err != nil {
		t.Fatalf("PlanFromMounts: %v", err)
	}
	names := plan.StackNames()
	want := []string{"blog", "brandnew", "shop"}
	if len(names) != len(want) {
		t.Fatalf("stacks = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("stacks = %v, want %v", names, want)
		}
	}
	if len(plan.Orphans) != 1 || plan.Orphans[0] != "ghost" {
		t.Errorf("orphans = %v, want [ghost]", plan.Orphans)
	}
	if plan.GeneratedAt == 0 {
		t.Error("GeneratedAt unset — the mtime guard downstream depends on it")
	}
}

// A stopped container still holds its volume: a crashed stack is not a leftover.
func TestPlanFromMountsCountsStoppedContainers(t *testing.T) {
	root := t.TempDir()
	at := time.Now().Add(-30 * 24 * time.Hour)
	dir := filepath.Join(root, "crashed", "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_ = os.Chtimes(dir, at, at)
	_ = os.Chtimes(filepath.Join(root, "crashed"), at, at)

	plan, err := PlanFromMounts(root, []runtime.HostMount{{Source: dir, Stack: "crashed"}}, nil, time.Now())
	if err != nil {
		t.Fatalf("PlanFromMounts: %v", err)
	}
	if len(plan.Stacks) != 1 || plan.Stacks[0].Name != "crashed" {
		t.Errorf("stacks = %v, want [crashed] — its container is still on the node", plan.StackNames())
	}
	if len(plan.Orphans) != 0 {
		t.Errorf("orphans = %v, want none", plan.Orphans)
	}
}

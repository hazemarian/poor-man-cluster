package backupagent

import (
	"os"
	"path/filepath"
	"testing"
)

// The plan must be derivable from the filesystem: an hourly run cannot depend on
// the control plane being reachable, and /var/stack/data/<stack>/… already says
// what the stacks are.
func TestPlanFromDiskDerivesOneStackPerDirectory(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"horizon-erp", "khaal-pvc-cms", "property-management"} {
		if err := os.MkdirAll(filepath.Join(root, name, name+"-data"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanFromDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	if plan.VolumeRoot != root {
		t.Fatalf("volume root = %q", plan.VolumeRoot)
	}
	if len(plan.Stacks) != 3 {
		t.Fatalf("stacks = %v, want the three directories (not lost+found)", plan.Stacks)
	}
	for _, s := range plan.Stacks {
		if s.Name == "lost+found" {
			t.Fatal("lost+found was planned as a stack")
		}
	}
}

func TestPlanFromDiskSkipsFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanFromDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stacks) != 1 || plan.Stacks[0].Name != "app" {
		t.Fatalf("stacks = %v, want only the directory", plan.Stacks)
	}
}

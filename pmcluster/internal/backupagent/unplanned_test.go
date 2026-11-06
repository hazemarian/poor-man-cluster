package backupagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A narrowed run must not report the plan's other stacks as unplanned: they are
// planned, just not asked for this time. A warning that lists live stacks is a
// warning an operator learns to ignore.
func TestUnplannedDirsForUsesThePlanNotTheNarrowedRun(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "ghost", "lost+found"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	plan := Plan{Stacks: []StackPlan{{Name: "a"}, {Name: "b"}}}

	all := UnplannedDirs(root, plan)
	if strings.Join(all, ",") != "ghost" {
		t.Errorf("UnplannedDirs = %v, want [ghost]", all)
	}

	narrowed := UnplannedDirsFor(root, []string{"a"})
	if strings.Join(narrowed, ",") != "b,ghost" {
		t.Errorf("UnplannedDirsFor([a]) = %v, want [b ghost] — b is planned, just not asked for", narrowed)
	}
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backupagent"
)

// The orphan policy is the one place where a stale plan could cost data, so its
// three branches are pinned: a stale orphan is skipped, a directory written to
// since the plan was published is archived anyway, and --include-orphans wins.
func TestApplyOrphanPolicySkipsStaleOrphansOnly(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, mtime time.Time) {
		p := filepath.Join(root, name, "data")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// Every path in the tree: a directory's own mtime moves when anything is
		// created inside it, so a partially backdated tree still looks active.
		_ = filepath.Walk(filepath.Join(root, name), func(q string, _ os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chtimes(q, mtime, mtime)
			}
			return nil
		})
	}
	now := time.Now()
	mk("shop", now.Add(-3*time.Hour))
	mk("ghost", now.Add(-5*time.Hour))   // listed orphan, not touched since
	mk("fresh", now.Add(-1*time.Minute)) // not in the plan, but written to just now

	plan := backupagent.Plan{
		Stacks:      []backupagent.StackPlan{{Name: "shop"}},
		Orphans:     []string{"ghost"},
		GeneratedAt: now.Add(-2 * time.Hour).Unix(),
	}

	var out bytes.Buffer
	got := applyOrphanPolicy(plan, root, &out, false)
	names := strings.Join(got.StackNames(), ",")
	if names != "fresh,shop" {
		t.Errorf("stacks = %q, want the plan's stack plus the one written to since (fresh)", names)
	}
	if strings.Contains(names, "ghost") {
		t.Error("a stale orphan was archived")
	}
	if !strings.Contains(out.String(), "ghost") {
		t.Errorf("a skipped orphan must be named in the output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "fresh") {
		t.Errorf("an adopted directory must be named in the output:\n%s", out.String())
	}

	// --include-orphans keeps everything, which is the operator's escape hatch.
	var out2 bytes.Buffer
	all := applyOrphanPolicy(plan, root, &out2, true)
	if got := strings.Join(all.StackNames(), ","); got != "fresh,ghost,shop" {
		t.Errorf("--include-orphans stacks = %q, want all three", got)
	}
}

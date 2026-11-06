package backupagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "the copies live in the store and the bucket, not on the node" has to delete
// exactly the right files: this node's older artifacts of the stack just
// archived, never another node's, never another stack's, and never the one that
// was just shipped.
func TestPruneLocalToNewestOfStackKeepsOnlyTheNewest(t *testing.T) {
	dir := t.TempDir()
	const node = "nodeabc"
	newest := "backup-" + node + "-2026-11-06T12-00-00-stack-app.tar.gz"
	files := []string{
		newest,
		newest + ".json",
		"backup-" + node + "-2026-11-06T11-00-00-stack-app.tar.gz",
		"backup-" + node + "-2026-11-06T11-00-00-stack-app.tar.gz.json",
		"backup-" + node + "-2026-11-06T10-00-00-stack-app.tar.gz",
		// another stack on this node: untouched
		"backup-" + node + "-2026-11-06T11-00-00-stack-other.tar.gz",
		// another node's artifact: untouched
		"backup-zzz999-2026-11-06T11-00-00-stack-app.tar.gz",
		// control-plane tarball: untouched (it does not parse as an artifact)
		"pmcluster-ctlplane-" + node + "-2026-11-06T03-00-00.tar.gz",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	a := &Agent{Node: node, ArchiveDir: dir}
	removed, err := a.pruneLocalToNewestOfStack("app", newest)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d archive(s), want 2", removed)
	}

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	joined := strings.Join(names, "\n")
	want := map[string]bool{
		newest: true, newest + ".json": true,
		"backup-" + node + "-2026-11-06T11-00-00-stack-other.tar.gz": true,
		"backup-zzz999-2026-11-06T11-00-00-stack-app.tar.gz":         true,
		"pmcluster-ctlplane-" + node + "-2026-11-06T03-00-00.tar.gz": true,
	}
	if len(names) != len(want) {
		t.Fatalf("left %d file(s), want %d:\n%s", len(names), len(want), joined)
	}
	for n := range want {
		if !strings.Contains(joined, n) {
			t.Errorf("%s is missing; left:\n%s", n, joined)
		}
	}
	if len(names) == len(want) {
		return
	}
	for _, keep := range []string{
		newest, newest + ".json",
		"backup-" + node + "-2026-11-06T11-00-00-stack-other.tar.gz",
		"backup-zzz999-2026-11-06T11-00-00-stack-app.tar.gz",
		"pmcluster-ctlplane-" + node + "-2026-11-06T03-00-00.tar.gz",
	} {
		if !strings.Contains(joined, keep) {
			t.Errorf("%s was removed but must be kept; left:\n%s", keep, joined)
		}
	}
	if strings.Contains(joined, "2026-11-06T11-00-00-stack-app.tar.gz") ||
		strings.Contains(joined, "2026-11-06T10-00-00-stack-app.tar.gz") {
		t.Errorf("an older copy of the stack survived:\n%s", joined)
	}
}

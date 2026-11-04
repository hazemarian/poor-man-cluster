package backupagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/backups"
)

// A 19-minute freeze on production stalled every container on the node and cost
// it its swarm membership. The freeze cannot be cancelled, so the only defence
// is to not start the next one.
func TestRunBacksOffAfterALongFreeze(t *testing.T) {
	dir := t.TempDir()
	prev := Result{Node: "n1", Started: 1, Snapshot: backups.SnapshotFacts{FrozenAt: 1_000_000_000, ThawedAt: 1_000_000_000 + 19*60*1_000_000_000}}
	b, _ := json.Marshal(prev)
	if err := os.WriteFile(filepath.Join(dir, "run-1-n1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{
		Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(),
		RunDir: dir, MaxFreezeSeconds: 30,
		Snapshot: &NoSnapshot{Root: "/x"},
	}
	res, err := a.Run(context.Background(), Plan{Node: "n1", Stacks: []StackPlan{{Name: "web"}}})
	if !errors.Is(err, ErrBackingOff) {
		t.Fatalf("got %v, want ErrBackingOff", err)
	}
	if res.Error == "" {
		t.Fatal("a back-off must explain itself in the run record")
	}
}

// And it must be a one-interval cooldown, not a permanent stop: the skipped run
// records no freeze, so the next one proceeds.
func TestBackOffIsOneIntervalOnly(t *testing.T) {
	dir := t.TempDir()
	prev := Result{Node: "n1", Started: 1, Snapshot: backups.SnapshotFacts{FrozenAt: 1, ThawedAt: 1 + 200*1_000_000}}
	b, _ := json.Marshal(prev)
	if err := os.WriteFile(filepath.Join(dir, "run-1-n1.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(),
		RunDir: dir, MaxFreezeSeconds: 30,
		Snapshot: &NoSnapshot{Root: "/x"},
	}
	if _, err := a.Run(context.Background(), Plan{Node: "n1", Stacks: []StackPlan{{Name: "web"}}}); err != nil {
		t.Fatalf("a 200 ms freeze must not trigger the breaker: %v", err)
	}
}

func TestNoHistoryMeansNoBackOff(t *testing.T) {
	a := &Agent{Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(),
		RunDir: t.TempDir(), MaxFreezeSeconds: 30, Snapshot: &NoSnapshot{Root: "/x"}}
	if _, err := a.Run(context.Background(), Plan{Node: "n1", Stacks: []StackPlan{{Name: "web"}}}); err != nil {
		t.Fatalf("first run must proceed: %v", err)
	}
}

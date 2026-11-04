package backupagent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Two runs on one node freeze the same filesystem and tar the same stacks. On
// production that produced a frozen node and a 33-minute orphan run, because the
// lock lived in the systemd unit and a forced run walked straight past it.
func TestARunRefusesWhileAnotherHoldsTheLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "run.lock")

	first := &Agent{Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(),
		LockPath: lock, Snapshot: &NoSnapshot{Root: "/x"}}
	release, err := first.lockRuns()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer release()

	second := &Agent{Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(),
		LockPath: lock, Snapshot: &NoSnapshot{Root: "/x"}}
	if _, err := second.Run(context.Background(), Plan{Node: "n1", Stacks: []StackPlan{{Name: "web"}}}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second run got %v, want ErrAlreadyRunning", err)
	}
}

// A dry run must not be turned away by the run it is diagnosing.
func TestADryRunIgnoresTheLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "run.lock")
	holder := &Agent{Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(), LockPath: lock, Snapshot: &NoSnapshot{Root: "/x"}}
	release, err := holder.lockRuns()
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer release()

	dry := &Agent{Node: "n1", VolumeRoot: t.TempDir(), ArchiveDir: t.TempDir(), LockPath: lock,
		Snapshot: &NoSnapshot{Root: "/x"}, DryRun: true}
	if _, err := dry.Run(context.Background(), Plan{Node: "n1", Stacks: []StackPlan{{Name: "web"}}}); err != nil {
		t.Fatalf("dry run should ignore the lock, got %v", err)
	}
}

package backupagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/lvmlock"
)

// lockIsHeld reports whether somebody else currently holds the LVM lock.
//
// flock is per open file description, so opening the file again inside the same
// process and failing to take the lock means a holder exists — which, in this
// test, can only be the call under test.
func lockIsHeld(t *testing.T) bool {
	t.Helper()
	f, err := os.OpenFile(lvmlock.Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	return true
}

// Every LVM command this package runs has to hold the lock.
//
// The snapshot call was the only guarded one for a while, which left two `lvs`
// and two `lvremove` able to deadlock a live node exactly the way `vgs` and
// `lvextend` did in production. This test fails on the next naked command rather
// than trusting the comment above the struct.
func TestEveryLVMCommandRunsUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	oldPath := lvmlock.Path
	lvmlock.Path = filepath.Join(dir, "lvm.lock")
	t.Cleanup(func() { lvmlock.Path = oldPath })

	var unguarded []string
	execer := func(_ context.Context, name string, args ...string) ([]byte, error) {
		// mount/umount/mkdir are not LVM and take no lock.
		if name == "lvs" || strings.HasPrefix(name, "lv") {
			if !lockIsHeld(t) {
				unguarded = append(unguarded, strings.TrimSpace(name+" "+strings.Join(args, " ")))
			}
		}
		if name == "lvs" {
			return []byte("pmcluster-snap-20260101T000000\n"), nil
		}
		return nil, nil
	}
	s := &LVMSnapshotter{
		VG: "vg-test", LV: "data", SnapDir: filepath.Join(dir, "snap"), Exec: execer,
	}
	ctx := context.Background()
	if _, _, err := s.Snapshot(ctx, "/var/stack/data", time.Unix(1767225600, 0)); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := s.Usage(ctx); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if _, err := s.RemoveStale(ctx); err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}
	if err := s.Drop(ctx, "pmcluster-snap-20260101T000000"); err != nil {
		t.Fatalf("Drop: %v", err)
	}

	if len(unguarded) > 0 {
		t.Errorf("LVM commands ran without the lock: %v\n"+
			"every LVM invocation must go through locked(): an unguarded lvs deadlocks "+
			"against a running lvextend the same way vgs does", unguarded)
	}
}

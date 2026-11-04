package backupagent

import (
	"context"
	"strings"
	"testing"
)

// A killed run never reaches its deferred lvremove. The leftover snapshot holds
// pool blocks until the 80% guard aborts runs for a reason that looks nothing
// like its cause, so the next run has to clear it.
func TestRemoveStaleDropsSnapshotsLeftByAnInterruptedRun(t *testing.T) {
	var calls []string
	l := &LVMSnapshotter{
		VG: "vg-pmcluster", LV: "data",
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			switch name {
			case "lvs":
				return []byte("  pmcluster-snap-20261104T005626\n  pmcluster-snap-20261104T004512\n"), nil
			case "umount":
				return []byte("umount: /run/pmcluster-backup/snap: not mounted\n"), nil
			}
			return nil, nil
		},
	}

	gone, err := l.RemoveStale(context.Background())
	if err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}
	if len(gone) != 2 {
		t.Fatalf("removed %v, want both snapshots", gone)
	}
	var lvremoves int
	for _, c := range calls {
		if strings.HasPrefix(c, "lvremove -y vg-pmcluster/pmcluster-snap-") {
			lvremoves++
		}
	}
	if lvremoves != 2 {
		t.Fatalf("lvremove called %d times, want 2; calls: %v", lvremoves, calls)
	}
}

func TestRemoveStaleIsQuietWhenNothingIsLeft(t *testing.T) {
	var calls []string
	l := &LVMSnapshotter{
		VG: "vg-pmcluster", LV: "data",
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name)
			return nil, nil
		},
	}
	gone, err := l.RemoveStale(context.Background())
	if err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("removed %v from an empty listing", gone)
	}
	for _, c := range calls {
		if c == "lvremove" {
			t.Fatalf("lvremove ran with nothing to remove: %v", calls)
		}
	}
}

func TestParseLVSNames(t *testing.T) {
	got := parseLVSNames("  a\n\tb  \n\n  c\n")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

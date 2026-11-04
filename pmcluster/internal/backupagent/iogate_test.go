package backupagent

import (
	"context"
	"strings"
	"testing"
)

// The freeze cannot be cancelled, so a saturated node has to be refused before
// the ioctl is issued — not timed out after it.
func TestGateRefusesASaturatedNode(t *testing.T) {
	busy := &ioGate{MaxPressure: 60, MaxDirtyBytes: 256 << 20, Read: func() (float64, int64, error) {
		return 71.5, 900 << 20, nil
	}}
	err := busy.check()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"io pressure 72%", "900 MiB dirty"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q should mention %q", err, want)
		}
	}
}

func TestGateAllowsAQuietNode(t *testing.T) {
	quiet := &ioGate{MaxPressure: 60, MaxDirtyBytes: 256 << 20, Read: func() (float64, int64, error) {
		return 2, 4 << 20, nil
	}}
	if err := quiet.check(); err != nil {
		t.Fatalf("quiet node refused: %v", err)
	}
}

// A reading that cannot be taken must not stop backups: the guard exists to
// avoid a stall, not to become a new way to fail.
func TestGateIgnoresAnUnreadablePressureFile(t *testing.T) {
	broken := &ioGate{MaxPressure: 60, Read: func() (float64, int64, error) {
		return 0, 0, context.DeadlineExceeded
	}}
	if err := broken.check(); err != nil {
		t.Fatalf("unreadable pressure should not refuse: %v", err)
	}
}

func TestLVMSnapshotterFreezeRefusesBeforeTouchingTheKernel(t *testing.T) {
	var calls []string
	l := &LVMSnapshotter{
		VG: "vg", LV: "data", Root: "/var/stack/data",
		Gate: &ioGate{MaxPressure: 1, Read: func() (float64, int64, error) { return 99, 0, nil }},
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name)
			return nil, nil
		},
	}
	err := l.Freeze(context.Background(), "/var/stack/data")
	if err == nil {
		t.Fatal("expected the freeze to be refused")
	}
	for _, c := range calls {
		if c == "fsfreeze" {
			t.Fatalf("fsfreeze ran despite the gate: %v", calls)
		}
	}
	if l.Frozen {
		t.Fatal("a refused freeze must not mark the filesystem frozen")
	}
}

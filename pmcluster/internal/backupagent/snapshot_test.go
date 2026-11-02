package backupagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recordingExec captures every command the snapshotter runs. The commands ARE
// the behaviour here, so asserting on them is the only way to test this without
// a real volume group — and a real volume group is exactly what a unit test
// does not have.
type recordingExec struct {
	calls [][]string
	fail  map[string]error
}

func (r *recordingExec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if err, ok := r.fail[name]; ok {
		return []byte("failed"), err
	}
	return nil, nil
}

func (r *recordingExec) find(cmd string) []string {
	for _, c := range r.calls {
		if c[0] == cmd {
			return c
		}
	}
	return nil
}

func (r *recordingExec) joined() string {
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return strings.Join(out, "\n")
}

// The bug this pins: Snapshot() mounted "/dev/<vg>/<lv>" — the ORIGIN — instead
// of the snapshot LV it had just created. On a node where the origin is mounted
// the kernel refuses and the run fails; on one where it is not, the agent would
// archive a live tree and call it a snapshot. Only a real `lvcreate` run caught
// it, so the mount target is asserted here by name.
func TestSnapshotMountsTheSnapshotNotTheOrigin(t *testing.T) {
	ex := &recordingExec{}
	l := &LVMSnapshotter{
		Root: "/var/stack/data", VG: "vg0", LV: "data",
		SnapDir: "/run/pmcluster-backup/snap", Prefix: "pmcluster-snap", Exec: ex.run,
	}
	at := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC)
	dir, token, err := l.Snapshot(context.Background(), l.Root, at)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if token != "pmcluster-snap-20261102T190000" {
		t.Fatalf("token = %q", token)
	}
	if dir != l.SnapDir {
		t.Fatalf("dir = %q", dir)
	}

	// 1. The snapshot LV is created FROM the origin.
	lv := ex.find("lvcreate")
	if lv == nil || !strings.Contains(strings.Join(lv, " "), "vg0/data") {
		t.Fatalf("lvcreate did not name the origin: %v", lv)
	}
	if !strings.Contains(strings.Join(lv, " "), token) {
		t.Fatalf("lvcreate did not name the snapshot: %v", lv)
	}

	// 2. The mount uses the SNAPSHOT device, and is read-only.
	mount := ex.find("mount")
	if mount == nil {
		t.Fatal("no mount was run")
	}
	joined := strings.Join(mount, " ")
	if !strings.Contains(joined, "/dev/vg0/"+token) {
		t.Fatalf("mount did not use the snapshot device: %q", joined)
	}
	if strings.Contains(joined, "/dev/vg0/data") {
		t.Fatalf("mount used the ORIGIN — that would archive a live tree: %q", joined)
	}
	if !strings.Contains(joined, "-o ro") {
		t.Fatalf("mount is not read-only: %q", joined)
	}

	// 3. Drop tears down the snapshot, and only the snapshot.
	if err := l.Drop(context.Background(), token); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	joined = ex.joined()
	if !strings.Contains(joined, "umount") {
		t.Error("Drop did not unmount")
	}
	rm := ex.find("lvremove")
	if rm == nil || !strings.Contains(strings.Join(rm, " "), "vg0/"+token) {
		t.Fatalf("Drop removed the wrong LV: %v", rm)
	}
}

func TestSnapshotFailsCleanlyWhenLvcreateFails(t *testing.T) {
	ex := &recordingExec{fail: map[string]error{"lvcreate": errFake}}
	l := &LVMSnapshotter{Root: "/d", VG: "vg0", LV: "data", Exec: ex.run}
	if _, _, err := l.Snapshot(context.Background(), "/d", time.Now().UTC()); err == nil {
		t.Fatal("want an error when lvcreate fails")
	}
	if mount := ex.find("mount"); mount != nil {
		t.Fatalf("mounted despite a failed lvcreate: %v", mount)
	}
}

// A failed mount must remove the snapshot it created, or every failed run
// leaks a thin LV until the pool fills — which then aborts later runs.
func TestSnapshotCleansUpWhenMountFails(t *testing.T) {
	ex := &recordingExec{fail: map[string]error{"mount": errFake}}
	l := &LVMSnapshotter{Root: "/d", VG: "vg0", LV: "data", Exec: ex.run}
	if _, _, err := l.Snapshot(context.Background(), "/d", time.Now().UTC()); err == nil {
		t.Fatal("want an error when mount fails")
	}
	if rm := ex.find("lvremove"); rm == nil {
		t.Fatalf("leaked the snapshot LV after a failed mount:\n%s", ex.joined())
	}
}

// Thaw must never unfreeze a filesystem this process did not freeze: a second
// agent, or a run that failed before freezing, must not release someone else's
// lock.
func TestThawIsANoOpWhenNothingWasFrozen(t *testing.T) {
	ex := &recordingExec{}
	l := &LVMSnapshotter{Root: "/d", Exec: ex.run}
	if err := l.Thaw(context.Background(), "/d"); err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if len(ex.calls) != 0 {
		t.Fatalf("Thaw ran commands without a freeze: %v", ex.calls)
	}
	if err := l.Freeze(context.Background(), "/d"); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if err := l.Thaw(context.Background(), "/d"); err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	joined := ex.joined()
	if !strings.Contains(joined, "fsfreeze -f /d") || !strings.Contains(joined, "fsfreeze -u /d") {
		t.Fatalf("freeze/thaw not paired: %s", joined)
	}
}

// The guard must read the POOL, not a data volume. Measured live: a healthy
// pool at 1.72% next to its data LV at 4.31% — and a data LV can legitimately
// sit at 85%, which the first "worst line" version would have aborted on.
func TestUsageReadsOnlyTheThinPool(t *testing.T) {
	l := &LVMSnapshotter{VG: "vg0", Exec: func(context.Context, string, ...string) ([]byte, error) {
		// lv_attr, data_percent: V = thin volume, t = thin pool.
		return []byte("  Vwi-a-tz-- 85.00\n  twi-aotz--  1.72\n"), nil
	}}
	pct, err := l.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if pct != 1.72 {
		t.Fatalf("pct = %v, want the pool's 1.72 (not the data volume's 85)", pct)
	}
}

func TestUsageWithoutAThinPoolIsUnknown(t *testing.T) {
	l := &LVMSnapshotter{VG: "vg0", Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("  -wi-ao---- 12.00\n"), nil
	}}
	pct, err := l.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if pct != -1 {
		t.Fatalf("pct = %v, want -1 (no thin pool, no guard)", pct)
	}
}

// Verify is the guard that makes a misconfigured node fail closed. On a fresh
// install the volume root is a plain directory on the root filesystem, so
// there is nothing atomic to snapshot; the agent must refuse rather than tar a
// live tree.
func TestVerifyRejectsAVolumeRootThatIsNotTheConfiguredLV(t *testing.T) {
	l := &LVMSnapshotter{VG: "vg0", LV: "data"}
	l.Exec = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "findmnt":
			return []byte("/dev/vda1\n"), nil // plain directory on the root fs
		case "readlink":
			return []byte(args[len(args)-1] + "\n"), nil
		}
		return nil, nil
	}
	err := l.Verify(context.Background(), "/var/stack/data")
	if err == nil {
		t.Fatal("Verify accepted a volume root mounted from the root filesystem")
	}
	if !strings.Contains(err.Error(), "not from /dev/vg0/data") {
		t.Fatalf("error does not explain the mismatch: %v", err)
	}
}

func TestVerifyAcceptsTheConfiguredLV(t *testing.T) {
	for _, source := range []string{"/dev/mapper/vg0-data", "/dev/vg0/data"} {
		l := &LVMSnapshotter{VG: "vg0", LV: "data"}
		l.Exec = func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "findmnt":
				return []byte(source + "\n"), nil
			case "readlink":
				return []byte("/dev/dm-7\n"), nil
			}
			return nil, nil
		}
		if err := l.Verify(context.Background(), "/var/stack/data"); err != nil {
			t.Fatalf("%s: Verify: %v", source, err)
		}
	}
}

func TestVerifyRequiresAVGAndLV(t *testing.T) {
	l := &LVMSnapshotter{Exec: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}
	if err := l.Verify(context.Background(), "/var/stack/data"); err == nil {
		t.Fatal("Verify accepted an unconfigured snapshotter")
	}
}

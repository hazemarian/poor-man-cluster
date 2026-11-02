package backupagent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Execer runs one command. Production is ExecCommand; tests substitute a fake
// so the whole snapshot lifecycle is exercised without LVM.
type Execer func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecCommand is the real Execer.
func ExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// LVMSnapshotter freezes the filesystem and takes a thin snapshot of an LV.
//
// Why the freeze: an LVM snapshot of a mounted ext4 filesystem is
// crash-consistent (equivalent to pulling the plug), and every engine in the
// wild recovers from that. Freezing first upgrades it to a clean image — the
// difference between "the database has to replay its log to start" and "the
// database starts". The freeze is the only write stall anywhere in the design,
// and it lasts as long as `lvcreate` takes: milliseconds.
//
// Why not stop the container: stopping is the thing this whole design removes.
type LVMSnapshotter struct {
	// Root is the mount point being frozen (/var/stack/data).
	Root string
	// VG and LV name the logical volume holding Root.
	VG, LV string
	// SnapDir is where the snapshot is mounted read-only.
	SnapDir string
	// Prefix is prepended to the generated snapshot LV name.
	Prefix string
	// Exec runs commands; nil means ExecCommand.
	Exec Execer
	// Frozen records whether Freeze succeeded, so Thaw is only ever called on
	// a filesystem this process actually froze.
	Frozen bool
}

func (l *LVMSnapshotter) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	e := l.Exec
	if e == nil {
		e = ExecCommand
	}
	return e(ctx, name, args...)
}

// Kind implements Snapshotter.
func (l *LVMSnapshotter) Kind() string { return "lvm" }

// Freeze implements Snapshotter with `fsfreeze -f`.
func (l *LVMSnapshotter) Freeze(ctx context.Context, root string) error {
	if root == "" {
		root = l.Root
	}
	if out, err := l.exec(ctx, "fsfreeze", "-f", root); err != nil {
		return fmt.Errorf("fsfreeze -f %s: %w (%s)", root, err, strings.TrimSpace(string(out)))
	}
	l.Frozen = true
	return nil
}

// Thaw implements Snapshotter with `fsfreeze -u`. It is a no-op when this
// process never froze anything, so a failed Freeze cannot unfreeze somebody
// else's lock.
func (l *LVMSnapshotter) Thaw(ctx context.Context, root string) error {
	if !l.Frozen {
		return nil
	}
	if root == "" {
		root = l.Root
	}
	out, err := l.exec(ctx, "fsfreeze", "-u", root)
	l.Frozen = false
	if err != nil {
		return fmt.Errorf("fsfreeze -u %s: %w (%s)", root, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Snapshot creates the thin snapshot and mounts it read-only.
func (l *LVMSnapshotter) Snapshot(ctx context.Context, root string, at time.Time) (string, string, error) {
	if l.VG == "" || l.LV == "" {
		return "", "", fmt.Errorf("snapshot: VG and LV are required (set backup_vg / backup_lv)")
	}
	prefix := l.Prefix
	if prefix == "" {
		prefix = "pmcluster-snap"
	}
	name := prefix + "-" + at.UTC().Format("20060102T150405")
	origin := l.VG + "/" + l.LV
	// -kn keeps the snapshot's minor number from being reused, -p r makes it
	// read-only: a writable snapshot would let nothing write to it anyway, but
	// the kernel's refusal is a better guard than ours.
	if out, err := l.exec(ctx, "lvcreate", "-s", "-n", name, "-kn", "-p", "r", origin); err != nil {
		return "", "", fmt.Errorf("lvcreate -s %s: %w (%s)", origin, err, strings.TrimSpace(string(out)))
	}
	dir := l.SnapDir
	if dir == "" {
		dir = "/run/pmcluster-backup/snap"
	}
	if out, err := l.exec(ctx, "mkdir", "-p", dir); err != nil {
		_ = l.dropLV(ctx, name)
		return "", "", fmt.Errorf("mkdir %s: %w (%s)", dir, err, strings.TrimSpace(string(out)))
	}
	// Mount the SNAPSHOT, never the origin: "/dev/<vg>/<origin-lv>" is the live
	// volume, and archiving that would tar a tree the applications are still
	// writing to — the exact failure this design exists to prevent. The kernel
	// only caught it here because the origin was mounted elsewhere.
	if out, err := l.exec(ctx, "mount", "-o", "ro", "/dev/"+l.VG+"/"+name, dir); err != nil {
		_ = l.dropLV(ctx, name)
		return "", "", fmt.Errorf("mount snapshot %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return dir, name, nil
}

// Usage reports the THIN POOL's data percentage, or -1 when the VG holds none.
//
// Only the pool's figure is a snapshot risk: when it fills, the kernel
// invalidates every snapshot in it. A thin *volume*'s data_percent measures how
// much of that volume's own size is used, so reading every line and taking the
// worst (the first version did) aborts runs on a perfectly healthy pool just
// because one data LV is 85% full.
func (l *LVMSnapshotter) Usage(ctx context.Context) (float64, error) {
	if l.VG == "" {
		return -1, nil
	}
	out, err := l.exec(ctx, "lvs", "--noheadings", "-o", "lv_attr,data_percent", l.VG)
	if err != nil {
		return -1, err
	}
	worst := -1.0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// lv_attr starts with the LV type: 't' is a thin pool.
		if !strings.HasPrefix(fields[0], "t") {
			continue
		}
		f, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		if f > worst {
			worst = f
		}
	}
	return worst, nil
}

// Drop unmounts and removes the snapshot. Errors are returned so the caller can
// log them; a leftover snapshot is a disk-space problem, not a data problem.
func (l *LVMSnapshotter) Drop(ctx context.Context, token string) error {
	dir := l.SnapDir
	if dir == "" {
		dir = "/run/pmcluster-backup/snap"
	}
	var firstErr error
	if out, err := l.exec(ctx, "umount", dir); err != nil {
		// Not mounted (or already gone) is the common benign case.
		if !strings.Contains(string(out), "not mounted") {
			firstErr = fmt.Errorf("umount %s: %w (%s)", dir, err, strings.TrimSpace(string(out)))
		}
	}
	if err := l.dropLV(ctx, token); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (l *LVMSnapshotter) dropLV(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	out, err := l.exec(ctx, "lvremove", "-y", l.VG+"/"+token)
	if err != nil {
		return fmt.Errorf("lvremove %s: %w (%s)", token, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NoSnapshot is the test / dry-run snapshotter: it hands back the live tree
// unchanged. It is never selected by a production run — a tar of a live tree
// is the exact failure the agent exists to prevent — but it lets the whole
// pipeline run under `go test` and on a host without LVM.
type NoSnapshot struct{ Root string }

// Kind implements Snapshotter.
func (n *NoSnapshot) Kind() string { return "none" }

// Freeze implements Snapshotter.
func (n *NoSnapshot) Freeze(context.Context, string) error { return nil }

// Thaw implements Snapshotter.
func (n *NoSnapshot) Thaw(context.Context, string) error { return nil }

// Snapshot implements Snapshotter.
func (n *NoSnapshot) Snapshot(_ context.Context, root string, _ time.Time) (string, string, error) {
	if root == "" {
		root = n.Root
	}
	if root == "" {
		return "", "", fmt.Errorf("no volume root")
	}
	return root, "", nil
}

// Usage implements Snapshotter.
func (n *NoSnapshot) Usage(context.Context) (float64, error) { return -1, nil }

// Drop implements Snapshotter.
func (n *NoSnapshot) Drop(context.Context, string) error { return nil }

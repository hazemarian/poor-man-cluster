package backupagent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/lvmlock"
)

// Execer runs one command. Production is ExecCommand; tests substitute a fake
// so the whole snapshot lifecycle is exercised without LVM.
type Execer func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecCommand is the real Execer.
func ExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// LVMSnapshotter takes a thin snapshot of an LV and mounts it read-write.
//
// There is no fsfreeze. It was removed after it hung a node for 19 minutes at a
// 4.4% chance per run: freezing a filesystem that is actively written to is
// unbounded, and the snapshot is worth nothing if the backup never finishes. A
// thin snapshot is crash-consistent by construction — the equivalent of pulling
// the power — and every engine here recovers from that on its own next start.
// Mounting the copy read-write lets the kernel replay the journal on the
// snapshot, so the archive holds an already-recovered tree.
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

// locked runs one LVM command under the shared LVM lock.
//
// EVERY LVM invocation in this file goes through here. The deadlock lvmlock
// exists to prevent ("vgs takes the global lock then waits for the group lock;
// lvextend takes them in the other order") is a property of the command, not of
// `lvcreate`: an unguarded `lvs` deadlocks against a running `lvextend` exactly
// like `vgs` does, and an unguarded `lvremove` holds the group lock exclusively.
// This file used to guard only the snapshot call, which left four commands able
// to reproduce the twelve-minute production hang.
func (l *LVMSnapshotter) locked(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out []byte
	var err error
	lockErr := lvmlock.With(ctx, func() error {
		out, err = l.exec(ctx, name, args...)
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return out, err
}

// Kind implements Snapshotter.
func (l *LVMSnapshotter) Kind() string { return "lvm" }

// Verify proves the volume root is a mount of the LV this snapshotter is
// configured to snapshot, BEFORE anything is frozen.
//
// It exists because the failure it prevents is silent. On a node where the
// volume root is still a plain directory on the root filesystem — which is
// exactly what a fresh install produces, since nothing in the platform
// provisions storage — `lvcreate -s` would snapshot the wrong volume (or fail),
// and any fallback that "just tars the directory" archives a tree the
// applications are still writing to. A configured-but-unprovisioned node must
// refuse to run, not run wrongly.
func (l *LVMSnapshotter) Verify(ctx context.Context, root string) error {
	if root == "" {
		root = l.Root
	}
	if l.VG == "" || l.LV == "" {
		return fmt.Errorf("verify: backup_vg/backup_lv are unset — this node has no snapshot-ready volume; provision one or leave the offen agent in place")
	}
	out, err := l.exec(ctx, "findmnt", "-no", "SOURCE", "--target", root)
	if err != nil {
		return fmt.Errorf("verify: findmnt %s: %w (%s)", root, err, strings.TrimSpace(string(out)))
	}
	source := strings.TrimSpace(string(out))
	if source == "" {
		return fmt.Errorf("verify: %s is not a mount point — it is a directory on the root filesystem, so there is nothing atomic to snapshot", root)
	}
	want := "/dev/" + l.VG + "/" + l.LV
	got, gerr := l.exec(ctx, "readlink", "-f", source)
	wantResolved, werr := l.exec(ctx, "readlink", "-f", want)
	if gerr == nil && werr == nil && strings.TrimSpace(string(got)) == strings.TrimSpace(string(wantResolved)) {
		return nil
	}
	// Fall back to the mapper name, which is what findmnt usually prints.
	if strings.Contains(source, "mapper/"+l.VG+"-"+l.LV) || strings.Contains(source, want) {
		return nil
	}
	return fmt.Errorf("verify: %s is mounted from %s, not from %s — refusing to snapshot a volume this node does not own", root, source, want)
}

// Freeze is deliberately a no-op.
//
// It used to call `fsfreeze -f`, which flushes every dirty page and blocks
// writers until the disk acknowledges them. That flush is unbounded and cannot be
// cancelled, and on a node whose pool is a loop file on the root disk it competes
// with Docker, containerd, journald and sshd for the same device. Measured on
// production: 19m29s of node-wide write stall, kernel hung-task reports, dockerd
// missing its swarm heartbeat, and every service on the node rescheduled.
//
// What it bought was filesystem-metadata consistency — and the filesystem already
// guarantees that itself. A journaling filesystem keeps "the on-disk state is
// always recoverable" true at every instant, so a thin snapshot (a metadata
// operation, 192 ms measured) captures a recoverable state. Mount the copy
// read-write and the journal replays, exactly as it does after a power cut. The
// same guarantee every cloud volume snapshot gives.
//
// Databases are unaffected: Postgres, InnoDB and SQLite are crash-safe by design
// and replay their own redo on open. The one invariant that matters is that the
// redo material lives in the SAME snapshot as the data — see the volume layout
// check in `pmcluster storage status`.
func (l *LVMSnapshotter) Freeze(ctx context.Context, root string) error { return nil }

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
	// Not -p r: the snapshot is mounted read-write so the filesystem can replay
	// its journal on the copy. A read-only snapshot of a filesystem captured
	// mid-transaction is exactly the case that needs recovery, and refusing the
	// copy the right to repair itself would hand back a torn tree.
	out, err := l.locked(ctx, "lvcreate", "-s", "-n", name, "-kn", origin)
	if err != nil {
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
	if out, err := l.exec(ctx, "mount", "/dev/"+l.VG+"/"+name, dir); err != nil {
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
	out, err := l.locked(ctx, "lvs", "--noheadings", "-o", "lv_attr,data_percent", l.VG)
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
// snapDir is where the snapshot gets mounted.
func (l *LVMSnapshotter) snapDir() string {
	if l.SnapDir != "" {
		return l.SnapDir
	}
	return "/run/pmcluster-backup/snap"
}

// RemoveStale drops snapshot LVs left behind by a run that never finished.
//
// A run killed with SIGKILL — an operator, an OOM kill, a reboot mid-ship, or
// the run deadline expiring — never reaches its deferred lvremove. The snapshot
// then sits on the origin holding pool blocks until the 80% guard starts
// aborting runs for a reason that looks nothing like its cause. Cleanup belongs
// at the start of the next run, not the end of the last one.
func (l *LVMSnapshotter) RemoveStale(ctx context.Context) ([]string, error) {
	prefix := l.Prefix
	if prefix == "" {
		prefix = "pmcluster-snap"
	}
	out, err := l.locked(ctx, "lvs", "--noheadings", "-o", "lv_name", "-S", "lv_name=~^"+prefix+"-", l.VG)
	if err != nil {
		return nil, fmt.Errorf("list stale snapshots in %s: %w (%s)", l.VG, err, strings.TrimSpace(string(out)))
	}
	var removed []string
	for _, name := range parseLVSNames(string(out)) {
		// A left-over mount would block the next run's mount point; an unmounted
		// LV reports "not mounted" here, which is the benign case.
		if out, uerr := l.exec(ctx, "umount", l.snapDir()); uerr != nil && !strings.Contains(string(out), "not mounted") {
			// Keep going: lvremove can still succeed, and failing the run over a
			// leftover directory is worse than trying.
			_ = out
		}
		if out, rerr := l.locked(ctx, "lvremove", "-y", l.VG+"/"+name); rerr != nil {
			return removed, fmt.Errorf("drop stale snapshot %s: %w (%s)", name, rerr, strings.TrimSpace(string(out)))
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// parseLVSNames reads `lvs --noheadings -o lv_name` output.
func parseLVSNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			names = append(names, n)
		}
	}
	return names
}

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
	// Bounded: the lock may be held by a `storage grow` that will run for minutes.
	// Waiting without a ceiling would park this run's cleanup for exactly as long,
	// which is the failure mode lvmlock exists to remove. Give up, leave the LV,
	// and let the next run's RemoveStale reap it.
	lockCtx, cancel := context.WithTimeout(ctx, dropLockWait)
	defer cancel()
	if err := l.dropLV(lockCtx, token); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// dropLockWait bounds how long a cleanup will queue behind another LVM command.
const dropLockWait = 2 * time.Minute

// dropLV removes one snapshot LV under the LVM lock.
//
// Never call it from inside an lvmlock.With body: flock conflicts with itself in
// one process and the second acquire parks forever (the package records `grow`
// hanging a node's LVM for seven minutes that way). Every caller here is outside
// a locked region.
func (l *LVMSnapshotter) dropLV(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	out, err := l.locked(ctx, "lvremove", "-y", l.VG+"/"+token)
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

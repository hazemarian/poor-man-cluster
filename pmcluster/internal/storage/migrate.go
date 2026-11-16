package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Migrate adopts a node that already holds stack data into a snapshot-capable
// volume.
//
// Provisioning refuses a non-empty volume root, and that refusal is right: a
// mount over a live directory hides the data and looks like loss. But an
// existing node is exactly the case that matters — every production node has
// stacks on a plain directory today, and "provision it when you rebuild the
// node" means never.
//
// The sequence is the one a database host has always used for this: build the
// new filesystem somewhere else, copy the data into it while nothing is writing,
// switch the mount, and keep the old tree until somebody deletes it on purpose.
//
// Nothing here is clever, and the parts that could lose data are ordered so a
// failure at any step leaves the live tree untouched:
//
//	build (temp mount)  →  copy  →  verify counts  →  umount  →  swap  →  mount
//
// A failure before "swap" leaves /var/stack/data exactly as it was. After swap
// the old tree is still on disk at <root>.pre-migration-<ts>.
func Migrate(ctx context.Context, spec Spec) (Result, error) {
	spec = spec.withDefaults()
	var res Result
	res.Mode = spec.Mode

	st, err := detect(ctx, spec)
	if err != nil {
		return res, err
	}
	if st.Provisioned() {
		res.AlreadyProvisioned = true
		res.Device = spec.device()
		return res, nil
	}
	if st.Mounted {
		return res, fmt.Errorf("%s is already a mount (from %s) — nothing to migrate", spec.VolumeRoot, st.Source)
	}
	if st.Entries == 0 {
		// Nothing to preserve: this is a plain provision, which already handles
		// building, mounting and verifying.
		return Provision(ctx, spec)
	}
	if spec.Mode != ModeLoop {
		return res, fmt.Errorf("migrate currently supports --mode loop; an existing block device needs a separate plan")
	}
	if err := writersOnVolume(ctx, spec); err != nil && !spec.Force {
		return res, fmt.Errorf("%w — drain the node first (`docker node update --availability drain <node>`), then re-run; --force skips this check", err)
	}

	ts := time.Now().UTC().Format("20060102T150405")
	staging := spec.VolumeRoot + ".staging-" + ts

	// 1. build the volume and mount it somewhere else entirely.
	if err := buildLoop(ctx, spec, st, &res); err != nil {
		return res, err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return res, fmt.Errorf("create staging mount point: %w", err)
	}
	if out, err := spec.exec(ctx, "mount", spec.device(), staging); err != nil {
		return res, fmt.Errorf("mount %s at %s: %w (%s)", spec.device(), staging, err, strings.TrimSpace(string(out)))
	}
	// From here on, every failure must unmount before returning.
	unmount := func() {
		_, _ = spec.exec(ctx, "umount", staging)
	}
	defer unmount()

	// 2. copy the live tree into it.
	res.Copied, res.CopyBytes, err = copyTree(spec.VolumeRoot, staging)
	if err != nil {
		return res, fmt.Errorf("copy %s → %s: %w (the live tree is untouched)", spec.VolumeRoot, staging, err)
	}

	// 3. prove the copy is complete before anything is switched.
	srcFiles, srcBytes, err := countTree(spec.VolumeRoot)
	if err != nil {
		return res, err
	}
	if srcFiles != res.Copied || srcBytes != res.CopyBytes {
		return res, fmt.Errorf("copy verification failed: source %d files / %d bytes, copy %d files / %d bytes — the live tree is untouched, nothing was switched",
			srcFiles, srcBytes, res.Copied, res.CopyBytes)
	}

	// 4. unmount the staging copy; the units will mount it at the real path.
	unmount()

	// 5. move the old tree aside and re-create the mount point.
	old := spec.VolumeRoot + ".pre-migration-" + ts
	if err := os.Rename(spec.VolumeRoot, old); err != nil {
		return res, fmt.Errorf("move %s aside: %w (nothing is mounted; the data is still there)", spec.VolumeRoot, err)
	}
	if err := os.MkdirAll(spec.VolumeRoot, 0o755); err != nil {
		return res, fmt.Errorf("re-create %s: %w (old data is at %s)", spec.VolumeRoot, err, old)
	}
	res.PreviousTree = old

	// 6. install the units and mount. writeUnits verifies the result.
	if err := writeUnits(ctx, spec, &res); err != nil {
		return res, fmt.Errorf("%w — old data is preserved at %s", err, old)
	}
	_ = os.Remove(staging)
	res.Device = spec.device()
	return res, nil
}

// writersOnVolume refuses the copy when a container is still writing into the
// volume root. Copying a live database into a new filesystem produces exactly
// the torn image this whole design exists to avoid.
func writersOnVolume(ctx context.Context, spec Spec) error {
	out, err := spec.exec(ctx, "docker", "ps", "-q")
	if err != nil {
		return nil // no docker: nothing to check, and the operator's call
	}
	ids := strings.Fields(string(out))
	for _, id := range ids {
		mounts, err := spec.exec(ctx, "docker", "inspect", "--format", "{{json .Mounts}}", id)
		if err != nil {
			continue
		}
		if strings.Contains(string(mounts), spec.VolumeRoot) {
			return fmt.Errorf("container %.12s is still writing into %s", id, spec.VolumeRoot)
		}
	}
	return nil
}

// copyTree copies dir → dst, preserving modes, mtimes, ownership and symlinks.
// Sockets and fifos are skipped: they cannot be copied and carry no data.
//
// Ownership is not a nicety here. This is the copy that moves stack data onto the
// snapshot-capable volume when a node is adopted, and it runs as root. Dropping
// uid/gid turns every app-data directory root-owned, so an app whose image runs
// as a non-root user can no longer write its own database — it exits on startup,
// swarm restarts it every few seconds, and each restart tears down and rebuilds
// the task's network sandbox. That is a saturation incident disguised as a
// permission error, and it outlives the adoption by however long nobody looks.
func copyTree(src, dst string) (files int, bytes int64, err error) {
	err = filepath.Walk(src, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		uid, gid := ownerOf(info)
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return ownPath(target, uid, gid, info.ModTime())
		case info.Mode()&os.ModeSymlink != 0:
			link, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			return ownPath(target, uid, gid, info.ModTime())
		case !info.Mode().IsRegular():
			return nil // sockets, devices, fifos
		}
		if err := copyFile(p, target, info.Mode().Perm()); err != nil {
			return err
		}
		if err := ownPath(target, uid, gid, info.ModTime()); err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}

// ownerOf reads uid/gid off a FileInfo where the platform exposes them.
func ownerOf(info os.FileInfo) (uid, gid int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// ownPath restores ownership and mtime. A -1 uid/gid means "unknown on this
// platform", in which case only the mtime is set: failing the whole migration
// because a stat had no ownership would be worse than carrying the copy as root.
func ownPath(path string, uid, gid int, mod time.Time) error {
	if uid >= 0 && gid >= 0 {
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
	}
	return os.Chtimes(path, mod, mod)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// countTree reports the file count and byte total, matching copyTree's rules so
// the two numbers are comparable.
func countTree(root string) (files int, bytes int64, err error) {
	err = filepath.Walk(root, func(_ string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes, err
}

// buildLoop creates the backing file, PV, VG, thin pool, thin volume and
// filesystem without mounting anything.
func buildLoop(ctx context.Context, spec Spec, st State, res *Result) error {
	// A volume group with our name that is not mounted over our root is stale
	// state — a torn-down volume, or a previous run interrupted between vgcreate
	// and mount. `vgcreate` reports that as "already exists in filesystem", which
	// reads like a filesystem problem and sends people looking in the wrong place.
	if st.VGExists {
		return fmt.Errorf("volume group %s already exists but %s is not mounted from it — run `pmcluster storage provision` to activate and mount it, or remove the stale group first (`vgchange -an %s && vgremove %s`)", spec.VG, spec.VolumeRoot, spec.VG, spec.VG)
	}
	if err := os.MkdirAll(filepath.Dir(spec.ImagePath), 0o755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	size := spec.Size
	if size == 0 {
		free, err := freeBytes(ctx, spec, filepath.Dir(spec.ImagePath))
		if err != nil {
			return err
		}
		total, terr := totalBytes(ctx, spec, filepath.Dir(spec.ImagePath))
		if terr != nil {
			// Fall back to free space rather than refusing to provision: a df
			// variant that does not know --output=size must not block an install.
			total = free * 2
		}
		size = AutoSize(total, free)
		// Say it out loud: on a small node the minimum still "works", but a pool
		// this tight leaves a snapshot almost no room, and that is the operator's
		// decision to make rather than a surprise at the first busy hour.
		if size == minAutoSize && int64(float64(total)*autoDiskShare) < minAutoSize {
			res.Warnings = append(res.Warnings, fmt.Sprintf("only %s free: using the %s minimum, which leaves little room for snapshots", humanBytes(free), humanBytes(minAutoSize)))
		}
	}
	res.ImagePath, res.ImageBytes = spec.ImagePath, size
	res.PoolBytes = PoolSize(size)
	res.DataBytes = DataSize(res.PoolBytes)

	if !st.ImageExists {
		if out, err := spec.exec(ctx, "truncate", "-s", fmt.Sprintf("%d", size), spec.ImagePath); err != nil {
			return fmt.Errorf("create backing file: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	loop := st.LoopDevice
	if loop == "" {
		out, err := spec.exec(ctx, "losetup", "--find", "--show", spec.ImagePath)
		if err != nil {
			return fmt.Errorf("attach loop device: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		loop = strings.TrimSpace(string(out))
	}
	// A torn-down volume can leave orphaned device-mapper mappings and a
	// /dev/<vg> directory behind even though `vgs` reports no such group —
	// exactly what a failed lvremove/vgremove leaves. vgcreate then refuses with
	// "already exists in filesystem", which reads like a filesystem error.
	//
	// Safe to clear only because the guard above already established that no
	// volume group by this name exists: these mappings belong to nothing.
	if cleared := clearStaleMappings(ctx, spec); len(cleared) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("cleared %d orphaned device-mapper mapping(s) from a previous teardown: %s", len(cleared), strings.Join(cleared, ", ")))
	}
	// Resume rather than restart. A build can stop between any two steps (a crash,
	// a full disk, an operator ^C), and the next attempt then meets a device that
	// already carries a physical volume — `pvcreate` refuses without -ff, which
	// looks like corruption and is really just an unfinished job.
	hasPV := pvBelongsTo(ctx, spec, loop, spec.VG)
	if !hasPV {
		if out, err := spec.exec(ctx, "pvcreate", "-q", loop); err != nil {
			return fmt.Errorf("pvcreate: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		if out, err := spec.exec(ctx, "vgcreate", "-q", spec.VG, loop); err != nil {
			return fmt.Errorf("vgcreate: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	} else {
		res.Warnings = append(res.Warnings, "resumed a partially built volume: "+loop+" already belongs to "+spec.VG)
	}
	if out, err := spec.exec(ctx, "vgchange", "-ay", spec.VG); err != nil {
		return fmt.Errorf("activate %s: %w (%s)", spec.VG, err, strings.TrimSpace(string(out)))
	}
	if !lvExists(ctx, spec, "pool") {
		if out, err := spec.exec(ctx, "lvcreate", "-q", "-T", spec.VG+"/pool", "-L", fmt.Sprintf("%dB", res.PoolBytes),
			"--poolmetadatasize", fmt.Sprintf("%dB", poolMetaSize(res.PoolBytes))); err != nil {
			return fmt.Errorf("lvcreate pool: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if !lvExists(ctx, spec, spec.LV) {
		if out, err := spec.exec(ctx, "lvcreate", "-q", "-V", fmt.Sprintf("%dB", res.DataBytes), "-T", spec.VG+"/pool", "-n", spec.LV); err != nil {
			return fmt.Errorf("lvcreate %s: %w (%s)", spec.LV, err, strings.TrimSpace(string(out)))
		}
	}
	if !hasFilesystem(ctx, spec) {
		if out, err := spec.exec(ctx, "mke2fs", "-q", "-t", "ext4", "-L", "pmcluster-data", spec.device()); err != nil {
			return fmt.Errorf("mke2fs: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	res.Device = spec.device()
	return nil
}

// clearStaleMappings removes device-mapper mappings and /dev nodes that name our
// volume group while no such group exists.
func clearStaleMappings(ctx context.Context, spec Spec) []string {
	escaped := strings.ReplaceAll(spec.VG, "-", "--")
	var cleared []string
	// Several passes: a thin pool cannot be removed before the volumes inside it,
	// so the first pass always leaves the parent behind with "Device or resource
	// busy". Removing children first and retrying converges.
	for pass := 0; pass < 5; pass++ {
		out, err := spec.exec(ctx, "dmsetup", "ls", "--noheadings", "-o", "name")
		if err != nil {
			break
		}
		remaining := 0
		for _, name := range strings.Fields(string(out)) {
			if !strings.HasPrefix(name, escaped+"-") {
				continue
			}
			remaining++
			if _, rerr := spec.exec(ctx, "dmsetup", "remove", "--force", name); rerr == nil {
				cleared = append(cleared, name)
			}
		}
		if remaining == 0 {
			break
		}
	}
	// Then the /dev nodes the mappings owned.
	dir := "/dev/" + spec.VG
	if entries, derr := os.ReadDir(dir); derr == nil {
		for _, e := range entries {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				cleared = append(cleared, "/dev/"+spec.VG+"/"+e.Name())
			}
		}
	}
	_ = os.Remove(dir)
	return cleared
}

// pvBelongsTo reports whether dev already carries a physical volume in vg.
func pvBelongsTo(ctx context.Context, spec Spec, dev, vg string) bool {
	out, err := spec.exec(ctx, "pvs", "--noheadings", "-o", "pv_name,vg_name", dev)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimSpace(f[0]) == dev && f[1] == vg {
			return true
		}
	}
	return false
}

// lvExists reports whether the named logical volume is already there.
func lvExists(ctx context.Context, spec Spec, name string) bool {
	out, err := spec.exec(ctx, "lvs", "--noheadings", "-o", "lv_name", spec.VG)
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(out)) {
		if f == name {
			return true
		}
	}
	return false
}

// hasFilesystem reports whether the data volume already carries an ext4
// signature, so a resumed build does not reformat a filesystem that may hold
// data.
func hasFilesystem(ctx context.Context, spec Spec) bool {
	out, err := spec.exec(ctx, "blkid", "-o", "value", "-s", "TYPE", spec.device())
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "ext4")
}

package storage

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/lvmlock"
)

// Repairing a thin pool exists because of one specific failure: thin metadata
// that disagrees with itself or with the kernel.
//
// The one seen in production was an orphaned thin device id — a snapshot
// creation interrupted mid-flight leaves the id allocated in the metadata with
// no logical volume behind it, and from then on every `lvcreate -s` on that node
// fails with "already contain thin device with device_id N". The backup agent
// has no way to recover from that, so the node stops backing anything up while
// still looking healthy: the volume is mounted, the group is present, `storage
// status` used to say "snapshots possible".
//
// There are exactly three exits, in increasing order of damage:
//
//  1. reload the pool target — free, and it fixes state that only lives in the
//     kernel. It did not fix the production case.
//  2. lvconvert --repair — rebuilds the metadata, needs the pool inactive and
//     needs free extents in the volume group for the replacement metadata.
//  3. rebuild the volume — destroy and re-create. Data is gone.
//
// This command walks 2 and 3, and refuses with the reason when neither is
// possible, because that reason (no free extents) is a provisioning mistake that
// is invisible from the error LVM prints.

// RepairOptions is what the operator decided.
type RepairOptions struct {
	// Unmount lets the repair unmount the volume's mount points itself. Without
	// it a mounted volume is refused: stopping every writer that fastens itself
	// to this filesystem is the operator's decision, not a side effect of asking
	// for a metadata repair.
	Unmount bool
	// Rebuild destroys and re-creates pool + data. Data is lost. Requires
	// AllowDataLoss, so it can never happen because a flag was copied.
	Rebuild bool
	// AllowDataLoss is the acknowledgement that Rebuild means data loss.
	AllowDataLoss bool
	// KeepBackup keeps LVM's copy of the unrepaired metadata (auto-named
	// <pool>_metaN) after a verified repair, for inspection.
	KeepBackup bool
}

// RepairResult reports what happened, in the terms a log line needs.
type RepairResult struct {
	// Mode is "repair" (metadata rebuilt in place) or "rebuild" (volume
	// re-created, data lost).
	Mode          string `json:"mode"`
	MetadataBytes int64  `json:"metadata_bytes"`
	PoolBytes     int64  `json:"pool_bytes,omitempty"`
	DataBytes     int64  `json:"data_bytes,omitempty"`
	// BackupLV is the volume LVM kept the unrepaired metadata in, when it did.
	BackupLV string `json:"backup_lv,omitempty"`
	// Unmounted lists the mount points the repair had to release.
	Unmounted []string `json:"unmounted,omitempty"`
	// Verified means a real snapshot was taken and removed after the repair.
	// Anything less would be reporting the layout again, which is what failed.
	Verified bool `json:"verified"`
	// RepairOutput is LVM's own account of the repair, kept for the human and
	// for the record: the interesting part of a metadata repair is what it
	// dropped.
	RepairOutput string `json:"repair_output,omitempty"`
}

// Repair repairs this volume's thin pool metadata, and proves it by taking a
// snapshot afterwards.
func Repair(ctx context.Context, spec Spec, opts RepairOptions) (RepairResult, error) {
	spec = spec.withDefaults()
	res := RepairResult{}
	if opts.Rebuild && !opts.AllowDataLoss {
		return res, fmt.Errorf("--rebuild destroys %s and its data; repeat with --allow-data-loss if that is what you want", spec.VolumeRoot)
	}

	st, err := detect(ctx, spec)
	if err != nil {
		return res, err
	}
	if !st.VGExists {
		return res, fmt.Errorf("volume group %s is not present on this node — nothing to repair (provision the volume first)", spec.VG)
	}

	if opts.Rebuild {
		return rebuild(ctx, spec, opts, res)
	}
	return repairInPlace(ctx, spec, opts, res)
}

// repairInPlace is lvconvert --repair, with the preflight LVM does not do.
func repairInPlace(ctx context.Context, spec Spec, opts RepairOptions, res RepairResult) (RepairResult, error) {
	res.Mode = "repair"
	meta, err := metadataBytes(ctx, spec)
	if err != nil {
		return res, err
	}
	res.MetadataBytes = meta

	// The preflight that turns a confusing LVM error into a sentence. lvconvert
	// needs one metadata copy of free extents (it writes the repaired metadata as
	// a new LV and keeps the old one beside it); a pool that fills its VG has
	// none, and LVM's own message ("insufficient free space (0 extents): 11
	// required") reads like a size problem rather than a provisioning decision.
	free, err := vgFreeBytes(ctx, spec)
	if err != nil {
		return res, err
	}
	if free < meta {
		// The pool cannot be shrunk to make room: LVM refuses to reduce a thin
		// pool's data volume ("cannot be reduced in size yet"), so the only way to
		// free extents is to add some. Growing the backing file by a gigabyte is
		// cheap and `storage grow` leaves exactly the reserve free afterwards.
		return res, fmt.Errorf("cannot repair in place: %s has %d bytes free and the repair needs %d for the replacement metadata. "+
			"Free extents first — the pool cannot be shrunk, so grow the backing file and let `pmcluster storage grow` keep the reserve "+
			"(e.g. `pmcluster storage grow --size <current+1G>`) — or pass --rebuild to destroy and re-create the volume",
			spec.VG, free, meta)
	}

	err = lvmlock.With(ctx, func() error {
		unmounted, err := releaseVolume(ctx, spec, opts)
		if err != nil {
			return err
		}
		res.Unmounted = unmounted

		before, err := backupMetadataLVs(ctx, spec)
		if err != nil {
			return err
		}
		// Deactivate the data volume before the pool: deactivating the pool under
		// a live thin volume fails, and it fails after the repair has started.
		if out, err := spec.exec(ctx, "lvchange", "-an", spec.VG+"/"+spec.LV); err != nil && !isNotActive(out, err) {
			return fmt.Errorf("deactivate %s/%s: %w (%s)", spec.VG, spec.LV, err, strings.TrimSpace(string(out)))
		}
		if out, err := spec.exec(ctx, "lvchange", "-an", spec.VG+"/pool"); err != nil && !isNotActive(out, err) {
			return fmt.Errorf("deactivate %s/pool: %w (%s)", spec.VG, err, strings.TrimSpace(string(out)))
		}

		out, rerr := spec.exec(ctx, "lvconvert", "--repair", "-y", spec.VG+"/pool")
		res.RepairOutput = strings.TrimSpace(string(out))

		// Whether lvconvert "succeeded" is decided by the probe below, not by its
		// exit status: it exits 0 after printing that it could not repair.
		reactivate(ctx, spec)
		if err := remount(ctx, spec); err != nil {
			return err
		}
		if rerr != nil {
			return fmt.Errorf("lvconvert --repair: %w (%s); the volume is back in service", rerr, res.RepairOutput)
		}

		if perr := Probe(ctx, spec); perr != nil {
			return fmt.Errorf("repair did not restore snapshotting (%v); the volume is back in service but the pool metadata is still suspect — "+
				"run `pmcluster storage repair --rebuild --allow-data-loss` on a node whose data you can lose, and open an issue with the output above", perr)
		}
		res.Verified = true

		res.BackupLV = findNewBackup(before, res.RepairOutput)
		if res.BackupLV != "" && !opts.KeepBackup {
			if out, err := spec.exec(ctx, "lvremove", "-f", spec.VG+"/"+res.BackupLV); err != nil {
				return fmt.Errorf("repair verified, but the metadata backup %s/%s could not be removed: %w (%s)",
					spec.VG, res.BackupLV, err, strings.TrimSpace(string(out)))
			}
		}
		return nil
	})
	return res, err
}

// rebuild destroys the volume and creates it again — the last exit, and the one
// that was improvised by hand on a production node before this command existed.
func rebuild(ctx context.Context, spec Spec, opts RepairOptions, res RepairResult) (RepairResult, error) {
	res.Mode = "rebuild"
	if !opts.Unmount {
		return res, fmt.Errorf("--rebuild has to unmount %s first; pass --unmount (it releases every mount point of the volume, including the docker bind mounts inside it)", spec.VolumeRoot)
	}

	err := lvmlock.With(ctx, func() error {
		// Sizes are measured before anything is destroyed: the rebuilt volume has
		// to be the same shape, or the next backup run sees a different volume
		// than the last one did.
		poolBytes, dataBytes := int64(0), int64(0)
		if out, err := spec.exec(ctx, "lvs", "--noheadings", "--units", "b", "-o", "lv_size", spec.VG+"/pool"); err == nil {
			poolBytes, _ = parseLVMBytes(out)
		}
		if out, err := spec.exec(ctx, "lvs", "--noheadings", "--units", "b", "-o", "lv_size", spec.VG+"/"+spec.LV); err == nil {
			dataBytes, _ = parseLVMBytes(out)
		}
		if poolBytes <= 0 && spec.Size > 0 {
			poolBytes = PoolSize(spec.Size)
		}
		if dataBytes <= 0 && poolBytes > 0 {
			dataBytes = DataSize(poolBytes)
		}
		if poolBytes <= 0 || dataBytes <= 0 {
			return fmt.Errorf("cannot size the rebuilt volume: unknown pool/data size (pass --size)")
		}
		res.PoolBytes, res.DataBytes = poolBytes, dataBytes
		res.MetadataBytes = poolMetaSize(poolBytes)

		unmounted, err := releaseVolume(ctx, spec, opts)
		if err != nil {
			return err
		}
		res.Unmounted = unmounted

		steps := [][]string{
			{"lvchange", "-an", spec.VG + "/" + spec.LV},
			{"lvchange", "-an", spec.VG + "/pool"},
			{"vgremove", "-f", spec.VG},
			{"vgcreate", "-q", spec.VG, spec.Device},
			{"lvcreate", "-q", "-T", spec.VG + "/pool", "-L", strconv.FormatInt(poolBytes, 10) + "B",
				"--poolmetadatasize", strconv.FormatInt(res.MetadataBytes, 10) + "B"},
			{"lvcreate", "-q", "-V", strconv.FormatInt(dataBytes, 10) + "B", "-T", spec.VG + "/pool", "-n", spec.LV},
			{"mke2fs", "-q", "-t", "ext4", "-L", "pmcluster-data", spec.device()},
		}
		for _, argv := range steps {
			out, err := spec.exec(ctx, argv[0], argv[1:]...)
			if err != nil {
				// The deactivations are best-effort: a volume that is already
				// inactive is exactly the state the next step wants.
				if argv[0] == "lvchange" && isNotActive(out, err) {
					continue
				}
				return fmt.Errorf("%s: %w (%s)", strings.Join(argv[:2], " "), err, strings.TrimSpace(string(out)))
			}
		}
		if err := remount(ctx, spec); err != nil {
			return err
		}
		if err := Probe(ctx, spec); err != nil {
			return fmt.Errorf("the volume was rebuilt but cannot snapshot (%v) — this is a bug, not an operator mistake", err)
		}
		res.Verified = true
		return nil
	})
	return res, err
}

// releaseVolume unmounts every mount point of the volume, innermost first.
//
// Innermost first matters on a real node: docker binds stack directories from
// inside the volume, so /var/stack/data has child mounts, and unmounting the
// parent while a child is still there fails with EBUSY.
func releaseVolume(ctx context.Context, spec Spec, opts RepairOptions) ([]string, error) {
	src := spec.device()
	if st, err := detect(ctx, spec); err == nil && st.Source != "" {
		src = st.Source
	}
	out, err := spec.exec(ctx, "findmnt", "-rn", "-S", src, "-o", "TARGET")
	if err != nil {
		return nil, nil // not mounted: nothing to release
	}
	var targets []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	if !opts.Unmount {
		return targets, fmt.Errorf("%s is mounted at %s — stop its writers first (docker service scale …=0), then re-run with --unmount",
			src, strings.Join(targets, ", "))
	}
	// Deepest path first: /var/stack/data/a/b before /var/stack/data.
	for i := len(targets) - 1; i >= 0; i-- {
		if out, err := spec.exec(ctx, "umount", targets[i]); err != nil {
			return targets, fmt.Errorf("unmount %s: %w (%s) — something is still using it", targets[i], err, strings.TrimSpace(string(out)))
		}
	}
	return targets, nil
}

// remount brings the volume root back, through the platform's mount unit when it
// exists so systemd's view and reality agree.
func remount(ctx context.Context, spec Spec) error {
	if out, err := spec.exec(ctx, "systemctl", "start", DefaultMountUnit); err == nil {
		return nil
	} else if out2, err2 := spec.exec(ctx, "mount", "-o", "rw,noatime", spec.device(), spec.VolumeRoot); err2 != nil {
		return fmt.Errorf("remount %s: %w (%s / %s)", spec.VolumeRoot, err2,
			strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
	}
	_ = os.MkdirAll(spec.VolumeRoot, 0o755)
	return nil
}

// reactivate re-activates the pool and the data volume, best effort: the caller
// reports a failure to remount, which is the one an operator would notice.
func reactivate(ctx context.Context, spec Spec) {
	_, _ = spec.exec(ctx, "lvchange", "-ay", spec.VG+"/pool")
	_, _ = spec.exec(ctx, "lvchange", "-ay", spec.VG+"/"+spec.LV)
}

// backupMetadataLVs lists the metadata backup volumes present before a repair,
// so the one the repair creates can be told apart from an older one.
func backupMetadataLVs(ctx context.Context, spec Spec) (map[string]bool, error) {
	out, err := spec.exec(ctx, "lvs", "--noheadings", "-o", "lv_name", spec.VG)
	if err != nil {
		return nil, fmt.Errorf("list volumes in %s: %w (%s)", spec.VG, err, strings.TrimSpace(string(out)))
	}
	seen := map[string]bool{}
	for _, f := range strings.Fields(string(out)) {
		seen[strings.TrimSpace(f)] = true
	}
	return seen, nil
}

// findNewBackup names the metadata backup this repair created: the newest
// <pool>_metaN that was not there before.
func findNewBackup(before map[string]bool, repairOutput string) string {
	for _, f := range strings.Fields(repairOutput) {
		name := strings.TrimSuffix(f, "/")
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		if strings.HasPrefix(name, "pool_meta") && !before[name] {
			return name
		}
	}
	return ""
}

// isNotActive reports whether a failed deactivation failed because the volume was
// already inactive, which is not an error worth stopping for.
func isNotActive(out []byte, err error) bool {
	s := strings.ToLower(string(out) + " " + err.Error())
	return strings.Contains(s, "not active") || strings.Contains(s, "not found")
}

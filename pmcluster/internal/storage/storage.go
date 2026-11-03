// Package storage provisions the volume that holds every stack's data.
//
// The point of this package is a property the backup agent cannot create for
// itself: /var/stack/data must be a mount of its own snapshot-capable
// filesystem, not a directory on the root filesystem. A directory on ext4 has
// no independent identity, so the only copy available is a file-by-file walk of
// a tree applications are still writing to — the failure mode that lost 18 of
// 19 stacks their backup in a single hour.
//
// The default mode is a loop device over a sparse file on the root filesystem,
// deliberately: a second disk or an attached cloud volume is something the
// platform can only *assume* an operator performed, and this platform's promise
// is that it provisions itself. A file is a thing pmcluster can create, size,
// verify and remove on its own.
package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Defaults. The VG name is fixed rather than derived from the hostname: it
// appears in unit files, in `findmnt` output an operator reads under pressure,
// and in the backup agent's --vg flag, and a name that changes per node makes
// all three harder to compare across a fleet.
const (
	DefaultVolumeRoot = "/var/stack/data"
	DefaultBackupRoot = "/var/stack/backup"
	DefaultImagePath  = "/var/lib/pmcluster/app-data.img"
	DefaultVG         = "vg-pmcluster"
	DefaultLV         = "data"
	DefaultUnitDir    = "/etc/systemd/system"
	DefaultMountUnit  = "var-stack-data.mount"
	DefaultLoopUnit   = "pmcluster-data-loop.service"

	// Pool headroom: the data volume is deliberately smaller than the pool it
	// lives in. Every backup run allocates blocks in the pool for the snapshot's
	// divergent writes, and a volume that already fills its pool has nowhere to
	// put them — the snapshot invalidates mid-copy and the run aborts. 80% gives
	// a run room to breathe without wasting the disk on a node that never fills.
	dataVolumeShare = 0.80
	// LVM metadata and chunk overhead live inside the pool's size.
	poolOverhead = 1 << 30
	// Auto-sizing bounds: below 8 GiB a pool cannot hold a realistic app plus a
	// snapshot, and above 128 GiB a single-node file is the wrong shape.
	minAutoSize = 8 << 30
	maxAutoSize = 128 << 30
	// Fraction of free space claimed on a fresh install. Half leaves the root
	// filesystem room for images, logs and the packages Docker pulls.
	autoFreeShare = 0.50
)

// String lets a Mode be stored in a setting or a flag without a cast at every
// call site.
func (m Mode) String() string { return string(m) }

// Mode is how the data volume is backed.
type Mode string

const (
	// ModeLoop is LVM on a loop device over a sparse file. Default.
	ModeLoop Mode = "loop"
	// ModeLVM is LVM on a block device the operator supplied (a second disk or
	// an attached volume). Supported because some hosts already have one, not
	// because the platform should need it.
	ModeLVM Mode = "lvm"
	// ModeNone leaves the volume root as a plain directory: no snapshots, and
	// the backup agent refuses to run against it.
	ModeNone Mode = "none"
)

// Execer runs one command. Production is ExecCommand; tests substitute a fake so
// the whole provisioning sequence is exercised without touching a disk.
type Execer func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecCommand is the real Execer.
func ExecCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Spec describes the volume to provision. The zero value means "loop mode,
// defaults, auto size", which is what a fresh install passes.
type Spec struct {
	Mode Mode
	// VolumeRoot is the mount point (/var/stack/data).
	VolumeRoot string
	// ImagePath is the backing file in loop mode.
	ImagePath string
	// VG, LV name the volume group and the data volume inside it.
	VG, LV string
	// Device is the block device in ModeLVM.
	Device string
	// Size is the backing file size in bytes; 0 means derive it from free space.
	Size int64
	// UnitDir is where systemd units are written.
	UnitDir string
	// Force skips the "a container is still writing" check during a migration.
	// It exists for an operator who has frozen the writers themselves, not as a
	// convenience: copying a live database produces the torn image this design
	// exists to prevent.
	Force bool
	// Exec runs commands; nil means ExecCommand.
	Exec Execer
}

// withDefaults fills in every field the caller left blank.
func (s Spec) withDefaults() Spec {
	if s.Mode == "" {
		s.Mode = ModeLoop
	}
	if s.VolumeRoot == "" {
		s.VolumeRoot = DefaultVolumeRoot
	}
	if s.ImagePath == "" {
		s.ImagePath = DefaultImagePath
	}
	if s.VG == "" {
		s.VG = DefaultVG
	}
	if s.LV == "" {
		s.LV = DefaultLV
	}
	if s.UnitDir == "" {
		s.UnitDir = DefaultUnitDir
	}
	return s
}

func (s Spec) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	e := s.Exec
	if e == nil {
		e = ExecCommand
	}
	return e(ctx, name, args...)
}

// Device is the path the mount unit and the snapshotter both use.
func (s Spec) device() string { return "/dev/" + s.VG + "/" + s.LV }

// mapperName is the device-mapper name LVM gives VG/LV. Every dash in either
// name is doubled, so vg-pmcluster/data becomes vg--pmcluster-data — the reason
// a naive "mapper/<vg>-<lv>" comparison never matches a hyphenated VG.
func (s Spec) mapperName() string {
	return "mapper/" + strings.ReplaceAll(s.VG, "-", "--") + "-" + strings.ReplaceAll(s.LV, "-", "--")
}

// State is what is on disk right now.
type State struct {
	Root string `json:"root"`
	// Mounted reports whether Root is a mount point at all.
	Mounted bool `json:"mounted"`
	// Source is what it is mounted from (findmnt), e.g. /dev/mapper/vg--pmcluster-data.
	Source string `json:"source,omitempty"`
	// Ours reports whether that source resolves to this spec's VG/LV.
	Ours bool `json:"ours"`
	// Entries counts entries in Root, ignoring lost+found (every fresh
	// filesystem has one and it belongs to no stack).
	Entries int `json:"entries"`
	// ImageExists/ImageBytes describe the backing file in loop mode.
	ImageExists bool  `json:"image_exists"`
	ImageBytes  int64 `json:"image_bytes,omitempty"`
	// LoopDevice is the loop device the image is currently attached to, if any.
	LoopDevice string `json:"loop_device,omitempty"`
	// VGExists reports whether the volume group is present.
	VGExists bool `json:"vg_exists"`
}

// Provisioned is the one predicate that matters: the volume root is a mount of
// our snapshot-capable volume, so backup runs can be atomic.
func (s State) Provisioned() bool { return s.Mounted && s.Ours }

// SnapshotReady is the same question in the terms an operator asks it.
func (s State) SnapshotReady() bool { return s.Provisioned() }

// Detect inspects the host. It never writes anything, which is what makes it
// safe to call from `status`, from the install wizard, and from the daemon's
// pre-flight.
func Detect(ctx context.Context, spec Spec) (State, error) {
	spec = spec.withDefaults()
	st := State{Root: spec.VolumeRoot}

	// --mountpoint, not --target: --target also matches the mount that merely
	// CONTAINS the path, so on every ordinary node it answers /dev/vda1 for
	// /var/stack/data and "is it mounted" becomes always-true. That made the
	// first version of Provision refuse to provision any fresh node ("mounted
	// from /dev/vda1, not from /dev/vg-pmcluster/data"). --mountpoint fails
	// unless the path is itself a mount point, which is the question.
	if out, err := spec.exec(ctx, "findmnt", "-no", "SOURCE", "--mountpoint", spec.VolumeRoot); err == nil {
		if src := strings.TrimSpace(string(out)); src != "" {
			st.Mounted = true
			st.Source = src
			st.Ours = sameMount(ctx, spec, src)
		}
	}

	entries, err := os.ReadDir(spec.VolumeRoot)
	if err != nil && !os.IsNotExist(err) {
		return st, fmt.Errorf("read %s: %w", spec.VolumeRoot, err)
	}
	for _, e := range entries {
		if e.Name() == "lost+found" {
			continue
		}
		st.Entries++
	}

	if fi, err := os.Stat(spec.ImagePath); err == nil {
		st.ImageExists = true
		st.ImageBytes = fi.Size()
	}
	if out, err := spec.exec(ctx, "losetup", "-j", spec.ImagePath); err == nil {
		if line := strings.TrimSpace(string(out)); line != "" {
			if dev, _, ok := strings.Cut(line, ":"); ok {
				st.LoopDevice = strings.TrimSpace(dev)
			}
		}
	}
	if out, err := spec.exec(ctx, "vgs", "--noheadings", "-o", "vg_name"); err == nil {
		for _, f := range strings.Fields(string(out)) {
			if f == spec.VG {
				st.VGExists = true
			}
		}
	}
	return st, nil
}

// sameMount reports whether src is the spec's LV. Two independent comparisons
// because findmnt prints the mapper node while `lvm` calls it /dev/<vg>/<lv>:
// resolving both through readlink settles it when the links exist, and the
// escaped mapper name settles it when they don't.
func sameMount(ctx context.Context, spec Spec, src string) bool {
	want := spec.device()
	if strings.Contains(src, spec.mapperName()) || strings.Contains(src, want) {
		return true
	}
	gotResolved, gerr := spec.exec(ctx, "readlink", "-f", src)
	if gerr != nil {
		return false
	}
	wantResolved, werr := spec.exec(ctx, "readlink", "-f", want)
	if werr != nil {
		return false
	}
	a, b := strings.TrimSpace(string(gotResolved)), strings.TrimSpace(string(wantResolved))
	return a != "" && a == b
}

// AutoSize derives the backing file size from free space on the filesystem that
// will hold it.
func AutoSize(freeBytes int64) int64 {
	size := int64(float64(freeBytes) * autoFreeShare)
	if size < minAutoSize {
		return minAutoSize
	}
	if size > maxAutoSize {
		return maxAutoSize
	}
	return size
}

// roundDown rounds n down to a multiple of unit.
//
// LVM refuses sector-misaligned sizes ("Size is not a multiple of 512"), and the
// arithmetic that produces them is invisible until it is not: 4G - 1G = 3 GiB,
// and 80% of that is 2,576,980,377 bytes. The VM test used 6G, whose 80% happens
// to be exact, so only a differently-sized disk in CI surfaced it.
func roundDown(n, unit int64) int64 {
	if unit <= 0 {
		return n
	}
	return n / unit * unit
}

// sectorAlign is the granularity every size must respect: 64 KiB is one thin-pool
// chunk, which is both a multiple of 512-byte sectors and of the ext4 block size.
const sectorAlign = 64 << 10

// PoolSize is the thin pool inside a backing file of the given size.
func PoolSize(fileBytes int64) int64 {
	if fileBytes <= poolOverhead {
		return roundDown(fileBytes, sectorAlign)
	}
	return roundDown(fileBytes-poolOverhead, sectorAlign)
}

// DataSize is the data volume inside a pool of the given size. Leaving a fifth
// of the pool unallocated is what lets a backup run's snapshot exist at all.
func DataSize(poolBytes int64) int64 {
	return roundDown(int64(float64(poolBytes)*dataVolumeShare), sectorAlign)
}

// Result reports what happened, in the terms a wizard or a log line needs.
type Result struct {
	// AlreadyProvisioned means Detect found the volume in place: the call was a
	// no-op, which is what makes this safe to re-run on every join and upgrade.
	AlreadyProvisioned bool     `json:"already_provisioned"`
	Mode               Mode     `json:"mode"`
	Device             string   `json:"device,omitempty"`
	ImagePath          string   `json:"image_path,omitempty"`
	ImageBytes         int64    `json:"image_bytes,omitempty"`
	PoolBytes          int64    `json:"pool_bytes,omitempty"`
	DataBytes          int64    `json:"data_bytes,omitempty"`
	UnitsWritten       []string `json:"units_written,omitempty"`
	// Copied / CopyBytes describe a migration's copy, and PreviousTree is where
	// the pre-migration directory was preserved.
	Copied       int    `json:"copied_files,omitempty"`
	CopyBytes    int64  `json:"copied_bytes,omitempty"`
	PreviousTree string `json:"previous_tree,omitempty"`
	// Warnings are things the operator should know but that did not stop the
	// provisioning (an auto-size clamped to the minimum, for instance).
	Warnings []string `json:"warnings,omitempty"`
}

// Provision creates the volume and installs the units that bring it back after
// a reboot.
//
// It refuses in two cases rather than doing something surprising with data: an
// existing directory full of stack data (provisioning would shadow it, and the
// data would look lost until somebody unmounted the new volume), and a volume
// root mounted from something that is not our LV (provisioning would snapshot
// the wrong filesystem).
func Provision(ctx context.Context, spec Spec) (Result, error) {
	spec = spec.withDefaults()
	var res Result
	res.Mode = spec.Mode

	st, err := Detect(ctx, spec)
	if err != nil {
		return res, err
	}
	if st.Provisioned() {
		res.AlreadyProvisioned = true
		res.Device = spec.device()
		// Converge the units even though the volume is up. "Already provisioned"
		// must not mean "already correct": an upgrade that fixes the boot
		// ordering heals a node only if provisioning rewrites the units on a
		// node that is already running.
		if err := writeUnits(ctx, spec, &res); err != nil {
			return res, err
		}
		return res, nil
	}
	if spec.Mode == ModeNone {
		res.Warnings = append(res.Warnings, "mode none: the volume root stays a plain directory and snapshot backups stay unavailable")
		return res, nil
	}
	if st.Mounted && !st.Ours {
		return res, fmt.Errorf("%s is mounted from %s, not from %s — refusing to provision over a volume this node does not own", spec.VolumeRoot, st.Source, spec.device())
	}
	// Storage that exists but is not mounted is the boot-failure case — a
	// reboot where the pool unit did not run leaves the volume group inactive
	// and the data untouched inside it. Rebuilding here would be catastrophic
	// (the file already carries a PV) and unnecessary: bring it up instead.
	if (spec.Mode == ModeLoop && st.ImageExists && st.ImageBytes > 0) || (st.VGExists && st.LoopDevice != "") {
		if err := attachExisting(ctx, spec, st, &res); err == nil {
			res.AlreadyProvisioned = true
			return res, nil
		} else if st.VGExists {
			// The volume group is real but would not activate: that is a repair
			// failure, not permission to rebuild over it.
			return res, fmt.Errorf("storage exists but cannot be brought up: %w", err)
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf("backing file present but holds no volume group (%v); building it now", err))
		}
	}

	// lost+found is excluded by Detect, so a freshly created but unmounted
	// filesystem does not block its own provisioning on a second run.
	if st.Entries > 0 {
		return res, fmt.Errorf("%s already holds %d entries — refusing to provision over live data; move the stacks first (pmcluster stack move --all) or provision this node before it takes work", spec.VolumeRoot, st.Entries)
	}

	switch spec.Mode {
	case ModeLoop:
		if err := provisionLoop(ctx, spec, st, &res); err != nil {
			return res, err
		}
	case ModeLVM:
		if spec.Device == "" {
			return res, fmt.Errorf("mode lvm needs a block device")
		}
		if err := provisionBlockDevice(ctx, spec, st, &res); err != nil {
			return res, err
		}
	default:
		return res, fmt.Errorf("unknown storage mode %q (want loop, lvm or none)", spec.Mode)
	}

	if err := writeUnits(ctx, spec, &res); err != nil {
		return res, err
	}
	return res, nil
}

// attachExisting activates storage that is already built and mounts it.
//
// It is the recovery path for the one failure that matters: a node whose pool
// unit did not run at boot. The data is intact inside the volume group; all that
// is missing is `vgchange -ay` and the mount. Rebuilding instead would either
// fail (the loop device already carries a physical volume) or, worse, succeed
// over live data.
func attachExisting(ctx context.Context, spec Spec, st State, res *Result) error {
	if st.LoopDevice == "" {
		if out, err := spec.exec(ctx, "losetup", "--find", "--show", spec.ImagePath); err != nil {
			return fmt.Errorf("attach loop device: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := spec.exec(ctx, "vgchange", "-ay", spec.VG); err != nil {
		return fmt.Errorf("activate %s: %w (%s)", spec.VG, err, strings.TrimSpace(string(out)))
	}
	// writeUnits starts the mount unit, which verifies the result.
	if err := writeUnits(ctx, spec, res); err != nil {
		return err
	}
	res.Device = spec.device()
	return nil
}

// provisionLoop builds the volume from a sparse file. The LVM sequence lives in
// buildLoop so provisioning and migration cannot drift apart.
func provisionLoop(ctx context.Context, spec Spec, st State, res *Result) error {
	return buildLoop(ctx, spec, st, res)
}

// provisionBlockDevice builds the volume on a device the operator supplied.
func provisionBlockDevice(ctx context.Context, spec Spec, st State, res *Result) error {
	var size int64
	if out, err := spec.exec(ctx, "blockdev", "--getsize64", spec.Device); err == nil {
		size, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}
	res.PoolBytes = PoolSize(size)
	res.DataBytes = DataSize(res.PoolBytes)

	steps := [][]string{
		{"pvcreate", "-q", spec.Device},
		{"vgcreate", "-q", spec.VG, spec.Device},
		{"lvcreate", "-q", "-T", spec.VG + "/pool", "-L", strconv.FormatInt(res.PoolBytes, 10) + "B"},
		{"lvcreate", "-q", "-V", strconv.FormatInt(res.DataBytes, 10) + "B", "-T", spec.VG + "/pool", "-n", spec.LV},
		{"mke2fs", "-q", "-t", "ext4", "-L", "pmcluster-data", spec.device()},
	}
	for _, argv := range steps {
		if out, err := spec.exec(ctx, argv[0], argv[1:]...); err != nil {
			return fmt.Errorf("%s: %w (%s)", argv[0], err, strings.TrimSpace(string(out)))
		}
	}
	res.Device = spec.device()
	return nil
}

// freeBytes reads the available space on the filesystem holding dir.
func freeBytes(ctx context.Context, spec Spec, dir string) (int64, error) {
	out, err := spec.exec(ctx, "df", "-B1", "--output=avail", dir)
	if err != nil {
		return 0, fmt.Errorf("measure free space on %s: %w", dir, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, fmt.Errorf("measure free space on %s: unexpected df output %q", dir, string(out))
	}
	return strconv.ParseInt(fields[len(fields)-1], 10, 64)
}

// SnapshotVolumeRoot is exported for callers that only need the default.
func SnapshotVolumeRoot() string { return DefaultVolumeRoot }

// humanBytes formats a size for a warning or a CLI line.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

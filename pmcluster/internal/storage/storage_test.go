package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost stands in for the machine: it records every command, and its answers
// change as provisioning progresses (findmnt starts reporting the mapper once
// the mount is started), so the verify-after-provision step is exercised rather
// than assumed.
type fakeHost struct {
	calls []string
	// mountSource is what findmnt --mountpoint answers: empty means the volume
	// root is NOT a mount point (a plain directory on the root filesystem).
	mountSource  string
	loopAttached string
	vg           bool
	free         string
	failOn       string
	// docker answers for the migration writer check.
	dockerPs     string
	dockerMounts string
}

func (f *fakeHost) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, line)
	if f.failOn != "" && strings.HasPrefix(line, f.failOn) {
		return []byte("simulated failure"), errors.New("exit status 1")
	}
	switch name {
	case "findmnt":
		if f.mountSource == "" {
			return nil, errors.New("exit status 1") // not a mount point
		}
		return []byte(f.mountSource + "\n"), nil
	case "readlink":
		return []byte(args[len(args)-1] + "\n"), nil
	case "losetup":
		if args[0] == "-j" {
			if f.loopAttached != "" {
				return []byte(f.loopAttached + ": []: (offset=0)\n"), nil
			}
			return nil, nil
		}
		f.loopAttached = "/dev/loop0"
		return []byte("/dev/loop0\n"), nil
	case "vgs":
		if f.vg {
			return []byte("  vg-pmcluster\n"), nil
		}
		return nil, nil
	case "df":
		return []byte("Avail\n" + f.free + "\n"), nil
	case "docker":
		if len(args) > 0 && args[0] == "ps" {
			if f.dockerPs == "" {
				return nil, nil
			}
			return []byte(f.dockerPs + "\n"), nil
		}
		if f.dockerMounts != "" {
			return []byte(f.dockerMounts), nil
		}
		return []byte("[]"), nil
	case "systemctl":
		if len(args) > 0 && args[0] == "start" {
			f.mountSource = "/dev/mapper/vg--pmcluster-data"
		}
		return nil, nil
	}
	return nil, nil
}

func (f *fakeHost) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// A path root for each test: an empty directory to provision and a scratch unit
// directory, so no test touches /etc/systemd.
func testSpec(t *testing.T, h *fakeHost) Spec {
	t.Helper()
	base := t.TempDir()
	return Spec{
		Mode:       ModeLoop,
		VG:         DefaultVG,
		LV:         DefaultLV,
		VolumeRoot: filepath.Join(base, "data"),
		ImagePath:  filepath.Join(base, "lib", "app-data.img"),
		UnitDir:    filepath.Join(base, "units"),
		Size:       16 << 30,
		Exec:       h.exec,
	}
}

// hasDirective reports whether a unit file contains KEY=VALUE as a real
// directive. Comments are skipped deliberately: the loop unit's comment explains
// which ordering it must NOT have, and a plain substring search for that
// ordering then matches its own warning.
func hasDirective(content, key, value string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == key+"="+value {
			return true
		}
		for _, part := range strings.Fields(strings.TrimPrefix(line, key+"=")) {
			if line == key+"="+value || (strings.HasPrefix(line, key+"=") && part == value) {
				return true
			}
		}
	}
	return false
}

func TestDetectRecognisesOurHyphenatedVolume(t *testing.T) {
	// The mapper name doubles every dash in the VG, so vg-pmcluster/data is
	// vg--pmcluster-data. A naive "mapper/<vg>-<lv>" comparison never matches a
	// hyphenated VG — that is the bug this pins.
	h := &fakeHost{mountSource: "/dev/mapper/vg--pmcluster-data"}
	spec := testSpec(t, h)
	st, err := Detect(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mounted || !st.Ours || !st.Provisioned() {
		t.Fatalf("state = %+v, want a provisioned volume", st)
	}
	if st.Source != "/dev/mapper/vg--pmcluster-data" {
		t.Fatalf("source = %q", st.Source)
	}
}

func TestDetectRejectsAPlainDirectoryOnTheRootFilesystem(t *testing.T) {
	h := &fakeHost{} // findmnt --mountpoint fails: a plain directory
	spec := testSpec(t, h)
	st, err := Detect(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mounted || st.Ours || st.Provisioned() {
		t.Fatalf("state = %+v, want an unmounted plain directory", st)
	}
	// The other half of the same question: a real mount that is not ours must be
	// refused, because provisioning would snapshot somebody else's filesystem.
	h.mountSource = "/dev/sdb1"
	foreign, err := Detect(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !foreign.Mounted || foreign.Ours || foreign.Provisioned() {
		t.Fatalf("state = %+v, want mounted-but-foreign", foreign)
	}
}

func TestProvisionRefusesToShadowExistingData(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	if err := os.MkdirAll(filepath.Join(spec.VolumeRoot, "horizon-erp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(context.Background(), spec); err == nil {
		t.Fatal("Provision accepted a volume root that already holds stack data")
	} else if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("error does not explain the refusal: %v", err)
	}
	// Nothing may have been created: provisioning over live data is the one
	// mistake that looks like recovery and ends as deletion.
	// Detect may read (losetup -j, vgs); nothing may write.
	for _, mutating := range []string{"truncate", "losetup --find", "pvcreate", "vgcreate", "lvcreate", "mke2fs", "systemctl"} {
		if h.ran(mutating) {
			t.Fatalf("provisioning ran %q before refusing: %v", mutating, h.calls)
		}
	}
}

func TestProvisionIgnoresLostAndFound(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	if err := os.MkdirAll(filepath.Join(spec.VolumeRoot, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(context.Background(), spec); err != nil {
		t.Fatalf("lost+found blocked provisioning: %v", err)
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	h := &fakeHost{mountSource: "/dev/mapper/vg--pmcluster-data"}
	spec := testSpec(t, h)
	res, err := Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.AlreadyProvisioned {
		t.Fatalf("result = %+v, want already provisioned", res)
	}
	// No disk work on a provisioned node — but the units ARE rewritten, so an
	// upgrade that fixes the boot ordering converges a running node.
	for _, mutating := range []string{"truncate", "pvcreate", "vgcreate", "lvcreate", "mke2fs"} {
		if h.ran(mutating) {
			t.Fatalf("a provisioned node was re-provisioned (%q): %v", mutating, h.calls)
		}
	}
	if !h.ran("systemctl daemon-reload") {
		t.Fatalf("units were not converged on a provisioned node: %v", h.calls)
	}
}

func TestProvisionLoopBuildsTheVolumeAndSizesItForSnapshots(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	res, err := Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	order := []string{"truncate -s 17179869184", "losetup --find --show", "pvcreate -q /dev/loop0",
		"vgcreate -q vg-pmcluster /dev/loop0", "lvcreate -q -T vg-pmcluster/pool", "lvcreate -q -V",
		"mke2fs -q -t ext4", "systemctl daemon-reload", "systemctl enable pmcluster-data-loop.service",
		"systemctl enable var-stack-data.mount", "systemctl start var-stack-data.mount"}
	last := -1
	for _, want := range order {
		idx := -1
		for i, c := range h.calls {
			if strings.HasPrefix(c, want) && i > last {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("missing step %q (or out of order); calls: %v", want, h.calls)
		}
		last = idx
	}

	// The data volume must be smaller than its pool, or the first backup run has
	// nowhere to put the snapshot's divergent writes.
	if res.PoolBytes != 15<<30 {
		t.Fatalf("pool = %d, want 15 GiB (file minus overhead)", res.PoolBytes)
	}
	if res.DataBytes != 12<<30 {
		t.Fatalf("data = %d, want 12 GiB (80%% of the pool)", res.DataBytes)
	}
	if !strings.Contains(strings.Join(h.calls, "\n"), "lvcreate -q -V 12884901888B") {
		t.Fatalf("data volume not sized with its headroom: %v", h.calls)
	}

	// The units land on disk with the ordering that survives a reboot.
	mount, err := os.ReadFile(filepath.Join(spec.UnitDir, DefaultMountUnit))
	if err != nil {
		t.Fatal(err)
	}
	// The mount must NOT be ordered before local-fs.target: the backing file
	// lives on /var, which local-fs.target mounts, so that ordering is a cycle
	// systemd resolves by silently deleting the mount job. This assertion is the
	// third version of this test, and it exists because the first two were wrong
	// in exactly this place.
	if hasDirective(string(mount), "Before", "local-fs.target") {
		t.Fatalf("mount unit is ordered before local-fs.target again — a volume cannot mount before the filesystem holding its backing file:\n%s", mount)
	}
	if !hasDirective(string(mount), "After", DefaultLoopUnit) {
		t.Fatalf("mount unit does not wait for the pool unit:\n%s", mount)
	}
	loop, err := os.ReadFile(filepath.Join(spec.UnitDir, DefaultLoopUnit))
	if err != nil {
		t.Fatal(err)
	}
	// The pool unit MUST be after local-fs.target: that is what mounts /var,
	// where the backing file lives. Before it, the file path resolves to an
	// empty directory on the root filesystem and the unit fails at boot — found
	// by rebooting a Fedora CoreOS box with /var on its own filesystem.
	if !hasDirective(string(loop), "After", "local-fs.target") {
		t.Fatalf("loop unit is not ordered after local-fs.target; on a host with a separate /var it will run before its own backing file exists:\n%s", loop)
	}
	if !hasDirective(string(loop), "Before", DefaultMountUnit) {
		t.Fatalf("loop unit does not precede the mount:\n%s", loop)
	}
	// Every consumer that reads stack data must be ordered behind the mount,
	// because the mount is no longer reached via local-fs.target.
	for _, consumer := range []string{"docker.service.d", "pmcluster.service.d", "pmcluster-backup.service.d"} {
		drop, err := os.ReadFile(filepath.Join(spec.UnitDir, consumer, "pmcluster-data.conf"))
		if err != nil {
			t.Fatalf("%s: %v", consumer, err)
		}
		if !hasDirective(string(drop), "RequiresMountsFor", spec.VolumeRoot) {
			t.Fatalf("%s drop-in does not require the mount:\n%s", consumer, drop)
		}
	}
}

func TestProvisionAutoSizesAndWarnsWhenSpaceIsTight(t *testing.T) {
	h := &fakeHost{free: "3000000000"} // 3 GB free
	spec := testSpec(t, h)
	spec.Size = 0
	res, err := Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.ImageBytes != minAutoSize {
		t.Fatalf("image = %d, want the %d minimum", res.ImageBytes, minAutoSize)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("no warning on a node too small to hold a pool")
	}
}

func TestProvisionAbortsWhenTheVolumeNeverMounts(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	// Findmnt keeps reporting the root filesystem: the mount silently did not
	// happen, which is the failure that would leave apps on an empty directory.
	spec.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := h.exec(ctx, name, args...)
		if name == "systemctl" && len(args) > 0 && args[0] == "start" {
			h.mountSource = "" // the mount silently did not take
		}
		return out, err
	}
	if _, err := Provision(context.Background(), spec); err == nil {
		t.Fatal("Provision reported success without the volume being mounted")
	}
}

func TestProvisionRepairsStorageThatExistsButIsNotMounted(t *testing.T) {
	// The boot-failure state: the backing file and the volume group are real,
	// the mount is not. Rebuilding would destroy the data; the fix is
	// vgchange + mount.
	h := &fakeHost{vg: true, loopAttached: "/dev/loop0"}
	spec := testSpec(t, h)
	if err := os.MkdirAll(filepath.Dir(spec.ImagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.ImagePath, []byte("pretend 6 GiB of thin pool"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Provision(context.Background(), spec)
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if !res.AlreadyProvisioned {
		t.Fatalf("result = %+v, want a repair reported as already provisioned", res)
	}
	for _, rebuild := range []string{"pvcreate", "vgcreate", "lvcreate", "mke2fs", "truncate"} {
		if h.ran(rebuild) {
			t.Fatalf("repair rebuilt storage (%q) instead of activating it: %v", rebuild, h.calls)
		}
	}
	if !h.ran("vgchange -ay vg-pmcluster") {
		t.Fatalf("repair did not activate the volume group: %v", h.calls)
	}
}

func TestProvisionFailsLoudlyOnALVMError(t *testing.T) {
	h := &fakeHost{failOn: "vgcreate"}
	spec := testSpec(t, h)
	if _, err := Provision(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "vgcreate") {
		t.Fatalf("error = %v, want the failing step named", err)
	}
}

// Every size handed to LVM must be a multiple of 512 bytes, and in practice of a
// 64 KiB chunk. Asserted across sizes that do not divide evenly — the case that
// only appeared on a runner with a differently-sized disk.
func TestSizesAreSectorAligned(t *testing.T) {
	for _, fileSize := range []int64{4 << 30, 5 << 30, 6 << 30, 7 << 30, 12<<30 + 12345, 100 << 30} {
		pool, data := PoolSize(fileSize), DataSize(PoolSize(fileSize))
		if pool%512 != 0 || data%512 != 0 {
			t.Fatalf("file %d: pool %d / data %d are not multiples of 512", fileSize, pool, data)
		}
		if pool%sectorAlign != 0 || data%sectorAlign != 0 {
			t.Fatalf("file %d: pool %d / data %d are not chunk-aligned", fileSize, pool, data)
		}
		if data > pool {
			t.Fatalf("file %d: data volume (%d) exceeds its pool (%d)", fileSize, data, pool)
		}
		// The headroom is the whole point of 80%: assert it survives rounding.
		if float64(data) > float64(pool)*dataVolumeShare {
			t.Fatalf("file %d: data %d exceeds %.0f%% of pool %d", fileSize, data, dataVolumeShare*100, pool)
		}
	}
}

func TestAutoSizeClamps(t *testing.T) {
	if got := AutoSize(4 << 30); got != minAutoSize {
		t.Fatalf("small disk = %d, want the minimum", got)
	}
	if got := AutoSize(4 << 40); got != maxAutoSize {
		t.Fatalf("huge disk = %d, want the maximum", got)
	}
	if got := AutoSize(200 << 30); got != 100<<30 {
		t.Fatalf("200 GiB free = %d, want half", got)
	}
}

func TestModeNoneLeavesTheVolumeRootAlone(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	spec.Mode = ModeNone
	res, err := Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyProvisioned || len(res.Warnings) == 0 {
		t.Fatalf("result = %+v, want a warning and no work", res)
	}
	if h.ran("truncate") || h.ran("systemctl") {
		t.Fatalf("mode none did work: %v", h.calls)
	}
}

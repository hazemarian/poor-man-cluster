package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptHost answers commands from a prefix→output table, longest prefix first.
//
// Separate from fakeHost on purpose: the repair path asks LVM questions that the
// provisioning fake has no opinion about (metadata size, free extents, the mount
// table), and teaching that one new tricks would have made the provisioning tests
// read like a mock of LVM.
type scriptHost struct {
	calls   []string
	answers map[string]string
	failing map[string]bool
}

func (h *scriptHost) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	h.calls = append(h.calls, line)
	best, out := "", ""
	for prefix := range h.answers {
		if strings.HasPrefix(line, prefix) && len(prefix) > len(best) {
			best, out = prefix, h.answers[prefix]
		}
	}
	for prefix := range h.failing {
		if strings.HasPrefix(line, prefix) {
			return []byte(out), errors.New("exit status 1")
		}
	}
	if best == "" {
		return nil, nil
	}
	return []byte(out), nil
}

func (h *scriptHost) index(prefix string) int {
	for i, c := range h.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// A pool that fits its backing file must still leave the volume group enough
// free extents for a metadata repair. Sizing it to the last byte is what left
// every node of the reference fleet unrepairable: healthy and unfixable at the
// same time.
func TestPoolSizeKeepsRoomForAMetadataRepair(t *testing.T) {
	for _, file := range []int64{8 << 30, 16 << 30, 42 << 30, 86 << 30, 377 << 30} {
		pool := PoolSize(file)
		free := file - poolOverhead - pool
		if want := repairReserve(pool); free < want {
			t.Errorf("file %d GiB: pool leaves %d bytes free in the VG, want at least %d for a repair",
				file>>30, free, want)
		}
		if want := file - poolOverhead; pool > want {
			t.Errorf("file %d GiB: pool %d is larger than the file allows (%d)", file>>30, pool, want)
		}
		if pool%poolMetaSize(pool) == 0 && pool <= poolOverhead {
			t.Errorf("file %d GiB: pool %d is degenerate", file>>30, pool)
		}
	}
}

// The metadata size is asked for, not inferred — and it has to be big enough that
// LVM does not refuse the pool ("metadata too small").
func TestPoolMetaSizeIsExplicitAndBounded(t *testing.T) {
	if got := poolMetaSize(8 << 30); got < poolMetaMin {
		t.Errorf("8 GiB pool asked for %d bytes of metadata, under the %d floor", got, poolMetaMin)
	}
	if got := poolMetaSize(1 << 40); got > poolMetaMax {
		t.Errorf("1 TiB pool asked for %d bytes of metadata, over the %d cap", got, poolMetaMax)
	}
	if got := poolMetaSize(42 << 30); got%poolMetaMin != 0 && got%(4<<20) != 0 {
		t.Errorf("metadata %d is not a whole number of 4 MiB blocks", got)
	}
}

// Growing the pool must not consume the reserve. The old implementation was
// `lvextend -l +100%FREE`, which is exactly the state this test forbids.
func TestGrowLeavesTheRepairReserveFree(t *testing.T) {
	h := &scriptHost{answers: map[string]string{
		"lvs --noheadings --units b -o lv_size vg-pmcluster/pool_tmeta": "50331648B\n", // 48 MiB
		"vgs --noheadings --units b -o vg_free vg-pmcluster":            "1048576B\n",  // 1 MiB
		"vgs ": "1048576B\n",
	}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Size: 16 << 30, Exec: h.exec}
	spec.ImagePath = filepath.Join(t.TempDir(), "app-data.img")
	if err := os.WriteFile(spec.ImagePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// vg_free is reported as 1 MiB and the reserve is 2×48 MiB, so there is
	// nothing to add: the command must say so instead of taking the last extent.
	err := grow(context.Background(), spec)
	if err == nil {
		t.Fatal("grow extended the pool into the reserve")
	}
	if !strings.Contains(err.Error(), "kept back") {
		t.Fatalf("error does not explain the reserve: %v", err)
	}
	if h.index("lvextend") >= 0 {
		t.Fatalf("lvextend ran anyway: %v", h.calls)
	}
}

func TestGrowExtendsByFreeMinusReserve(t *testing.T) {
	h := &scriptHost{answers: map[string]string{
		"lvs --noheadings --units b -o lv_size vg-pmcluster/pool_tmeta": "50331648B\n",
		"vgs --noheadings --units b -o vg_free vg-pmcluster":            "1073741824B\n",
	}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Size: 16 << 30, Exec: h.exec}
	// The backing file has to exist for grow to truncate it; the fake does not
	// care about its contents.
	dir := t.TempDir()
	spec.ImagePath = filepath.Join(dir, "app-data.img")
	if err := os.WriteFile(spec.ImagePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := grow(context.Background(), spec); err != nil {
		t.Fatalf("grow: %v", err)
	}
	extend := int64(1073741824 - 2*50331648)
	want := fmt.Sprintf("lvextend -q -L +%dB vg-pmcluster/pool", roundDown(extend, sectorAlign))
	if h.index(want) < 0 {
		t.Fatalf("want %q, calls: %v", want, h.calls)
	}
}

// The preflight that turns LVM's "insufficient free space (0 extents)" into a
// sentence an operator can act on.
func TestRepairRefusesInPlaceWithoutHeadroom(t *testing.T) {
	h := &scriptHost{answers: map[string]string{
		"vgs --noheadings -o vg_name":                                   "  vg-pmcluster\n",
		"lvs --noheadings --units b -o lv_size vg-pmcluster/pool_tmeta": "50331648B\n",
		"vgs --noheadings --units b -o vg_free vg-pmcluster":            "0B\n",
	}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	_, err := Repair(context.Background(), spec, RepairOptions{Unmount: true})
	if err == nil {
		t.Fatal("repair ran on a volume group with no free extents")
	}
	if !strings.Contains(err.Error(), "cannot repair in place") || !strings.Contains(err.Error(), "storage grow") {
		t.Fatalf("error does not explain the shortfall or how to fix it: %v", err)
	}
	// Shrinking a thin pool is not a thing LVM can do, and telling an operator to
	// try it wastes the worst possible minute of their day.
	if strings.Contains(err.Error(), "lvreduce") {
		t.Fatalf("error suggests shrinking a thin pool, which LVM refuses: %v", err)
	}
	if h.index("lvconvert") >= 0 {
		t.Fatalf("lvconvert ran anyway: %v", h.calls)
	}
}

// Unmounting is the operator's decision: a mounted volume is refused, and the
// refusal names the mount points so they know what to stop.
func TestRepairRefusesToUnmountWithoutBeingAsked(t *testing.T) {
	h := &scriptHost{answers: map[string]string{
		"vgs --noheadings -o vg_name":                                   "  vg-pmcluster\n",
		"lvs --noheadings --units b -o lv_size vg-pmcluster/pool_tmeta": "50331648B\n",
		"vgs --noheadings --units b -o vg_free vg-pmcluster":            "1073741824B\n",
		"findmnt -no SOURCE --mountpoint":                               "/dev/mapper/vg--pmcluster-data\n",
		"findmnt -rn -S":                                                "/var/stack/data\n/var/stack/data/app/data\n",
	}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	_, err := Repair(context.Background(), spec, RepairOptions{})
	if err == nil {
		t.Fatal("repair unmounted a live volume without --unmount")
	}
	if !strings.Contains(err.Error(), "--unmount") || !strings.Contains(err.Error(), "/var/stack/data") {
		t.Fatalf("error does not name the mounts and the flag: %v", err)
	}
	if h.index("umount") >= 0 {
		t.Fatalf("umount ran anyway: %v", h.calls)
	}
}

// The success path, end to end: deactivate data before the pool, repair, bring it
// back, and prove it with a snapshot — then remove LVM's metadata backup.
func TestRepairRepairsAndProvesItWithASnapshot(t *testing.T) {
	h := &scriptHost{answers: map[string]string{
		"vgs --noheadings -o vg_name":                                   "  vg-pmcluster\n",
		"lvs --noheadings --units b -o lv_size vg-pmcluster/pool_tmeta": "50331648B\n",
		"vgs --noheadings --units b -o vg_free vg-pmcluster":            "1073741824B\n",
		"lvs --noheadings -o lv_name vg-pmcluster":                      "  data\n  pool\n",
		"lvconvert --repair":                                            "WARNING: LV vg-pmcluster/pool_meta0 holds a backup of the unrepaired metadata.\n",
	}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	res, err := Repair(context.Background(), spec, RepairOptions{Unmount: true})
	if err != nil {
		t.Fatalf("repair: %v (calls: %v)", err, h.calls)
	}
	if !res.Verified || res.Mode != "repair" {
		t.Fatalf("result = %+v, want a verified in-place repair", res)
	}
	if res.BackupLV != "pool_meta0" {
		t.Fatalf("BackupLV = %q, want pool_meta0", res.BackupLV)
	}
	// Order: the data volume must be deactivated before the pool, or LVM refuses
	// to deactivate a pool that still has an active thin volume over it.
	off, pool := h.index("lvchange -an vg-pmcluster/data"), h.index("lvchange -an vg-pmcluster/pool")
	if off < 0 || pool < 0 || off > pool {
		t.Fatalf("deactivation order is wrong: %v", h.calls)
	}
	if h.index("lvcreate -q -s -n data-probe") < h.index("lvconvert --repair") {
		t.Fatalf("the snapshot probe did not run after the repair: %v", h.calls)
	}
	if h.index("lvremove -f vg-pmcluster/pool_meta0") < h.index("lvcreate -q -s") {
		t.Fatalf("the metadata backup was removed before the repair was verified: %v", h.calls)
	}
}

// A probe that cannot snapshot must say so, not report success.
func TestProbeReportsAFailedSnapshot(t *testing.T) {
	h := &scriptHost{failing: map[string]bool{"lvcreate -q -s": true}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	err := Probe(context.Background(), spec)
	if err == nil {
		t.Fatal("probe reported success on a failed lvcreate")
	}
	if !strings.Contains(err.Error(), "snapshot probe failed") {
		t.Fatalf("error = %v", err)
	}
}

// Probing twice must work: a probe killed mid-flight leaves the name behind.
func TestProbeClearsAStaleProbeFirst(t *testing.T) {
	h := &scriptHost{}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	if err := Probe(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	first := h.index("lvremove -f vg-pmcluster/data-probe")
	create := h.index("lvcreate -q -s -n data-probe")
	if first < 0 || create < 0 || first > create {
		t.Fatalf("a stale probe is not cleared before creating one: %v", h.calls)
	}
	// And the probe's own volume is removed afterwards, exactly once.
	if n := strings.Count(strings.Join(h.calls, "\n"), "lvremove -f vg-pmcluster/data-probe"); n != 2 {
		t.Fatalf("probe volumes removed %d times, want 2 (stale, then its own): %v", n, h.calls)
	}
}

// Rebuilding loses data, so it cannot happen because a flag was copied.
func TestRebuildRequiresAnExplicitDataLossAcknowledgement(t *testing.T) {
	h := &scriptHost{answers: map[string]string{"vgs --noheadings -o vg_name": "  vg-pmcluster\n"}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	if _, err := Repair(context.Background(), spec, RepairOptions{Rebuild: true, Unmount: true}); err == nil {
		t.Fatal("rebuild ran without --allow-data-loss")
	}
	if h.index("vgremove") >= 0 {
		t.Fatalf("vgremove ran anyway: %v", h.calls)
	}
}

// The rebuild reproduces the shape it found, and pins the metadata size so the
// next repair has a reserve to work with.
func TestRebuildReproducesTheSameVolumeShape(t *testing.T) {
	h := &scriptHost{answers: map[string]string{
		"vgs --noheadings -o vg_name":                             "  vg-pmcluster\n",
		"lvs --noheadings --units b -o lv_size vg-pmcluster/pool": "45000687616B\n",
		"lvs --noheadings --units b -o lv_size vg-pmcluster/data": "3439329280B\n",
	}}
	spec := Spec{VG: DefaultVG, LV: DefaultLV, Exec: h.exec}
	res, err := Repair(context.Background(), spec, RepairOptions{Rebuild: true, Unmount: true, AllowDataLoss: true})
	if err != nil {
		t.Fatalf("rebuild: %v (calls: %v)", err, h.calls)
	}
	if !res.Verified || res.Mode != "rebuild" {
		t.Fatalf("result = %+v, want a verified rebuild", res)
	}
	joined := strings.Join(h.calls, "\n")
	for _, want := range []string{
		"vgremove -f vg-pmcluster",
		"lvcreate -q -T vg-pmcluster/pool -L 45000687616B",
		"lvcreate -q -V 3439329280B -T vg-pmcluster/pool -n data",
		"mke2fs -q -t ext4 -L pmcluster-data /dev/vg-pmcluster/data",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rebuild is missing %q:\n%s", want, joined)
		}
	}
}

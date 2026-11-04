package backupagent

import (
	"reflect"
	"testing"
)

const mountTable = `/dev/mapper/vg--pmcluster-data /var/stack/data ext4 rw,relatime 0 0
/dev/sdb1 /var/stack/data/shop/pg_wal ext4 rw,relatime 0 0
10.0.0.5:/exports /var/stack/data/shop/uploads nfs4 rw 0 0
tmpfs /var/stack/data/lms/tmp tmpfs rw 0 0
/dev/sdc1 /var/stack/backup ext4 rw 0 0
proc /proc proc rw 0 0
`

// A snapshot is one filesystem. A stack that mounts a second volume — a WAL
// directory on its own disk is the PostgreSQL-documented case — gets a backup that
// looks verified and cannot be restored.
func TestSplitStacksNamesStacksThatMountAnotherFilesystem(t *testing.T) {
	got := SplitStacks([]byte(mountTable), "/var/stack/data")
	want := []string{"lms", "shop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SplitStacks = %v, want %v", got, want)
	}
}

// The archive directory next to the volume is not a stack, and the volume root
// itself is the snapshot — neither is a split.
func TestSplitStacksIgnoresTheVolumeRootAndSiblings(t *testing.T) {
	if got := SplitStacks([]byte(mountTable), "/var/stack/backup"); len(got) != 0 {
		t.Fatalf("SplitStacks on the archive dir = %v, want none", got)
	}
}

// A stack whose data is wholly inside the volume is the normal case and must stay
// quiet: a warning on every run is a warning nobody reads.
func TestSplitStacksIsSilentWhenEverythingIsInside(t *testing.T) {
	table := `/dev/mapper/vg--pmcluster-data /var/stack/data ext4 rw,relatime 0 0
proc /proc proc rw 0 0
`
	if got := SplitStacks([]byte(table), "/var/stack/data"); len(got) != 0 {
		t.Fatalf("SplitStacks = %v, want none", got)
	}
}

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seed writes a small tree that looks like a stack: a database in a volume
// directory, plus a symlink and a nested directory.
func seedTree(t *testing.T, root string) (files int, bytes int64) {
	t.Helper()
	db := []byte(strings.Repeat("sqlite-page", 512))
	if err := os.MkdirAll(filepath.Join(root, "horizon-erp", "erp-data", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "horizon-erp", "erp-data", "app.db"), db, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "horizon-erp", "erp-data", "nested", "asset.bin"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.db", filepath.Join(root, "horizon-erp", "erp-data", "current")); err != nil {
		t.Fatal(err)
	}
	return 2, int64(len(db)) + 3
}

// A migration must copy everything, prove the copy is complete, and never touch
// the live tree before the copy is verified.
func TestMigrateCopiesVerifiesAndSwaps(t *testing.T) {
	h := &fakeHost{} // plain directory, no VG, no loop yet
	spec := testSpec(t, h)
	wantFiles, wantBytes := seedTree(t, spec.VolumeRoot)
	// Migration needs a stash for the old tree next to the volume root.
	spec.Size = 2 << 30

	res, err := Migrate(context.Background(), spec)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.Copied != wantFiles || res.CopyBytes != wantBytes {
		t.Fatalf("copied %d files / %d bytes, want %d / %d", res.Copied, res.CopyBytes, wantFiles, wantBytes)
	}
	if res.PreviousTree == "" {
		t.Fatal("the previous tree was not preserved")
	}
	// The live tree is still there, complete, under the preserved path.
	prevFiles, prevBytes, err := countTree(res.PreviousTree)
	if err != nil {
		t.Fatal(err)
	}
	if prevFiles != wantFiles || prevBytes != wantBytes {
		t.Fatalf("previous tree has %d files / %d bytes, want %d / %d", prevFiles, prevBytes, wantFiles, wantBytes)
	}
	// The volume ended up mounted (the fake flips this on systemctl start).
	st, err := Detect(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Provisioned() {
		t.Fatalf("after migration the volume is not mounted: %+v", st)
	}
	// The order that matters: build+mount the new tree, copy, unmount it, only
	// then switch. Asserting on call positions rather than on outcomes keeps the
	// test honest about "the live tree is not touched until the copy is done".
	idx := func(prefix string) int {
		for i, c := range h.calls {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}
	mountIdx := idx("mount " + spec.device())
	umountIdx := idx("umount ")
	startIdx := idx("systemctl start")
	if mountIdx < 0 || umountIdx < 0 || startIdx < 0 {
		t.Fatalf("missing mount/umount/systemctl-start: %v", h.calls)
	}
	if !(mountIdx < umountIdx && umountIdx < startIdx) {
		t.Fatalf("order must be mount(%d) → copy → umount(%d) → switch(%d): %v", mountIdx, umountIdx, startIdx, h.calls)
	}
}

func TestMigrateRefusesWhenAContainerIsStillWriting(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	seedTree(t, spec.VolumeRoot)
	// A container with a bind mount under the volume root.
	h.dockerPs = "abc123def456"
	h.dockerMounts = `[{"Type":"bind","Source":"` + spec.VolumeRoot + `/horizon-erp/erp-data"}]`

	if _, err := Migrate(context.Background(), spec); err == nil {
		t.Fatal("migrated while a container was writing into the volume")
	} else if !strings.Contains(err.Error(), "drain the node") {
		t.Fatalf("error does not tell the operator what to do: %v", err)
	}
	// Nothing may have been built.
	if h.ran("truncate") || h.ran("pvcreate") {
		t.Fatalf("built storage before refusing: %v", h.calls)
	}

	// --force is the operator's override.
	spec.Force = true
	if _, err := Migrate(context.Background(), spec); err != nil {
		t.Fatalf("Force did not override the writer check: %v", err)
	}
}

func TestMigrateOnAnEmptyRootIsJustProvisioning(t *testing.T) {
	h := &fakeHost{}
	spec := testSpec(t, h)
	if err := os.MkdirAll(spec.VolumeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Migrate(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.PreviousTree != "" {
		t.Fatalf("an empty root was moved aside: %q", res.PreviousTree)
	}
	if !h.ran("pvcreate") {
		t.Fatalf("nothing was provisioned: %v", h.calls)
	}
}

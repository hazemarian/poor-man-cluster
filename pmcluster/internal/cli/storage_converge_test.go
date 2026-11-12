package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/storage"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// storageStoreWithNodes is a real store with one setting in it: the convergence reads
// the actual intent, not a stub of it.
func storageStoreWithNodes(t *testing.T, nodes string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), nodes); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	return st
}

// storageHarness counts what the convergence decided to do, without touching LVM.
type storageHarness struct {
	state      storage.State
	detectErr  error
	provision  int
	provResult storage.Result
	provErr    error
	out        bytes.Buffer
}

func (h *storageHarness) install(t *testing.T) {
	t.Helper()
	od, op := storageDetectFn, storageProvisionFn
	t.Cleanup(func() { storageDetectFn, storageProvisionFn = od, op })
	storageDetectFn = func(context.Context, storage.Spec) (storage.State, error) { return h.state, h.detectErr }
	storageProvisionFn = func(context.Context, storage.Spec) (storage.Result, error) {
		h.provision++
		return h.provResult, h.provErr
	}
}

func (h *storageHarness) logf(f string, a ...any) {
	h.out.WriteString(strings.TrimSpace(fmt.Sprintf(f, a...)) + "\n")
}

// A storage node with an empty volume root gets provisioned: this is the console's
// toggle doing what the CLI does.
func TestStorageConvergenceProvisionsAStorageNodeWithoutAVolume(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot, Mounted: false, Entries: 0}}
	h.install(t)

	convergeStorage(context.Background(), nil, "nxt-sw-4-m", h.logf)
	if h.provision != 0 {
		t.Fatal("provisioned with no store (cannot know the intent)")
	}

	st := storageStoreWithNodes(t, "nxt-sw-1-m,nxt-sw-4-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if h.provision != 1 {
		t.Fatalf("provisioned %d time(s), want 1 — the UI toggle would still be a lie", h.provision)
	}
	if !strings.Contains(h.out.String(), "provisioning now") {
		t.Fatalf("the journal does not say what it did:\n%s", h.out.String())
	}
}

// A node that is not in storage_nodes is left alone, even without a volume.
func TestStorageConvergenceIgnoresNonStorageNodes(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot}}
	h.install(t)
	st := storageStoreWithNodes(t, "nxt-sw-1-m,nxt-sw-5-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if h.provision != 0 {
		t.Fatal("provisioned a node the setting never named")
	}
}

// An already-provisioned node is a silent no-op: a log line on every start is how an
// operator learns to ignore the log.
func TestStorageConvergenceIsSilentOnAProvisionedNode(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot, Mounted: true, Ours: true, Entries: 42}}
	h.install(t)
	st := storageStoreWithNodes(t, "nxt-sw-4-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if h.provision != 0 {
		t.Fatal("provisioned a node that already has its volume")
	}
	if h.out.Len() != 0 {
		t.Fatalf("a healthy node produced output:\n%s", h.out.String())
	}
}

// A volume root with data is never provisioned: the node may already be serving
// stacks, and shadowing that directory is how a platform appears to lose every file.
func TestStorageConvergenceRefusesAVolumeRootWithData(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot, Entries: 3}}
	h.install(t)
	st := storageStoreWithNodes(t, "nxt-sw-4-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if h.provision != 0 {
		t.Fatal("provisioned over a directory that already holds data")
	}
	if !strings.Contains(h.out.String(), "NOT provisioning") {
		t.Fatalf("the refusal was not reported:\n%s", h.out.String())
	}
}

// A failed provisioning says the node cannot host storage, rather than leaving the
// operator to discover it when a stack comes up without its volume.
func TestStorageConvergenceReportsAFailedProvision(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot}, provErr: errors.New("no space left on device")}
	h.install(t)
	st := storageStoreWithNodes(t, "nxt-sw-4-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if !strings.Contains(h.out.String(), "CANNOT host stateful stacks") {
		t.Fatalf("a failed provision was not reported as a storage problem:\n%s", h.out.String())
	}
}

// storageStoreWithMarkers sets both the storage list and the adoption marker.
func storageStoreWithMarkers(t *testing.T, nodes, adopt string) *store.Store {
	t.Helper()
	st := storageStoreWithNodes(t, nodes)
	if err := st.SetSetting(context.Background(), cluster.SettingStorageAdoptNodes(), adopt); err != nil {
		t.Fatalf("SetSetting(adopt): %v", err)
	}
	return st
}

// adoptHarness records the drain → migrate → activate sequence.
type adoptHarness struct {
	avail      []string
	drainErr   error
	migrated   int
	migrateErr error
}

func (a *adoptHarness) install(t *testing.T) {
	t.Helper()
	oa, om := nodeAvailabilityFn, storageMigrateFn
	t.Cleanup(func() { nodeAvailabilityFn, storageMigrateFn = oa, om })
	nodeAvailabilityFn = func(_ context.Context, node, want string) error {
		a.avail = append(a.avail, want)
		if want == "drain" && a.drainErr != nil {
			return a.drainErr
		}
		return nil
	}
	storageMigrateFn = func(context.Context, storage.Spec) (storage.Result, error) {
		a.migrated++
		return storage.Result{Copied: 7, CopyBytes: 979000, PreviousTree: "/var/stack/data.pre-migration-x"}, a.migrateErr
	}
}

// An adoption marker is what turns "refuse" into "drain, copy, activate" — and the
// node clears the marker afterwards so it never adopts twice.
func TestStorageConvergenceAdoptsOnlyWhenAsked(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot, Entries: 2}}
	h.install(t)
	a := &adoptHarness{}
	a.install(t)
	st := storageStoreWithMarkers(t, "nxt-sw-4-m", "nxt-sw-4-m")

	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)

	if a.migrated != 1 {
		t.Fatalf("migrated %d time(s), want 1", a.migrated)
	}
	if strings.Join(a.avail, ",") != "drain,active" {
		t.Fatalf("availability sequence = %v, want drain then active", a.avail)
	}
	left := st.SettingDefault(context.Background(), cluster.SettingStorageAdoptNodes(), "")
	if left != "" {
		t.Fatalf("adoption marker = %q after a successful adoption, want cleared", left)
	}
	if !strings.Contains(h.out.String(), "ADOPTING") {
		t.Fatalf("the journal does not announce the drain: %s", h.out.String())
	}
}

// A failed drain means nothing was copied and nothing was stopped.
func TestStorageConvergenceRefusesAdoptionWhenItCannotDrain(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot, Entries: 2}}
	h.install(t)
	a := &adoptHarness{drainErr: errors.New("node not found")}
	a.install(t)
	st := storageStoreWithMarkers(t, "nxt-sw-4-m", "nxt-sw-4-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if a.migrated != 0 {
		t.Fatal("copied data under a running node")
	}
	if !strings.Contains(h.out.String(), "refused") {
		t.Fatalf("the refusal was not reported: %s", h.out.String())
	}
}

// A failed migration must still re-activate the node: leaving it drained is worse than
// the failed copy, because nothing else will notice.
func TestStorageConvergenceReactivatesAfterAFailedMigration(t *testing.T) {
	h := &storageHarness{state: storage.State{Root: storage.DefaultVolumeRoot, Entries: 2}}
	h.install(t)
	a := &adoptHarness{migrateErr: errors.New("no space left on device")}
	a.install(t)
	st := storageStoreWithMarkers(t, "nxt-sw-4-m", "nxt-sw-4-m")
	convergeStorage(context.Background(), st, "nxt-sw-4-m", h.logf)
	if strings.Join(a.avail, ",") != "drain,active" {
		t.Fatalf("availability sequence = %v, want drain then active even on failure", a.avail)
	}
	if !strings.Contains(h.out.String(), "FAILED") {
		t.Fatalf("the failure was not reported: %s", h.out.String())
	}
}

package cluster

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// Placement is per-stack, because the stacks differ in whether they own data.
// These tests pin the three rules so a future edit cannot quietly put a
// host-path volume on a node that does not hold it, or pin a stateless service
// to a hostname it has no business following.

func placementFleet() *fakeDocker {
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "nxt-sw-1-m", Role: "manager", IsLeader: true},
		{Hostname: "nxt-sw-2-m", Role: "manager"},
		{Hostname: "nxt-sw-3-w", Role: "worker"},
		{Hostname: "nxt-sw-4-w", Role: "worker"},
	}
	return f
}

func TestBackupPlacementUsesWorkersOnly(t *testing.T) {
	f := placementFleet()
	got := BackupPlacement(context.Background(), nil, f)
	if got != "nxt-sw-3-w" && got != "nxt-sw-4-w" {
		t.Fatalf("backup placed on %q — it must land on a worker, never a manager/storage node", got)
	}
	// Deterministic: the same fleet must always yield the same answer, or the
	// backup volume "moves" between reconciles.
	if again := BackupPlacement(context.Background(), nil, f); again != got {
		t.Fatalf("backup placement is not stable: %q then %q", got, again)
	}
}

func TestBackupPlacementFallsBackWhenNoWorkers(t *testing.T) {
	f := newFakeDocker()
	f.nodes = []runtime.Node{{Hostname: "only-m", Role: "manager", IsLeader: true}}
	if got := BackupPlacement(context.Background(), nil, f); got != "" {
		t.Fatalf("a manager-only fleet has no worker pool; want %q, got %q", "", got)
	}
}

func TestLeaderNodeFollowsTheLeader(t *testing.T) {
	f := placementFleet()
	if got := LeaderNode(context.Background(), f); got != "nxt-sw-1-m" {
		t.Fatalf("leader = %q, want nxt-sw-1-m", got)
	}
	// After an election the console must follow, not stay on the old host.
	for i := range f.nodes {
		f.nodes[i].IsLeader = f.nodes[i].Hostname == "nxt-sw-2-m"
	}
	if got := LeaderNode(context.Background(), f); got != "nxt-sw-2-m" {
		t.Fatalf("after the election leader = %q, want nxt-sw-2-m", got)
	}
}

func TestObservabilityNodeIsTheStorageRoundRobin(t *testing.T) {
	dir := t.TempDir()
	st, err := openTestStore(t, filepath.Join(dir, "placement.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	// No storage nodes yet (a single-node install): nothing to pin to.
	if got := ObservabilityNode(ctx, st); got != "" {
		t.Fatalf("with no storage_nodes the observability stack should be unconstrained, got %q", got)
	}

	if err := st.SetSetting(ctx, SettingStorageNodes(), "nxt-sw-2-m,nxt-sw-1-m,nxt-sw-4-m"); err != nil {
		t.Fatalf("set storage_nodes: %v", err)
	}
	got := ObservabilityNode(ctx, st)
	switch got {
	case "nxt-sw-1-m", "nxt-sw-2-m", "nxt-sw-4-m":
	default:
		t.Fatalf("observability placed on %q, which is not one of the storage nodes", got)
	}
	// The same inputs must resolve to the same node every time; that stability is
	// the only thing keeping Parseable's Parquet store on the node that holds it.
	if again := ObservabilityNode(ctx, st); again != got {
		t.Fatalf("observability placement is not stable: %q then %q", got, again)
	}
	// Node order from the API must not change the answer.
	if err := st.SetSetting(ctx, SettingStorageNodes(), "nxt-sw-4-m,nxt-sw-1-m,nxt-sw-2-m"); err != nil {
		t.Fatalf("reorder storage_nodes: %v", err)
	}
	if again := ObservabilityNode(ctx, st); again != got {
		t.Fatalf("storage_nodes order changed the choice: %q then %q", got, again)
	}
}

// TestObservabilityPlacementIsFrozenAfterFirstResolution is the data-loss guard:
// the round-robin choice must be recorded, so a later storage_nodes change does
// not re-resolve Parseable onto a node that holds none of its Parquet store.
func TestObservabilityPlacementIsFrozenAfterFirstResolution(t *testing.T) {
	dir := t.TempDir()
	st, err := openTestStore(t, filepath.Join(dir, "pin.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if err := st.SetSetting(ctx, SettingStorageNodes(), "nxt-sw-1-m,nxt-sw-2-m,nxt-sw-4-m"); err != nil {
		t.Fatalf("set storage_nodes: %v", err)
	}
	first := ObservabilityNode(ctx, st)
	if first == "" {
		t.Fatal("no placement resolved")
	}
	if pin := st.SettingDefault(ctx, "stack_pin_observability", ""); pin != first {
		t.Fatalf("the resolved node was not recorded (pin=%q, resolved=%q) — it would move on the next render", pin, first)
	}

	// Grow the storage pool: the modulus changes, so an unpinned stack would
	// re-resolve. The recorded pin must win.
	if err := st.SetSetting(ctx, SettingStorageNodes(), "nxt-sw-1-m,nxt-sw-2-m,nxt-sw-4-m,extra-node"); err != nil {
		t.Fatalf("grow storage_nodes: %v", err)
	}
	if again := ObservabilityNode(ctx, st); again != first {
		t.Fatalf("placement moved from %q to %q after a storage_nodes change", first, again)
	}
}

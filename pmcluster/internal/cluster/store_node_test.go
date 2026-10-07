package cluster

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

func newStoreNodeTest(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, context.Background()
}

// TestPickStoreNode_Leader verifies the "leader" setting pins SeaweedFS to the
// node with IsLeader=true, preferring it over any other manager.
func TestPickStoreNode_Leader(t *testing.T) {
	st, ctx := newStoreNodeTest(t)
	if err := st.SetSetting(ctx, SettingBackupStoreOn(), "leader"); err != nil {
		t.Fatal(err)
	}
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "manager-1", Role: "manager", IsLeader: false},
		{Hostname: "manager-2", Role: "manager", IsLeader: true},
		{Hostname: "worker-1", Role: "worker"},
	}
	if got := pickStoreNode(ctx, f, st); got != "manager-2" {
		t.Errorf("leader pick = %q, want manager-2 (the IsLeader node)", got)
	}
}

// TestPickStoreNode_LeaderFallsBackToManager verifies "leader" falls back to
// any manager hostname when no node reports IsLeader.
func TestPickStoreNode_LeaderFallsBackToManager(t *testing.T) {
	st, ctx := newStoreNodeTest(t)
	if err := st.SetSetting(ctx, SettingBackupStoreOn(), "leader"); err != nil {
		t.Fatal(err)
	}
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "manager-1", Role: "manager"},
		{Hostname: "worker-1", Role: "worker"},
	}
	if got := pickStoreNode(ctx, f, st); got != "manager-1" {
		t.Errorf("leader fallback = %q, want manager-1 (any manager)", got)
	}
}

// TestPickStoreNode_WorkerPrefersNonStorageWorker verifies the "worker" (and
// empty) setting keeps the original behavior: first non-storage worker.
func TestPickStoreNode_WorkerPrefersNonStorageWorker(t *testing.T) {
	st, ctx := newStoreNodeTest(t)
	if err := st.SetSetting(ctx, SettingStorageNodes(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "manager-1", Role: "manager"},
		{Hostname: "worker-1", Role: "worker"}, // storage node → excluded
		{Hostname: "worker-2", Role: "worker"},
	}

	for _, setting := range []string{"", "worker"} {
		if err := st.SetSetting(ctx, SettingBackupStoreOn(), setting); err != nil {
			t.Fatal(err)
		}
		if got := pickStoreNode(ctx, f, st); got != "worker-2" {
			t.Errorf("pickStoreNode(backup_store_on=%q) = %q, want worker-2 (first non-storage worker)", setting, got)
		}
	}
}

// TestPickStoreNode_WorkerFallsBackToNonStorageNode verifies the worker path
// falls back to the first non-storage node when there is no non-storage worker.
func TestPickStoreNode_WorkerFallsBackToNonStorageNode(t *testing.T) {
	st, ctx := newStoreNodeTest(t)
	if err := st.SetSetting(ctx, SettingStorageNodes(), "worker-1,manager-1"); err != nil {
		t.Fatal(err)
	}
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "manager-1", Role: "manager"}, // storage → excluded
		{Hostname: "worker-1", Role: "worker"},   // storage → excluded
		{Hostname: "manager-2", Role: "manager"},
	}
	if got := pickStoreNode(ctx, f, st); got != "manager-2" {
		t.Errorf("worker fallback = %q, want manager-2 (first non-storage node)", got)
	}
}

// TestPickStoreNode_LeaderIgnoresStorageNodes verifies "leader" ignores the
// storage_nodes list entirely (an explicit leader choice is allowed as-is).
func TestPickStoreNode_LeaderIgnoresStorageNodes(t *testing.T) {
	st, ctx := newStoreNodeTest(t)
	if err := st.SetSetting(ctx, SettingBackupStoreOn(), "leader"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, SettingStorageNodes(), "manager-1"); err != nil {
		t.Fatal(err)
	}
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "manager-1", Role: "manager", IsLeader: true},
		{Hostname: "worker-1", Role: "worker"},
	}
	if got := pickStoreNode(ctx, f, st); got != "manager-1" {
		t.Errorf("leader pick (with storage_nodes=manager-1) = %q, want manager-1", got)
	}
}

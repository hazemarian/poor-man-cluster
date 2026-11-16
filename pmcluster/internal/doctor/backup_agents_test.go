package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// agentsFixture is a store with three storage nodes and a controllable run history.
type agentsFixture struct {
	store *store.Store
	ctx   context.Context
}

func newAgentsFixture(t *testing.T, hosts ...string) *agentsFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if len(hosts) > 0 {
		if err := st.SetSetting(ctx, cluster.SettingStorageNodes(), strings.Join(hosts, ",")); err != nil {
			t.Fatalf("set storage_nodes: %v", err)
		}
	}
	return &agentsFixture{store: st, ctx: ctx}
}

// recordRun writes a finished backup row whose artifact names the node that took it.
func (f *agentsFixture) recordRun(t *testing.T, nodeID string, startedAt time.Time) {
	t.Helper()
	id, err := f.store.CreateBackup(f.ctx, "shop", 0)
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	archive := "/var/stack/backup/backup-" + nodeID + "-2026-11-16T12-00-00-stack-shop.tar.gz"
	if err := f.store.FinishBackup(f.ctx, id, "succeeded", archive, ""); err != nil {
		t.Fatalf("FinishBackup: %v", err)
	}
	_ = startedAt // the row is always "now"; tests age it by moving the CLOCK (Input.Now)
}

func agentsDocker() runtime.Client {
	return &nodeListOnly{nodes: []runtime.Node{
		{Hostname: "nxt-sw-1-m", ID: "aaa", Role: "manager"},
		{Hostname: "nxt-sw-4-m", ID: "bbb", Role: "manager"},
		{Hostname: "nxt-sw-5-m", ID: "ccc", Role: "manager"},
	}}
}

// nodeListOnly satisfies the swarm reads these checks use, and nothing else.
type nodeListOnly struct {
	runtime.Client
	nodes    []runtime.Node
	services []runtime.Service
}

func (n *nodeListOnly) NodeList(context.Context) ([]runtime.Node, error) { return n.nodes, nil }

func (n *nodeListOnly) ServiceList(context.Context) ([]runtime.Service, error) {
	return n.services, nil
}

// TestBackupAgents_SilentStorageNodeFails is the production finding: three storage nodes, two
// reporting runs, one never having reported anything — its data is on the volume and nothing
// archives it, while coverage reports ✓ because placement is fine.
func TestBackupAgents_SilentStorageNodeFails(t *testing.T) {
	f := newAgentsFixture(t, "nxt-sw-1-m", "nxt-sw-4-m", "nxt-sw-5-m")
	f.recordRun(t, "aaa", time.Time{})
	f.recordRun(t, "ccc", time.Time{})

	got := checkBackupAgents(f.ctx, Input{Store: f.store, Docker: agentsDocker(), Now: time.Now()})
	if got.Status != StatusFail {
		t.Fatalf("status = %v, want fail (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "nxt-sw-4-m") {
		t.Errorf("the silent node is not named: %s", got.Detail)
	}
	if strings.Contains(got.Detail, "nxt-sw-1-m") || strings.Contains(got.Detail, "nxt-sw-5-m") {
		t.Errorf("a node that did report is listed as silent: %s", got.Detail)
	}
	if !strings.Contains(got.Detail, "backup-agent install") {
		t.Errorf("the finding does not say how to fix it: %s", got.Detail)
	}
}

func TestBackupAgents_AllNodesReportingIsOK(t *testing.T) {
	f := newAgentsFixture(t, "nxt-sw-1-m", "nxt-sw-4-m")
	f.recordRun(t, "aaa", time.Time{})
	f.recordRun(t, "bbb", time.Time{})

	got := checkBackupAgents(f.ctx, Input{Store: f.store, Docker: agentsDocker(), Now: time.Now()})
	if got.Status != StatusOK {
		t.Fatalf("status = %v, want ok (%s)", got.Status, got.Detail)
	}
}

// TestBackupAgents_AnOldRunDoesNotCount keeps the check about "never", inside a window: a node
// whose only run is older than the window is a node that has stopped, which is worth a failure
// too — but a row from six days ago must not be mistaken for silence.
func TestBackupAgents_AnOldRunDoesNotCount(t *testing.T) {
	f := newAgentsFixture(t, "nxt-sw-4-m")
	f.recordRun(t, "bbb", time.Time{})

	// Eight days later the same row is outside the window: the node has stopped.
	late := time.Now().Add(8 * 24 * time.Hour)
	if got := checkBackupAgents(f.ctx, Input{Store: f.store, Docker: agentsDocker(), Now: late}); got.Status != StatusFail {
		t.Fatalf("a run outside the window read as ok: %v", got.Detail)
	}
	// Six days later it is still inside it.
	soon := time.Now().Add(6 * 24 * time.Hour)
	if got := checkBackupAgents(f.ctx, Input{Store: f.store, Docker: agentsDocker(), Now: soon}); got.Status != StatusOK {
		t.Fatalf("a run inside the window did not count: %s", got.Detail)
	}
}

func TestBackupAgents_NoDesignationSkips(t *testing.T) {
	f := newAgentsFixture(t) // no storage_nodes
	got := checkBackupAgents(f.ctx, Input{Store: f.store, Docker: agentsDocker()})
	if got.Status != StatusSkip {
		t.Fatalf("status = %v, want skip when nothing is designated", got.Status)
	}
}

// TestNodeFromArchivePaths pins the parse the check depends on: the node id sits between
// "backup-" and the timestamp.
func TestNodeFromArchivePaths(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/var/stack/backup/backup-b0nkxwnkm952bqwg6bi7i8m8q-2026-11-16T12-24-32-stack-adam-world-dev.tar.gz", "b0nkxwnkm952bqwg6bi7i8m8q"},
		{"/a/x.tar.gz,/b/backup-n9xllyzn-2026-01-01T00-00-00-stack-shop.tar.gz", "n9xllyzn"},
		{"", ""},
		{"/var/stack/backup/no-node-prefix.tar.gz", ""},
	}
	for _, c := range cases {
		if got := nodeFromArchivePaths(c.in); got != c.want {
			t.Errorf("nodeFromArchivePaths(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

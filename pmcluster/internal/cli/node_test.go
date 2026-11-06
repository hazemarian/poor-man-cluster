package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fakeNodeClient is an in-memory runtime.Client used to stub node promote/
// demote. It embeds the real interface so only NodeList + SetNodeLabel need
// overriding.
type fakeNodeClient struct {
	runtime.Client
	nodes      []runtime.Node
	labelCalls []string // "<id>:<key>:<value>"
	labelErr   error
}

func (f *fakeNodeClient) Close() error { return nil }

func (f *fakeNodeClient) NodeList(context.Context) ([]runtime.Node, error) { return f.nodes, nil }

func (f *fakeNodeClient) SetNodeLabel(_ context.Context, id, key, value string) error {
	f.labelCalls = append(f.labelCalls, id+":"+key+":"+value)
	return f.labelErr
}

// tc7NodeFixtures returns the two-node swarm used across the tests.
func tc7NodeFixtures() []runtime.Node {
	return []runtime.Node{
		{ID: "node1abc", Hostname: "nxt-sw-1-m", Role: "manager", Status: "ready", Availability: "active", IsLeader: true, EngineVersion: "29.8.2"},
		{ID: "node2xyz", Hostname: "nxt-sw-2-m", Role: "manager", Status: "ready", Availability: "active", IsLeader: false, EngineVersion: "29.8.2"},
	}
}

// installFakeNodeClient swaps dockerNewFn for the fake and returns the fake +
// a restore func. Requires a fresh CLI env (newTestCLIEnv) to be active so
// openStore resolves.
func installFakeNodeClient(t *testing.T, nodes []runtime.Node, labelErr error) *fakeNodeClient {
	t.Helper()
	orig := dockerNewFn
	fc := &fakeNodeClient{nodes: nodes, labelErr: labelErr}
	dockerNewFn = func() (runtime.Client, error) { return fc, nil }
	t.Cleanup(func() { dockerNewFn = orig })
	return fc
}

// newNodeTestCmd returns a cobra command wired to the given RunE with a
// background context + output buffers (mirrors newTestCmd without flags).
func newNodeTestCmd(runE func(cmd *cobra.Command, args []string) error) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{Use: "test", RunE: runE}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}

func TestRunNodePromote_AppendsToStorageNodes(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	fc := installFakeNodeClient(t, tc7NodeFixtures(), nil)

	// Leader is already a storage node (cluster-up default); promoting the other
	// MANAGER appends it. (A worker is refused — see TestNodePromoteRefusesAWorker.)
	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "nxt-sw-1-m"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = st.Close()

	cmd, out := newNodeTestCmd(runNodePromote)
	if err := cmd.RunE(cmd, []string{"nxt-sw-2-m"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !strings.Contains(out.String(), "✓ nxt-sw-2-m is now a storage node (storage_nodes=nxt-sw-1-m,nxt-sw-2-m)") {
		t.Fatalf("promote output = %q", out.String())
	}
	if len(fc.labelCalls) != 1 || fc.labelCalls[0] != "node2xyz:pmcluster.storage:true" {
		t.Fatalf("labelCalls = %v", fc.labelCalls)
	}

	st2, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if got := st2.SettingDefault(cmd.Context(), cluster.SettingStorageNodes(), ""); got != "nxt-sw-1-m,nxt-sw-2-m" {
		t.Fatalf("storage_nodes = %q", got)
	}
}

func TestRunNodePromote_AlreadyStorageNode(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	fc := installFakeNodeClient(t, tc7NodeFixtures(), nil)

	// Seed storage_nodes with both nodes already present.
	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "nxt-sw-1-m,nxt-sw-2-m"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = st.Close()

	cmd, out := newNodeTestCmd(runNodePromote)
	if err := cmd.RunE(cmd, []string{"nxt-sw-2-m"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !strings.Contains(out.String(), "nxt-sw-2-m is already a storage node (storage_nodes=nxt-sw-1-m,nxt-sw-2-m)") {
		t.Fatalf("promote output = %q", out.String())
	}
	// Idempotent promote still re-stamps the label exactly once.
	if len(fc.labelCalls) != 1 || fc.labelCalls[0] != "node2xyz:pmcluster.storage:true" {
		t.Fatalf("labelCalls = %v", fc.labelCalls)
	}
}

func TestRunNodePromote_UnknownNode(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	fc := installFakeNodeClient(t, tc7NodeFixtures(), nil)

	cmd, _ := newNodeTestCmd(runNodePromote)
	err := cmd.RunE(cmd, []string{"ghost-node"})
	if err == nil || !strings.Contains(err.Error(), `ghost-node" not found in swarm`) {
		t.Fatalf("err = %v", err)
	}
	if len(fc.labelCalls) != 0 {
		t.Fatalf("labelCalls = %v (should be empty)", fc.labelCalls)
	}
}

func TestRunNodePromote_LabelErrorWarns(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	installFakeNodeClient(t, tc7NodeFixtures(), context.DeadlineExceeded)

	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "nxt-sw-1-m"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = st.Close()

	cmd, out := newNodeTestCmd(runNodePromote)
	if err := cmd.RunE(cmd, []string{"nxt-sw-2-m"}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if !strings.Contains(out.String(), "⚠ updated storage_nodes=nxt-sw-1-m,nxt-sw-2-m but the pmcluster.storage label could not be stamped") {
		t.Fatalf("promote output = %q", out.String())
	}
	// Setting was still persisted despite the label failure.
	st, _, err = openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if got := st.SettingDefault(context.Background(), cluster.SettingStorageNodes(), ""); got != "nxt-sw-1-m,nxt-sw-2-m" {
		t.Fatalf("storage_nodes = %q", got)
	}
}

func TestRunNodeDemote_RemovesFromStorageNodes(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	// Two managers in the fixture: leaving one is refused by the "<3" guard unless
	// the operator accepts it (the guard has its own test).
	prevForce := nodeDemoteForce
	nodeDemoteForce = true
	t.Cleanup(func() { nodeDemoteForce = prevForce })
	fc := installFakeNodeClient(t, tc7NodeFixtures(), nil)

	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "nxt-sw-1-m,nxt-sw-2-m"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = st.Close()

	cmd, out := newNodeTestCmd(runNodeDemote)
	if err := cmd.RunE(cmd, []string{"nxt-sw-2-m"}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if !strings.Contains(out.String(), "✓ nxt-sw-2-m is no longer a storage node (storage_nodes=nxt-sw-1-m)") {
		t.Fatalf("demote output = %q", out.String())
	}
	if len(fc.labelCalls) != 1 || fc.labelCalls[0] != "node2xyz:pmcluster.storage:" {
		t.Fatalf("labelCalls = %v", fc.labelCalls)
	}

	st2, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if got := st2.SettingDefault(context.Background(), cluster.SettingStorageNodes(), ""); got != "nxt-sw-1-m" {
		t.Fatalf("storage_nodes = %q", got)
	}
}

func TestRunNodeDemote_NotInStorageNodes(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	// Two managers in the fixture: leaving one trips the "<3" guard, which
	// has its own test — here the removal mechanics are what is under test.
	prevForce := nodeDemoteForce
	nodeDemoteForce = true
	t.Cleanup(func() { nodeDemoteForce = prevForce })
	fc := installFakeNodeClient(t, tc7NodeFixtures(), nil)

	// Seed storage_nodes WITHOUT the demoted host.
	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "nxt-sw-1-m"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = st.Close()

	cmd, out := newNodeTestCmd(runNodeDemote)
	if err := cmd.RunE(cmd, []string{"nxt-sw-2-m"}); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if !strings.Contains(out.String(), "nxt-sw-2-m was not in storage_nodes (label cleared anyway)") {
		t.Fatalf("demote output = %q", out.String())
	}
	// Label still cleared even though the setting had no entry.
	if len(fc.labelCalls) != 1 || fc.labelCalls[0] != "node2xyz:pmcluster.storage:" {
		t.Fatalf("labelCalls = %v", fc.labelCalls)
	}
}

// TestRunNodeDemote_RefusesLeader verifies the BUG-024 policy: the leader must
// stay a storage node, so demoting it is refused with an actionable error.
func TestRunNodeDemote_RefusesLeader(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	// Two managers in the fixture: leaving one trips the "<3" guard, which
	// has its own test — here the removal mechanics are what is under test.
	prevForce := nodeDemoteForce
	nodeDemoteForce = true
	t.Cleanup(func() { nodeDemoteForce = prevForce })
	installFakeNodeClient(t, tc7NodeFixtures(), nil)

	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "nxt-sw-1-m,nxt-sw-2-m"); err != nil {
		t.Fatalf("seed storage_nodes: %v", err)
	}
	cmd, out := newNodeTestCmd(runNodeDemote)
	err = cmd.RunE(cmd, []string{"nxt-sw-1-m"})
	if err == nil {
		t.Fatal("demoting the leader must fail")
	} else if !strings.Contains(err.Error(), "refusing to demote") {
		t.Fatalf("unexpected error: %v", err)
	}
	st2, _, err := openStore()
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got := st2.SettingDefault(context.Background(), cluster.SettingStorageNodes(), "")
	_ = st2.Close()
	if !strings.Contains(got, "nxt-sw-1-m") {
		t.Fatalf("storage_nodes must be unchanged, got %q", got)
	}
	_ = out
}

// Storage nodes are managers. `node promote` on a worker must refuse and say how
// to do it properly, rather than putting stateful data (and its backups) on a node
// that cannot read the control plane.
func TestNodePromoteRefusesAWorker(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	prevNew := dockerNewFn
	t.Cleanup(func() { dockerNewFn = prevNew })
	f := &fakeNodeClient{nodes: []runtime.Node{
		{Hostname: "worker1", ID: "n2", Role: "worker", Status: "ready"},
		{Hostname: "mgr1", ID: "n1", Role: "manager", Status: "ready", IsLeader: true},
	}}
	dockerNewFn = func() (runtime.Client, error) { return f, nil }

	cmd, out := newNodeTestCmd(runNodePromote)
	err := cmd.RunE(cmd, []string{"worker1"})
	if err == nil {
		t.Fatal("promoted a worker to a storage node")
	}
	for _, want := range []string{"MANAGERS", "docker node promote worker1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
	if len(f.labelCalls) != 0 {
		t.Errorf("a refused promote must not touch the swarm: %v", f.labelCalls)
	}
	_ = out
}

// Three managers is the smallest count that survives a loss. Demoting below it is
// refused unless the operator says --force, and a forced demote still says what
// they just gave up.
func TestNodeDemoteRefusesBelowThreeManagersUnlessForced(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
	prevNew := dockerNewFn
	prevForce := nodeDemoteForce
	t.Cleanup(func() { dockerNewFn = prevNew; nodeDemoteForce = prevForce })
	f := &fakeNodeClient{nodes: []runtime.Node{
		{Hostname: "mgr1", ID: "n1", Role: "manager", Status: "ready", IsLeader: true},
		{Hostname: "mgr2", ID: "n2", Role: "manager", Status: "ready"},
	}}
	dockerNewFn = func() (runtime.Client, error) { return f, nil }

	st, _, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if serr := st.SetSetting(context.Background(), cluster.SettingStorageNodes(), "mgr1,mgr2"); serr != nil {
		t.Fatalf("seed: %v", serr)
	}
	_ = st.Close()

	cmd, out := newNodeTestCmd(runNodeDemote)

	nodeDemoteForce = false
	err = cmd.RunE(cmd, []string{"mgr2"})
	if err == nil {
		t.Fatal("demoted below three managers without --force")
	}
	if !strings.Contains(err.Error(), "three") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal should name the rule and the escape hatch: %v", err)
	}

	nodeDemoteForce = true
	out.Reset()
	if err := cmd.RunE(cmd, []string{"mgr2"}); err != nil {
		t.Fatalf("--force should go through: %v", err)
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Errorf("a forced demote must still warn about the loss:\n%s", out.String())
	}
}

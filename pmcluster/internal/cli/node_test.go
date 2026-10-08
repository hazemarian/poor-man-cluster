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
		{ID: "node2xyz", Hostname: "nxt-sw-2-m", Role: "worker", Status: "ready", Availability: "active", IsLeader: false, EngineVersion: "29.8.2"},
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

	// Leader is already a storage node (cluster-up default); promoting the
	// worker appends it.
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
	if got := st2.GetSettingDefault(cmd.Context(), cluster.SettingStorageNodes(), ""); got != "nxt-sw-1-m,nxt-sw-2-m" {
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
	if got := st.GetSettingDefault(context.Background(), cluster.SettingStorageNodes(), ""); got != "nxt-sw-1-m,nxt-sw-2-m" {
		t.Fatalf("storage_nodes = %q", got)
	}
}

func TestRunNodeDemote_RemovesFromStorageNodes(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
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
	if got := st2.GetSettingDefault(context.Background(), cluster.SettingStorageNodes(), ""); got != "nxt-sw-1-m" {
		t.Fatalf("storage_nodes = %q", got)
	}
}

func TestRunNodeDemote_NotInStorageNodes(t *testing.T) {
	_, restore := newTestCLIEnv(t)
	defer restore()
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
	got := st2.GetSettingDefault(context.Background(), cluster.SettingStorageNodes(), "")
	_ = st2.Close()
	if !strings.Contains(got, "nxt-sw-1-m") {
		t.Fatalf("storage_nodes must be unchanged, got %q", got)
	}
	_ = out
}

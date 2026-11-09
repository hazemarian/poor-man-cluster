package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// installLeaderMoveClient wires the fake swarm into openDocker and keeps the
// election instant.
func installLeaderMoveClient(t *testing.T, nodes []runtime.Node, electedBy string) *fakeNodeClient {
	t.Helper()
	orig := dockerNewFn
	fc := &fakeNodeClient{nodes: nodes, electedBy: electedBy}
	dockerNewFn = func() (runtime.Client, error) { return fc, nil }
	t.Cleanup(func() { dockerNewFn = orig })

	prevOpts := clusterLeaderMoveOptions
	clusterLeaderMoveOptions = cluster.LeaderMoveOptions{
		PollInterval:  time.Millisecond,
		SettleTimeout: 200 * time.Millisecond,
		ElectionGrace: time.Millisecond,
		MaxElections:  3,
		Budget:        2 * time.Second,
	}
	t.Cleanup(func() { clusterLeaderMoveOptions = prevOpts })

	prevURL := apiURL
	apiURL = ""
	t.Cleanup(func() { apiURL = prevURL })
	return fc
}

func leaderMoveNodes() []runtime.Node {
	return []runtime.Node{
		{ID: "n4", Hostname: "nxt-sw-4-m", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
		{ID: "n5", Hostname: "nxt-sw-5-m", Role: "manager", Status: "ready", Availability: "active"},
		{ID: "n6", Hostname: "nxt-sw-6-m", Role: "manager", Status: "ready", Availability: "active"},
		{ID: "n7", Hostname: "nxt-sw-7-m", Role: "manager", Status: "ready", Availability: "active"},
	}
}

// On success the CLI reports where leadership actually went — the operator uses
// that to know which daemon is now serving.
func TestRunClusterLeaderReportsTheNewLeader(t *testing.T) {
	fc := installLeaderMoveClient(t, leaderMoveNodes(), "nxt-sw-5-m")

	cmd, out := newNodeTestCmd(runClusterLeader)
	if err := cmd.RunE(cmd, []string{"nxt-sw-5-m"}); err != nil {
		t.Fatalf("cluster leader: %v", err)
	}
	if !strings.Contains(out.String(), "✓ leadership moved to nxt-sw-5-m") {
		t.Fatalf("output must name the new leader, got %q", out.String())
	}
	if !strings.Contains(out.String(), "1 forced election") {
		t.Fatalf("output must say how many elections it took, got %q", out.String())
	}
	if !strings.Contains(out.String(), "storage_nodes was not touched") {
		t.Fatalf("output must state that storage membership did not move, got %q", out.String())
	}
	if got := strings.Join(fc.roleCalls, ","); got != "n4:worker,n4:manager" {
		t.Fatalf("role calls = %v, want the old leader demoted and restored", fc.roleCalls)
	}
}

// A worker target is refused before any role is touched.
func TestRunClusterLeaderRefusesAWorker(t *testing.T) {
	nodes := append(leaderMoveNodes(), runtime.Node{
		ID: "n3", Hostname: "nxt-sw-3-w", Role: "worker", Status: "ready", Availability: "active",
	})
	fc := installLeaderMoveClient(t, nodes, "")

	cmd, _ := newNodeTestCmd(runClusterLeader)
	err := cmd.RunE(cmd, []string{"nxt-sw-3-w"})
	if err == nil || !strings.Contains(err.Error(), "only a manager can win") {
		t.Fatalf("expected a worker refusal, got %v", err)
	}
	if len(fc.roleCalls) != 0 {
		t.Fatalf("a refusal must not touch any role, calls = %v", fc.roleCalls)
	}
}

// Below three managers, forcing the election would leave the swarm unable to
// survive another loss.
func TestRunClusterLeaderRefusesBelowThreeManagers(t *testing.T) {
	nodes := []runtime.Node{
		{ID: "n1", Hostname: "nxt-sw-1-m", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
		{ID: "n2", Hostname: "nxt-sw-2-m", Role: "manager", Status: "ready", Availability: "active"},
	}
	fc := installLeaderMoveClient(t, nodes, "")

	cmd, _ := newNodeTestCmd(runClusterLeader)
	err := cmd.RunE(cmd, []string{"nxt-sw-2-m"})
	if err == nil || !strings.Contains(err.Error(), "leaving 1 manager(s)") {
		t.Fatalf("expected a below-three refusal, got %v", err)
	}
	if len(fc.roleCalls) != 0 {
		t.Fatalf("a refusal must not touch any role, calls = %v", fc.roleCalls)
	}
}

// Already leading: say so instead of forcing a pointless election.
func TestRunClusterLeaderNoOpWhenAlreadyLeading(t *testing.T) {
	fc := installLeaderMoveClient(t, leaderMoveNodes(), "")

	cmd, out := newNodeTestCmd(runClusterLeader)
	if err := cmd.RunE(cmd, []string{"nxt-sw-4-m"}); err != nil {
		t.Fatalf("cluster leader: %v", err)
	}
	if !strings.Contains(out.String(), "already the swarm leader") {
		t.Fatalf("output = %q, want an already-leader note", out.String())
	}
	if len(fc.roleCalls) != 0 {
		t.Fatalf("no election should be forced, calls = %v", fc.roleCalls)
	}
}

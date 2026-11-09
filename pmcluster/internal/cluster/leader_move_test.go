package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// fastMove keeps the forced-election loop instant in tests while leaving the
// production defaults alone.
func fastMove() LeaderMoveOptions {
	return LeaderMoveOptions{
		PollInterval:  time.Millisecond,
		SettleTimeout: 200 * time.Millisecond,
		ElectionGrace: time.Millisecond,
		MaxElections:  3,
		Budget:        2 * time.Second,
	}
}

// electOnDemote simulates the swarm: a demoted leader becomes a worker, and the
// first remaining ready manager is elected in its place. `winner` pins the
// election to one hostname so a test is deterministic.
func electOnDemote(winner string) func(*fakeDocker, string, string) {
	return func(f *fakeDocker, nodeID, role string) {
		for i := range f.nodes {
			if f.nodes[i].ID != nodeID {
				continue
			}
			f.nodes[i].Role = role
			if role != "manager" {
				f.nodes[i].IsLeader = false
			}
		}
		if role != "worker" {
			return
		}
		for i := range f.nodes {
			if f.nodes[i].IsLeader {
				continue
			}
			if f.nodes[i].Hostname == winner || (winner == "" && f.nodes[i].Role == "manager") {
				if f.nodes[i].Role == "manager" {
					f.nodes[i].IsLeader = true
					return
				}
			}
		}
	}
}

func managersFixture() []runtime.Node {
	return []runtime.Node{
		{Hostname: "nxt-sw-4-m", ID: "n4", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
		{Hostname: "nxt-sw-5-m", ID: "n5", Role: "manager", Status: "ready", Availability: "active"},
		{Hostname: "nxt-sw-6-m", ID: "n6", Role: "manager", Status: "ready", Availability: "active"},
		{Hostname: "nxt-sw-7-m", ID: "n7", Role: "manager", Status: "ready", Availability: "active"},
	}
}

// A worker cannot win the Raft election, so the move is refused before any role
// is touched — with the way out in the message.
func TestMoveLeadershipRefusesAWorkerTarget(t *testing.T) {
	f := newFakeDocker()
	f.nodes = append(managersFixture(), runtime.Node{
		Hostname: "nxt-sw-3-w", ID: "n3", Role: "worker", Status: "ready", Availability: "active",
	})
	_, err := MoveLeadership(context.Background(), f, "nxt-sw-3-w", fastMove())
	if err == nil {
		t.Fatal("expected a refusal for a worker target")
	}
	var refusal *LeaderMoveRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error should be a refusal (409), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "only a manager can win") || !strings.Contains(err.Error(), "docker node promote") {
		t.Fatalf("refusal must name the rule and the way out, got: %v", err)
	}
	if len(f.nodeRoles) != 0 {
		t.Fatalf("a refusal must not touch any role, calls = %v", f.nodeRoles)
	}
}

// Forcing an election demotes the leader for the duration. With three managers
// that leaves two, which cannot survive another loss — the same floor
// `node demote` refuses on.
func TestMoveLeadershipRefusesBelowThreeManagers(t *testing.T) {
	f := newFakeDocker()
	f.nodes = []runtime.Node{
		{Hostname: "a-m", ID: "a", Role: "manager", Status: "ready", Availability: "active", IsLeader: true},
		{Hostname: "b-m", ID: "b", Role: "manager", Status: "ready", Availability: "active"},
		{Hostname: "c-m", ID: "c", Role: "manager", Status: "ready", Availability: "active"},
	}
	_, err := MoveLeadership(context.Background(), f, "b-m", fastMove())
	if err == nil || !strings.Contains(err.Error(), "leaving 2 manager(s)") {
		t.Fatalf("expected a below-three refusal, got %v", err)
	}
	var refusal *LeaderMoveRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error should be a refusal, got %T", err)
	}
	if len(f.nodeRoles) != 0 {
		t.Fatalf("a refusal must not touch any role, calls = %v", f.nodeRoles)
	}
}

// No quorum: the membership change could not be committed, so say that instead
// of hanging until the API gives up.
func TestMoveLeadershipRefusesWithoutQuorum(t *testing.T) {
	f := newFakeDocker()
	f.nodes = managersFixture()
	// Four managers, one reachable: required is three.
	for i := 1; i < len(f.nodes); i++ {
		f.nodes[i].Status = "down"
	}
	_, err := MoveLeadership(context.Background(), f, "nxt-sw-5-m", fastMove())
	if err == nil || !strings.Contains(err.Error(), "no quorum") {
		t.Fatalf("expected a no-quorum refusal, got %v", err)
	}
	if len(f.nodeRoles) != 0 {
		t.Fatalf("a refusal must not touch any role, calls = %v", f.nodeRoles)
	}
}

// The happy path: the leader is demoted, the target wins the election, and the
// old leader is promoted back to a plain manager. The result reports the new
// leader — the thing the caller actually needs to know.
func TestMoveLeadershipMovesAndReportsTheNewLeader(t *testing.T) {
	f := newFakeDocker()
	f.nodes = managersFixture()
	f.onSetNodeRole = electOnDemote("nxt-sw-5-m")

	res, err := MoveLeadership(context.Background(), f, "nxt-sw-5-m", fastMove())
	if err != nil {
		t.Fatalf("MoveLeadership: %v", err)
	}
	if res.Leader != "nxt-sw-5-m" || res.From != "nxt-sw-4-m" || res.Elections != 1 || !res.Changed() {
		t.Fatalf("result = %+v, want leader nxt-sw-5-m from nxt-sw-4-m in 1 election", res)
	}
	want := []string{"n4:worker", "n4:manager"}
	if strings.Join(f.nodeRoles, ",") != strings.Join(want, ",") {
		t.Fatalf("role changes = %v, want %v (the old leader must come back as a manager)", f.nodeRoles, want)
	}
}

// Already there: no role is touched, and the answer says so rather than
// performing a pointless election.
func TestMoveLeadershipNoOpWhenTargetAlreadyLeads(t *testing.T) {
	f := newFakeDocker()
	f.nodes = managersFixture()
	res, err := MoveLeadership(context.Background(), f, "nxt-sw-4-m", fastMove())
	if err != nil {
		t.Fatalf("MoveLeadership: %v", err)
	}
	if res.Changed() || res.Leader != "nxt-sw-4-m" {
		t.Fatalf("result = %+v, want an unchanged no-op on nxt-sw-4-m", res)
	}
	if len(f.nodeRoles) != 0 {
		t.Fatalf("no election should be forced, calls = %v", f.nodeRoles)
	}
}

// The swarm elects its own leader. When the target does not win, the command
// must restore the demoted manager and report where leadership actually is —
// never claim success.
func TestMoveLeadershipReportsWhenTheElectionLandsElsewhere(t *testing.T) {
	f := newFakeDocker()
	f.nodes = managersFixture()
	f.onSetNodeRole = electOnDemote("nxt-sw-6-m")

	opts := fastMove()
	opts.MaxElections = 1
	res, err := MoveLeadership(context.Background(), f, "nxt-sw-5-m", opts)
	if err == nil {
		t.Fatal("expected an error when leadership landed on another node")
	}
	if res.Leader != "nxt-sw-6-m" {
		t.Fatalf("result.Leader = %q, want the actual leader nxt-sw-6-m", res.Leader)
	}
	if !strings.Contains(err.Error(), "nxt-sw-6-m") {
		t.Fatalf("error must name the node that won, got: %v", err)
	}
	// The demoted leader was restored even though the move failed.
	if got := strings.Join(f.nodeRoles, ","); got != "n4:worker,n4:manager" {
		t.Fatalf("role changes = %v, want the leader restored to manager", f.nodeRoles)
	}
}

func TestMoveLeadershipRefusesAnUnknownNode(t *testing.T) {
	f := newFakeDocker()
	f.nodes = managersFixture()
	_, err := MoveLeadership(context.Background(), f, "nope", fastMove())
	if err == nil || !strings.Contains(err.Error(), "not in this swarm") {
		t.Fatalf("expected an unknown-node refusal, got %v", err)
	}
}

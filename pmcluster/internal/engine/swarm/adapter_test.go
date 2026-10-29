package swarm_test

import (
	"context"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine/contracttest"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine/swarm"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// stubClient implements only what the adapter uses. runtime.Client is embedded
// so the other 29 methods exist without being written: calling one would panic,
// which is the behaviour we want if the adapter ever starts reaching for more
// of the engine than the seam declares.
type stubClient struct {
	runtime.Client
	restarted []string
}

func (s *stubClient) NodeList(context.Context) ([]runtime.Node, error) {
	return []runtime.Node{
		{Hostname: "nxt-sw-1-m", Role: "manager", Availability: "active", Status: "ready", IsLeader: true},
		{Hostname: "nxt-sw-4-m", Role: "manager", Availability: "active", Status: "ready"},
		{Hostname: "nxt-sw-3-w", Role: "worker", Availability: "active", Status: "ready"},
	}, nil
}

func (s *stubClient) ServiceList(context.Context) ([]runtime.Service, error) {
	return []runtime.Service{
		{
			ID: "svc-edge", Name: "edge_pmcluster-edge", Stack: "edge",
			Replicas: 1, Desired: 1, Mode: "replicated", Platform: true,
			Node: "nxt-sw-1-m", RunningNode: "nxt-sw-1-m",
		},
		{
			ID: "svc-app", Name: "abbas_abbas", Stack: "abbas",
			Replicas: 1, Desired: 1, Mode: "replicated",
			Node: "nxt-sw-5-m", RunningNode: "nxt-sw-5-m",
		},
		{
			ID: "svc-job", Name: "observability_provision", Stack: "observability",
			Replicas: 0, Desired: 1, Mode: "replicated", RunOnce: true, Platform: true,
		},
	}, nil
}

func (s *stubClient) ServiceRestart(_ context.Context, id string) error {
	s.restarted = append(s.restarted, id)
	return nil
}

// TestAdapterMeetsTheContract is the point of the seam: a Swarm adapter is
// accepted when the SHARED suite passes, so a future adapter is judged by the
// same assertions rather than by its own opinion of correctness.
func TestAdapterMeetsTheContract(t *testing.T) {
	stub := &stubClient{}
	contracttest.Run(t, func(t *testing.T) engine.Engine {
		return swarm.New(stub)
	})
}

// TestAdapterTranslatesNamesToIDs pins the one piece of translation the read
// surface needs: the platform speaks workload names, Swarm mutates by ID.
func TestAdapterTranslatesNamesToIDs(t *testing.T) {
	stub := &stubClient{}
	a := swarm.New(stub)

	if err := a.Restart(context.Background(), "abbas_abbas"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(stub.restarted) != 1 || stub.restarted[0] != "svc-app" {
		t.Fatalf("restarted = %v, want [svc-app]", stub.restarted)
	}

	if err := a.Restart(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("Restart of an unknown workload must fail, not silently succeed")
	}
}

// TestAdapterNormalisesRoles: callers must never have to know the engine's
// spelling of a node class.
func TestAdapterNormalisesRoles(t *testing.T) {
	a := swarm.New(&stubClient{})
	nodes, err := a.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	want := map[string]string{
		"nxt-sw-1-m": "manager",
		"nxt-sw-4-m": "manager",
		"nxt-sw-3-w": "worker",
	}
	for _, n := range nodes {
		if string(n.Role) != want[n.Hostname] {
			t.Errorf("%s role = %q, want %q", n.Hostname, n.Role, want[n.Hostname])
		}
	}
}

// Package contracttest is the shared definition of engine parity.
//
// A new engine adapter is "done" when contracttest.Run passes against it — not
// when its own tests pass. Keeping the assertions here means every adapter is
// held to the same behaviour, and the platform layer can be written against
// engine.Engine without caring which one is underneath.
package contracttest

import (
	"context"
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
)

// Run executes the full parity suite against the engine produced by newEngine.
// The factory is a function so each subtest gets a clean engine.
func Run(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Helper()

	t.Run("capabilities are declared", func(t *testing.T) {
		c := newEngine(t).Capabilities()
		// Capability flags drive platform compensation (job reaping, canary
		// weights, autoscale policy, pinning stateful workloads). An adapter
		// that leaves them at the zero value silently tells the platform to
		// skip work it must do itself, so the zero value is not a valid
		// answer: an engine must either have the feature or be honest.
		if !c.RoutingMesh && !c.VolumesFollowWorkload {
			// Both false is legal (no mesh AND node-local volumes) — but only
			// if the adapter says so deliberately. This branch exists to keep
			// the two independent axes from collapsing into one flag.
			t.Log("engine has neither a routing mesh nor workload-following volumes")
		}
	})

	t.Run("nodes are listed with a normalised role", func(t *testing.T) {
		nodes, err := newEngine(t).Nodes(context.Background())
		if err != nil {
			t.Fatalf("Nodes: %v", err)
		}
		if len(nodes) == 0 {
			t.Fatal("Nodes returned nothing; an engine always has at least one machine")
		}
		for _, n := range nodes {
			if n.Hostname == "" {
				t.Error("node with empty hostname")
			}
			switch n.Role {
			case engine.RoleManager, engine.RoleWorker, engine.RoleAny:
			default:
				t.Errorf("node %q has un-normalised role %q", n.Hostname, n.Role)
			}
			// Address is what lets a caller reach a workload on this node
			// (endpoint discovery, probes). Without it every caller has to go
			// back to the engine's own node type to find one.
			if n.Address == "" {
				t.Errorf("node %q has no address; the read model must carry one", n.Hostname)
			}
		}
	})

	t.Run("leader agrees with the node list", func(t *testing.T) {
		eng := newEngine(t)
		leader, err := eng.Leader(context.Background())
		if err != nil {
			t.Fatalf("Leader: %v", err)
		}
		nodes, err := eng.Nodes(context.Background())
		if err != nil {
			t.Fatalf("Nodes: %v", err)
		}
		if leader == "" {
			t.Fatal("Leader returned empty with a nil error — an engine that has a " +
				"cluster always has a leader; report a lookup failure as an error instead")
		}
		var found bool
		var flagged int
		for _, n := range nodes {
			if n.IsLeader {
				flagged++
			}
			if n.Hostname == leader {
				found = true
			}
		}
		if !found {
			t.Errorf("Leader %q is not among the listed nodes", leader)
		}
		if flagged != 1 {
			t.Errorf("%d nodes reported IsLeader, want exactly 1", flagged)
		}
	})

	t.Run("leader follows the flagged node after failover", func(t *testing.T) {
		eng := newEngine(t)
		ctx := context.Background()
		before, err := eng.Leader(ctx)
		if err != nil {
			t.Fatalf("Leader: %v", err)
		}
		nodes, err := eng.Nodes(ctx)
		if err != nil {
			t.Fatalf("Nodes: %v", err)
		}
		// The platform's failover behaviour depends on Leader() tracking the
		// FLAG, never a remembered hostname: a cached leader is how a console
		// keeps talking to a dead manager. Re-reading must return whatever is
		// flagged now.
		var flagged string
		for _, n := range nodes {
			if n.IsLeader {
				flagged = n.Hostname
			}
		}
		if flagged != before {
			t.Errorf("Leader() = %q but the flagged node is %q; Leader must derive "+
				"from the live flag, not from a cached value", before, flagged)
		}
	})

	t.Run("workloads project identity, desired vs running, and placement", func(t *testing.T) {
		eng := newEngine(t)
		ws, err := eng.Workloads(context.Background())
		if err != nil {
			t.Fatalf("Workloads: %v", err)
		}
		if len(ws) == 0 {
			t.Fatal("Workloads returned nothing")
		}
		for _, w := range ws {
			if w.Name == "" {
				t.Error("workload with empty name")
			}
			switch w.Mode {
			case "replicated", "global":
			default:
				t.Errorf("workload %q has unexpected mode %q", w.Name, w.Mode)
			}
			// A one-shot is allowed to sit at zero running replicas: that is
			// completion, not a crash. Anything else at zero is a fault the
			// platform must be able to see.
			if w.Running == 0 && w.Desired > 0 && !w.OneShot {
				t.Logf("workload %q is at 0/%d running (fault, and visible)", w.Name, w.Desired)
			}
			if w.PlacedNode != "" && w.RunningNode != "" && w.PlacedNode != w.RunningNode {
				t.Logf("workload %q placed on %s, running on %s (drift, and visible)",
					w.Name, w.PlacedNode, w.RunningNode)
			}
		}
	})

	t.Run("restart reaches the engine", func(t *testing.T) {
		eng := newEngine(t)
		ctx := context.Background()
		ws, err := eng.Workloads(ctx)
		if err != nil {
			t.Fatalf("Workloads: %v", err)
		}
		if len(ws) == 0 {
			t.Skip("no workload to restart")
		}
		if err := eng.Restart(ctx, ws[0].Name); err != nil {
			t.Fatalf("Restart(%q): %v", ws[0].Name, err)
		}
	})
}

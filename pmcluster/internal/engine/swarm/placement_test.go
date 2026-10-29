package swarm

import (
	"testing"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
)

// TestConstraints pins the Swarm rendering of the neutral placement. This is
// the only place Swarm constraint syntax is allowed to exist, so the strings
// asserted here are the adapter's whole public contract.
func TestConstraints(t *testing.T) {
	cases := []struct {
		name string
		in   engine.Placement
		want []string
	}{
		{
			name: "unconstrained emits nothing",
			in:   engine.Placement{},
			want: nil,
		},
		{
			name: "manager role",
			in:   engine.Placement{Role: engine.RoleManager},
			want: []string{"node.role == manager"},
		},
		{
			name: "worker role",
			in:   engine.Placement{Role: engine.RoleWorker},
			want: []string{"node.role == worker"},
		},
		{
			name: "node pin",
			in:   engine.Placement{Node: "nxt-sw-5-m"},
			want: []string{"node.hostname == nxt-sw-5-m"},
		},
		{
			// Swarm ANDs constraints together, so emitting both would leave
			// the service unschedulable whenever the pinned node has the other
			// role. The pin must win outright.
			name: "pin wins over role — never both",
			in:   engine.Placement{Node: "nxt-sw-3-w", Role: engine.RoleManager},
			want: []string{"node.hostname == nxt-sw-3-w"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Constraints(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("constraints = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("constraints = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestCapabilitiesAreDeclared records what Swarm cannot do, because the
// platform layer must fill those gaps itself (jobs reaping, canary weights,
// autoscaling policy). If this ever starts returning true, the platform code
// that compensates for it needs revisiting in the same change.
func TestCapabilitiesAreDeclared(t *testing.T) {
	c := Capabilities()
	if !c.RoutingMesh {
		t.Error("Swarm has a routing mesh; platform ingress relies on it")
	}
	if !c.NodeLabels {
		t.Error("Swarm nodes carry labels; placement pins rely on them")
	}
	if c.VolumesFollowWorkload {
		t.Error("Swarm volumes are node-local: stateful workloads MUST be pinned")
	}
	if c.NativeCanary || c.NativeAutoscale || c.NativeJobs {
		t.Error("Swarm has none of these natively; claiming otherwise would let the " +
			"platform skip the work it must do itself")
	}
}

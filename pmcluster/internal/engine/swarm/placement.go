// Package swarm translates the platform's engine-neutral vocabulary into
// Docker Swarm syntax.
//
// Everything Swarm-specific about placement lives here, and nowhere else in
// the platform layer: if a second engine is added, it gets a sibling package
// (internal/engine/nomad) that expresses the same engine.Placement in its own
// syntax, and nothing above this line changes.
package swarm

import (
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
)

// Constraint strings understood by the Swarm scheduler.
const (
	constraintRoleManager = "node.role == manager"
	constraintRoleWorker  = "node.role == worker"
	constraintNodePrefix  = "node.hostname == "
)

// Constraints renders a Placement as Swarm placement constraints.
//
// A node pin wins over a role: Swarm evaluates every constraint conjunctively,
// so emitting both "node.hostname == X" and "node.role == worker" would make a
// service unschedulable whenever the pinned node holds the other role.
func Constraints(p engine.Placement) []string {
	switch {
	case p.Node != "":
		return []string{constraintNodePrefix + p.Node}
	case p.Role == engine.RoleManager:
		return []string{constraintRoleManager}
	case p.Role == engine.RoleWorker:
		return []string{constraintRoleWorker}
	default:
		return nil
	}
}

// Capabilities is what Swarm natively provides. Each false is a gap the
// platform layer has to fill (or declare as unsupported), not a bug.
func Capabilities() engine.Capabilities {
	return engine.Capabilities{
		RoutingMesh:           true,  // published ports bind on every node
		NativeJobs:            false, // replicated-job exists, but nothing reaps or schedules it
		NativeCanary:          false,
		NativeAutoscale:       false,
		VolumesFollowWorkload: false, // host paths and node-local volumes
		NodeLabels:            true,
	}
}

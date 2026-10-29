// Package engine holds the engine-neutral vocabulary of the platform layer.
//
// pmcluster is a platform that sits ABOVE a container engine (today Docker
// Swarm, possibly Nomad or k3s later). The platform decides WHERE a workload
// runs, what it is allowed to mount, and how traffic reaches it; the engine
// only schedules it. This package is that boundary: types expressed in
// platform terms, with no engine syntax in them.
//
// The rule that keeps the boundary honest: nothing in this package may
// mention a constraint string ("node.role ==", "node.hostname =="), a Swarm
// object (Service, Task, ManagerStatus) or a Nomad one (Allocation, Job). Any
// such string belongs in an adapter under internal/engine/<engine>.
package engine

// Role is the engine-neutral restriction on which class of node a workload
// may run on.
type Role string

const (
	// RoleAny places no role restriction.
	RoleAny Role = ""
	// RoleManager restricts to control-plane/manager-class nodes.
	RoleManager Role = "manager"
	// RoleWorker restricts to worker-class nodes.
	RoleWorker Role = "worker"
)

// Placement is the platform's statement of where a workload may run. It is
// deliberately structural: an engine adapter decides how to express it.
//
// Empty Placement means unconstrained — the engine may schedule anywhere.
type Placement struct {
	// Node pins the workload to one node by hostname. Used for stateful
	// workloads whose data lives on a single host: letting the engine
	// schedule them elsewhere either strands the data or starts a fresh
	// empty volume.
	Node string
	// Role restricts to a node class (ignored when Node is set).
	Role Role
	// MatchLabels requires the node to carry these labels (engine adapters
	// that have no node-label concept may ignore it, and must then report
	// the reduced capability).
	MatchLabels map[string]string
}

// Pinned reports whether the placement pins to a single node. Callers use it
// to decide whether to record the pin as a service label.
func (p Placement) Pinned() bool { return p.Node != "" }

// DeclaredPlacement values understood by the manifest layer.
const (
	// DeclaredManager and DeclaredWorker name the role-restricted values an
	// operator may write in a service's placement field.
	DeclaredManager = "manager"
	DeclaredWorker  = "worker"
)

// PlacementFor resolves the placement for one service from its declared value
// plus the platform's rules. The rules, in order:
//
//  1. "manager" / "worker" → a role restriction.
//  2. empty + stateful + a platform pin node + not global → pin to that node.
//     A stateful service holds a volume under the platform's volume root, and
//     that data exists on exactly one host, so an unpinned one would strand
//     data or come back with an empty volume. Global-mode services are exempt:
//     they run on every node and never pin.
//  3. any other value → that value is a node hostname pin.
func PlacementFor(declared string, pinNode string, stateful, global bool) Placement {
	switch declared {
	case DeclaredManager:
		return Placement{Role: RoleManager}
	case DeclaredWorker:
		return Placement{Role: RoleWorker}
	case "":
		if stateful && pinNode != "" && !global {
			return Placement{Node: pinNode}
		}
		return Placement{}
	default:
		return Placement{Node: declared}
	}
}

// Capabilities declares what an engine can do natively, so the platform can
// prefer native behaviour and fall back deliberately instead of probing.
//
// This exists because some differences cannot be hidden by an adapter: an
// engine with no routing mesh needs ingress bound per node; an engine with
// native canary does not need weighted-traffic emulation. Declaring them is
// the difference between a seam that survives a second engine and one that
// leaks the moment you plug one in.
type Capabilities struct {
	// RoutingMesh: publishing a port binds it on every node, so any node can
	// front a service.
	RoutingMesh bool
	// NativeJobs: one-shot / cron workloads are first-class and cleaned up by
	// the engine.
	NativeJobs bool
	// NativeCanary: the engine can run a canary alongside the old version and
	// promote it.
	NativeCanary bool
	// NativeAutoscale: the engine (or its companion) scales on metrics.
	NativeAutoscale bool
	// VolumesFollowWorkload: a volume can be attached to a workload regardless
	// of which node it lands on (CSI / networked storage). When false, the
	// platform must pin every stateful workload — which is exactly what
	// PlacementFor does above.
	VolumesFollowWorkload bool
	// NodeLabels: nodes carry queryable labels for placement.
	NodeLabels bool
}

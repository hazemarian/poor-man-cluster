package engine

import "context"

// Node is the platform's view of one schedulable machine. It is deliberately
// smaller than any engine's node object: only what the platform's own logic
// (placement, console inventory, failover checks) actually reads.
type Node struct {
	Hostname string
	// Role is the engine's node class, normalised to RoleManager / RoleWorker.
	Role Role
	// Availability is "active" or "drain"/"draining" — the platform refuses to
	// place new workloads on a draining node.
	Availability string
	// Status is "ready" or anything else (down, unknown).
	Status string
	// IsLeader reports the control-plane leader. Planes that elect among
	// managers use it to follow leadership.
	IsLeader bool
	// Labels are the node's placement labels (storage, leader, custom).
	Labels map[string]string
	// EngineVersion is the engine's own version on this machine.
	EngineVersion string
}

// Workload is the platform's view of one deployed unit (a Swarm service, a
// Nomad task group, …): identity, desired vs actual, and where it is.
type Workload struct {
	Name     string
	Stack    string
	Image    string
	Replicas uint64
	// Desired is the requested replica count; Running is what is actually up.
	Desired uint64
	Running uint64
	// Mode is "replicated" or "global".
	Mode string
	// OneShot marks a run-once workload: zero running replicas means it
	// completed rather than crashed.
	OneShot bool
	// Platform marks platform-managed workloads (ingress, console, backend,
	// backup, SSO) so callers can split them from customer apps without
	// name heuristics.
	Platform bool
	// PlacedNode is where the platform asked it to run (a pin), RunningNode is
	// where it actually runs. They diverge when a workload lands elsewhere,
	// which is the one condition an operator must be able to see.
	PlacedNode  string
	RunningNode string
	// Labels is the workload's full label set, used to diff a live workload
	// against a fresh render.
	Labels map[string]string
}

// Engine is the surface the platform layer drives. It is intentionally the
// READ + CONTROL-OPERATION surface, not the deploy path:
//
//   - rendering a workload (Compose → engine format) is an adapter concern that
//     already has its own seam (stacks.Deployer);
//   - scaling needs a transport method neither engine exposes yet
//     (runtime.Client has no Scale), so it is deliberately absent rather than
//     declared and left unimplemented.
//
// Every implementation must pass contracttest.Run — that suite, not this
// comment, is the definition of engine parity.
type Engine interface {
	// Capabilities declares what this engine can do natively.
	Capabilities() Capabilities
	// Nodes returns every schedulable machine.
	Nodes(ctx context.Context) ([]Node, error)
	// Leader returns the control-plane leader's hostname. An engine with no
	// elected leader returns "" with a nil error; a lookup failure returns an
	// error. Callers must distinguish the two.
	Leader(ctx context.Context) (string, error)
	// Workloads returns every deployed workload.
	Workloads(ctx context.Context) ([]Workload, error)
	// Restart forces a rolling restart of one workload, in place.
	Restart(ctx context.Context, workload string) error
}

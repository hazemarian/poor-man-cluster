package swarm

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/engine"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// Adapter implements engine.Engine on top of the Swarm transport.
//
// It is a projection layer, not a second client: it takes the domain objects
// runtime.Client already returns and restates them in engine terms. Keeping it
// projection-only means the platform layer can be written against
// engine.Engine while the Swarm specifics (service IDs, task names) stay below
// this line.
type Adapter struct {
	Client runtime.Client
}

// New wraps a Swarm client.
func New(c runtime.Client) *Adapter { return &Adapter{Client: c} }

// Capabilities reports what Swarm can do natively.
func (a *Adapter) Capabilities() engine.Capabilities { return Capabilities() }

// Nodes lists every machine in the Swarm, with roles normalised.
func (a *Adapter) Nodes(ctx context.Context) ([]engine.Node, error) {
	raw, err := a.Client.NodeList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make([]engine.Node, 0, len(raw))
	for _, n := range raw {
		out = append(out, engine.Node{
			Hostname:      n.Hostname,
			Role:          normaliseRole(n.Role),
			Availability:  n.Availability,
			Status:        n.Status,
			IsLeader:      n.IsLeader,
			EngineVersion: n.EngineVersion,
			Address:       n.Address,
		})
	}
	return out, nil
}

// Leader returns the hostname of the node Swarm currently has as manager
// leader.
//
// It derives the answer from the live flag on every call rather than caching a
// hostname: a remembered leader is exactly how a console keeps talking to a
// manager that died five minutes ago.
func (a *Adapter) Leader(ctx context.Context) (string, error) {
	nodes, err := a.Nodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.IsLeader {
			return n.Hostname, nil
		}
	}
	return "", fmt.Errorf("no swarm manager reports itself as leader")
}

// Workloads lists services as engine workloads.
func (a *Adapter) Workloads(ctx context.Context) ([]engine.Workload, error) {
	raw, err := a.Client.ServiceList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	out := make([]engine.Workload, 0, len(raw))
	for _, s := range raw {
		out = append(out, engine.Workload{
			Name:        s.Name,
			Stack:       s.Stack,
			Image:       s.Image,
			Replicas:    s.Replicas,
			Desired:     s.Desired,
			Running:     s.Replicas,
			Mode:        s.Mode,
			OneShot:     s.RunOnce,
			Platform:    s.Platform,
			PlacedNode:  s.Node,
			RunningNode: s.RunningNode,
			Labels:      s.Labels,
		})
	}
	return out, nil
}

// Restart forces an in-place rolling restart. Swarm addresses services by ID,
// so the name is resolved first — the platform deals in names, the engine in
// IDs, and that translation belongs here.
func (a *Adapter) Restart(ctx context.Context, workload string) error {
	services, err := a.Client.ServiceList(ctx)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", workload, err)
	}
	for _, s := range services {
		if s.Name == workload {
			if err := a.Client.ServiceRestart(ctx, s.ID); err != nil {
				return fmt.Errorf("restart %q: %w", workload, err)
			}
			return nil
		}
	}
	return fmt.Errorf("restart %q: no such workload", workload)
}

// normaliseRole maps an engine node class onto the platform's vocabulary.
// Swarm says "manager"/"worker"; anything else becomes RoleAny so callers never
// have to know the engine's spelling.
func normaliseRole(role string) engine.Role {
	switch role {
	case string(engine.RoleManager):
		return engine.RoleManager
	case string(engine.RoleWorker):
		return engine.RoleWorker
	default:
		return engine.RoleAny
	}
}

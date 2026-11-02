package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// SelfNode is what this node can say about itself without a store and without
// asking anyone else. It exists separately from runtime.Client because the CLI
// needs it *before* it decides whether the store may be opened at all, and
// because the useful half of that interface (NodeList, JoinTokens) is refused by
// a worker's daemon.
type SelfNode struct {
	ID string
	// Name is the docker daemon name (the hostname), always available.
	Name     string
	Hostname string
	// Role is "manager" or "worker", straight from the swarm node spec.
	Role      string
	IsManager bool
	// SwarmActive is false on a node that is not part of a swarm at all. Such a
	// node has no leader because it has no cluster yet — the CLI must work there
	// (that is how a cluster is created), and the daemon already serves in this
	// state as "standalone/local mode".
	SwarmActive bool
	// IsLeader is true only on the node that currently holds swarm leadership.
	// A worker has no ManagerStatus at all, so this stays false there.
	IsLeader bool
	// LeaderHostname names the current leader when this node can enumerate
	// managers (i.e. on a manager). It is empty on a worker, where `docker node
	// ls` is refused — that is a property of the swarm, not a bug here.
	LeaderHostname string
}

// InspectSelf reports this node's swarm identity. It works on workers as well as
// managers, which is exactly why the CLI's authority check uses it: the question
// "am I the leader?" has to be answerable on every node, including the ones that
// are not allowed to answer anything else.
func InspectSelf(ctx context.Context) (SelfNode, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return SelfNode{}, fmt.Errorf("create docker client: %w", err)
	}
	defer func() { _ = c.Close() }()

	// `docker info` answers on every node; `docker node inspect self` does not —
	// a worker's daemon refuses it outright ("This node is not a swarm manager"),
	// which is precisely the node whose answer we need to be able to classify.
	sys, err := c.Info(ctx)
	if err != nil {
		return SelfNode{}, fmt.Errorf("read local docker info: %w", err)
	}
	if sys.Swarm.LocalNodeState != "active" {
		// No swarm (or one still forming): there is no leader to defer to, so this
		// is standalone/local mode. docker info still answers, which is how a fresh
		// node can run init/cluster up at all.
		return SelfNode{Name: sys.Name, Hostname: sys.Name}, nil
	}

	self := SelfNode{
		ID:          sys.Swarm.NodeID,
		Name:        sys.Name,
		Hostname:    sys.Name,
		SwarmActive: true,
		IsManager:   sys.Swarm.ControlAvailable,
	}
	self.Role = "worker"
	if self.IsManager {
		self.Role = "manager"
	}

	// Only a manager can answer the leader question, and only a manager can name
	// the leader (listing nodes is manager-only).
	if self.IsManager && self.ID != "" {
		// "self" is resolved by the docker CLI itself, not by the daemon, so the
		// SDK call has to use the node ID from docker info. Getting this wrong is
		// not a cosmetic bug: a failed lookup would report the leader as a follower
		// and refuse its own commands.
		info, _, err := c.NodeInspectWithRaw(ctx, self.ID)
		if err != nil {
			return SelfNode{}, fmt.Errorf("inspect node %s: %w", self.ID, err)
		}
		if info.Description.Hostname != "" {
			self.Hostname = info.Description.Hostname
		}
		self.IsLeader = info.ManagerStatus != nil && info.ManagerStatus.Leader
	}

	// The leader's name is only knowable from a manager; on a worker the swarm
	// refuses to list nodes, and guessing would be worse than saying nothing.
	if self.IsManager && !self.IsLeader {
		if nodes, err := c.NodeList(ctx, swarm.NodeListOptions{}); err == nil {
			for _, n := range nodes {
				if n.ManagerStatus != nil && n.ManagerStatus.Leader {
					self.LeaderHostname = n.Description.Hostname
					break
				}
			}
		}
	}
	return self, nil
}

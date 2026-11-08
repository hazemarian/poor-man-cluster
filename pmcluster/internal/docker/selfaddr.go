package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/client"
)

// NodeSelfAddr returns this node's swarm-advertised address — node.Status.Addr,
// the address the swarm hands to every peer, to the routing mesh, and to
// anything else that has to reach this node by its own name. It is the address
// the daemon's self-telemetry must dial (see config.ResolveOTLPEndpoint), and
// the swarm's own record is the only trustworthy source for it: an address
// guessed from the host's interfaces is a different address, and pointing an
// exporter at a port nobody listens on is what saturated a node's journal on
// 2026-11-08.
//
// Which call answers depends on the node's role, and that is a property of the
// swarm's API rather than something to work around:
//
//   - manager: docker info names this node's ID, and NodeInspectWithRaw reads
//     our own node record — the same record a NodeList entry carries.
//   - worker: the node-inspect API is manager-only ("This node is not a swarm
//     manager"), but docker info already reports the same Status.Addr in
//     Swarm.NodeAddr, straight from the daemon's own view of itself. Verified
//     on a live worker: `docker node inspect self` is refused there while
//     `docker info` reports the identical address.
//
// A node that is not in a swarm has no swarm address, and this returns an error
// rather than inventing one: the caller disables self-telemetry instead.
func NodeSelfAddr(ctx context.Context) (string, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", fmt.Errorf("create docker client: %w", err)
	}
	defer func() { _ = c.Close() }()

	info, err := c.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("read local docker info: %w", err)
	}
	if info.Swarm.LocalNodeState != "active" {
		return "", fmt.Errorf("this node is not part of a swarm (local node state %q)", info.Swarm.LocalNodeState)
	}

	addr := strings.TrimSpace(info.Swarm.NodeAddr)
	if info.Swarm.ControlAvailable && info.Swarm.NodeID != "" {
		// Manager: read the node's own record rather than the summary in
		// docker info. Managed nodes only exist on a manager, and this is the
		// record the swarm places tasks against.
		node, _, err := c.NodeInspectWithRaw(ctx, info.Swarm.NodeID)
		if err != nil {
			return "", fmt.Errorf("inspect own node %s: %w", info.Swarm.NodeID, err)
		}
		addr = strings.TrimSpace(node.Status.Addr)
	}
	if addr == "" {
		return "", fmt.Errorf("the swarm reports no address for this node")
	}
	return addr, nil
}

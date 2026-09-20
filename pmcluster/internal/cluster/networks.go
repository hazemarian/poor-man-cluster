package cluster

import (
	"context"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
)

// pmclusterLabel is set on every Docker resource pmcluster creates so we
// can identify them later (e.g. for `cluster down --purge`).
const pmclusterLabel = "io.pmcluster.managed"

// PmclusterLabel is exported for cross-package access (serve health checks).
const PmclusterLabel = pmclusterLabel

// EnsureNetwork creates an attachable overlay network if it doesn't already
// exist. Returns true if the network was newly created (handy for logs).
//
// Idempotent: repeated calls are safe and cheap (one inspect, no recreate).
// Existing networks are NEVER reconfigured — if an operator pre-created the
// network with different settings, pmcluster respects their choice.
func EnsureNetwork(ctx context.Context, d runtime.Client, name string) (created bool, err error) {
	exists, err := d.NetworkExists(ctx, name)
	if err != nil {
		return false, fmt.Errorf("check network %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	err = d.NetworkCreate(ctx, runtime.NetworkSpec{
		Name:       name,
		Driver:     "overlay",
		Attachable: true,
	})
	if err != nil {
		return false, fmt.Errorf("create network %s: %w", name, err)
	}
	return true, nil
}

// EnsureBundledNetworks creates the two overlay networks the bundled stacks
// need: traefik-net (ingress) and monitoring-net (telemetry).
// Returns the names of any newly created networks.
func EnsureBundledNetworks(ctx context.Context, d runtime.Client) ([]string, error) {
	var created []string
	for _, name := range []string{"traefik-net", "monitoring-net"} {
		isNew, err := EnsureNetwork(ctx, d, name)
		if err != nil {
			return created, err
		}
		if isNew {
			created = append(created, name)
		}
	}
	return created, nil
}

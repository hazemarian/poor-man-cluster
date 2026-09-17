// Package impl holds the local adapters that implement the service ports.
package impl

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
)

// Cluster adapts the cluster package functions (up / update / down / status)
// to the ClusterService port. Consumers depend on the port, never on the
// cluster package directly.
type Cluster struct{}

// NewCluster returns a ClusterService backed by the local cluster core.
func NewCluster() service.ClusterService { return &Cluster{} }

// Up brings the cluster up end-to-end (see cluster.Up).
func (c *Cluster) Up(ctx context.Context, deps cluster.UpDeps, in cluster.UpInput) (*cluster.UpResult, error) {
	return cluster.Up(ctx, deps, in)
}

// Update re-applies the platform stacks content-aware (see cluster.Update).
func (c *Cluster) Update(ctx context.Context, deps cluster.UpdateDeps, in cluster.UpdateInput) (*cluster.UpdateResult, error) {
	return cluster.Update(ctx, deps, in)
}

// Down tears the cluster's platform stacks down (see cluster.Down).
func (c *Cluster) Down(ctx context.Context, deps cluster.DownDeps, in cluster.DownInput) (*cluster.DownResult, error) {
	return cluster.Down(ctx, deps, in)
}

// Status reports the cluster health (see cluster.Status).
func (c *Cluster) Status(ctx context.Context, d docker.Client) (*cluster.StatusReport, error) {
	return cluster.Status(ctx, d)
}

package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/cluster"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// ClusterService manages the platform itself: bringing the Swarm up, applying
// changes (cluster update), tearing it down and reporting status. The local
// adapter wraps the cluster package functions; a later remote adapter will
// proxy to the daemon API. The dep structs (UpDeps, UpdateDeps, DownDeps)
// carry the Docker/deployer/provisioner wiring and are supplied by the
// adapter's constructor.
type ClusterService interface {
	Up(ctx context.Context, deps cluster.UpDeps, in cluster.UpInput) (*cluster.UpResult, error)
	Update(ctx context.Context, deps cluster.UpdateDeps, in cluster.UpdateInput) (*cluster.UpdateResult, error)
	Down(ctx context.Context, deps cluster.DownDeps, in cluster.DownInput) (*cluster.DownResult, error)
	Status(ctx context.Context, d docker.Client) (*cluster.StatusReport, error)
}

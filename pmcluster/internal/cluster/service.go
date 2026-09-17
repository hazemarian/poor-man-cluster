package cluster

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/docker"
)

// Service is the platform-lifecycle port: bringing the Swarm up, applying
// changes (cluster update), tearing it down and reporting status. The local
// binding below wraps the package's own functions; a remote adapter could
// proxy to the daemon API. The dep structs (UpDeps, UpdateDeps, DownDeps)
// carry the Docker/deployer/provisioner wiring and are supplied by the
// adapter's constructor.
type Service interface {
	Up(ctx context.Context, deps UpDeps, in UpInput) (*UpResult, error)
	Update(ctx context.Context, deps UpdateDeps, in UpdateInput) (*UpdateResult, error)
	Down(ctx context.Context, deps DownDeps, in DownInput) (*DownResult, error)
	Status(ctx context.Context, d docker.Client) (*StatusReport, error)
}

// Local is the thin binding adapter that satisfies Service by delegating to
// the package's own functions. Consumers depend on Service, never on the
// free functions directly.
type Local struct{}

// NewService returns a Service backed by the local cluster core.
func NewService() Service { return &Local{} }

// Up brings the cluster up end-to-end (see Up).
func (l *Local) Up(ctx context.Context, deps UpDeps, in UpInput) (*UpResult, error) {
	return Up(ctx, deps, in)
}

// Update re-applies the platform stacks content-aware (see Update).
func (l *Local) Update(ctx context.Context, deps UpdateDeps, in UpdateInput) (*UpdateResult, error) {
	return Update(ctx, deps, in)
}

// Down tears the cluster's platform stacks down (see Down).
func (l *Local) Down(ctx context.Context, deps DownDeps, in DownInput) (*DownResult, error) {
	return Down(ctx, deps, in)
}

// Status reports the cluster health (see Status).
func (l *Local) Status(ctx context.Context, d docker.Client) (*StatusReport, error) {
	return Status(ctx, d)
}

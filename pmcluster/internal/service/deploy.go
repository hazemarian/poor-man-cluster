package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/deploy"
)

// DeployService deploys, rolls back and removes application stacks. It is the
// single business entry point shared by the daemon API, the webhook receiver
// and the CLI. Implemented by *deploy.Service.
type DeployService interface {
	// Deploy translates a DSL manifest and deploys the stack.
	Deploy(ctx context.Context, p deploy.Payload) (*deploy.DeployResult, error)
	// Rollback redeploys a stack from a stored revision.
	Rollback(ctx context.Context, stackName string, sourceRevision int64) (*deploy.DeployResult, error)
	// Undeploy removes the stack's services, volumes and secrets, then its record.
	Undeploy(ctx context.Context, stackName string) error
}

package service

import (
	"context"
	"errors"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// ErrEdgeUserProtected is returned by APIKeysService.Delete when the target
// is the 'edge' user that the operator console authenticates with.
var ErrEdgeUserProtected = errors.New("edge user is required by the operator console and cannot be removed")

// ErrBackupTriggerNotConfigured is returned by BackupsService.Trigger when no
// backup trigger is wired. The daemon always wires one; consumers that only
// list backups can run without one.
var ErrBackupTriggerNotConfigured = errors.New("backup trigger not configured")

// ErrSelfDelete is returned by APIKeysService.Delete when the caller tries
// to remove the API key it is currently authenticated with.
var ErrSelfDelete = errors.New("cannot remove the API key you are currently authenticated with")

// WebhooksService manages the webhook receiver sources. Each source has a
// one-time secret used to HMAC-sign deploy requests.
type WebhooksService interface {
	Create(ctx context.Context, source, description string) (secret string, err error)
	List(ctx context.Context) ([]*store.WebhookSource, error)
	Delete(ctx context.Context, source string) error
}

// APIKeysService manages daemon API tokens (users). Tokens are shown once at
// creation; only a hash is stored.
type APIKeysService interface {
	Create(ctx context.Context, name string) (id int64, token string, err error)
	List(ctx context.Context) ([]store.UserRow, error)
	Delete(ctx context.Context, id int64) error
}

// BackupsService triggers and lists backups of stack volumes.
type BackupsService interface {
	// Trigger records the backup run, executes the backup and finalizes the
	// audit row with the resulting archive paths.
	Trigger(ctx context.Context, stackName string, revision int64) (id int64, paths []string, err error)
	// List returns the most recent backup runs.
	List(ctx context.Context, limit int) ([]*store.Backup, error)
	// ListForStack returns the backup runs recorded for a single stack.
	ListForStack(ctx context.Context, stackName string) ([]*store.Backup, error)
}

// CredentialsService manages the platform bootstrap credentials (Traefik,
// Portainer, OpenObserve, edge) with rotation.
type CredentialsService interface {
	Get(ctx context.Context, name string) (*store.ManagedCredential, error)
	List(ctx context.Context) ([]*store.ManagedCredential, error)
	Rotate(ctx context.Context, name string) (password string, err error)
}

// StacksService is the read side of deployed stacks.
type StacksService interface {
	Get(ctx context.Context, name string) (*store.Stack, error)
	List(ctx context.Context) ([]*store.Stack, error)
	Revisions(ctx context.Context, name string, limit int) ([]*store.StackRevision, error)
}

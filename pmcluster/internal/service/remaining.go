package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

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
	Create(ctx context.Context, name string) (token string, err error)
	List(ctx context.Context) ([]store.UserRow, error)
	Delete(ctx context.Context, id int64) error
}

// BackupsService triggers and lists backups of stack volumes.
type BackupsService interface {
	Trigger(ctx context.Context, stackName string, revision int64) (int64, error)
	List(ctx context.Context, limit int) ([]*store.Backup, error)
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

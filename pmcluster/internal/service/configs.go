package service

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// ConfigsService is the business entry point for the versioned configs store
// (cluster-scope platform templates and per-stack service configs). The local
// adapter wraps *store.Store; a remote adapter will proxy to the daemon API.
type ConfigsService interface {
	Create(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error)
	Get(ctx context.Context, name string) (*store.ConfigRow, error)
	List(ctx context.Context, scope, stack string) ([]*store.ConfigRow, error)
	Update(ctx context.Context, name, content, version string) (string, error)
	Rollback(ctx context.Context, name string, versionID int64) (string, error)
	Delete(ctx context.Context, name string) error
	ListVersions(ctx context.Context, name string) ([]*store.ConfigVersionRow, error)
	// SetRendered stamps the post-substitution snapshot on a config row.
	SetRendered(ctx context.Context, name, content string) error
	// ListRendered returns the config rows that carry a rendered snapshot.
	ListRendered(ctx context.Context) ([]*store.ConfigRow, error)
}

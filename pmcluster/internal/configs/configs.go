// Package configs is the bounded context for versioned configs: cluster-scope
// platform templates and per-stack service configs, plus the rendered
// post-substitution snapshots written by every cluster update.
package configs

import "context"

// Config is a versioned config value (cluster- or service-scoped). Rendered
// holds the last post-substitution snapshot written by cluster update ("" when
// none has been recorded).
type Config struct {
	ID        int64
	Scope     string
	Stack     string
	Name      string
	Kind      string
	Version   string
	Hash      string
	Content   string
	Rendered  string
	CreatedAt int64
	UpdatedAt int64
}

// ConfigVersion is one entry of a config's edit history.
type ConfigVersion struct {
	ID        int64
	Hash      string
	CreatedAt int64
}

// Service is the configs port: CRUD with version history, plus the read-only
// listing of rendered snapshots.
type Service interface {
	Create(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error)
	Get(ctx context.Context, name string) (*Config, error)
	List(ctx context.Context, scope, stack string) ([]Config, error)
	Update(ctx context.Context, name, content, version string) (string, error)
	Rollback(ctx context.Context, name string, versionID int64) (string, error)
	Delete(ctx context.Context, name string) error
	ListVersions(ctx context.Context, name string) ([]ConfigVersion, error)
	ListRendered(ctx context.Context) ([]Config, error)
}

// Renderer is the local-only snapshot capability: it stamps a config row with
// its rendered post-substitution content. Remote adapters cannot honor it —
// cluster update on the daemon performs the snapshot against the store.
type Renderer interface {
	SetRendered(ctx context.Context, name, content string) error
}

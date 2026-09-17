package configs

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Local implements Service and Renderer against the local store.
type Local struct {
	Store *store.Store
}

// NewLocal wires the configs service onto the store.
func NewLocal(st *store.Store) *Local { return &Local{Store: st} }

var (
	_ Service  = (*Local)(nil)
	_ Renderer = (*Local)(nil)
)

func (s *Local) Create(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error) {
	return s.Store.CreateConfig(ctx, scope, stack, name, kind, content, version)
}

func (s *Local) Get(ctx context.Context, name string) (*Config, error) {
	row, err := s.Store.GetConfig(ctx, name)
	if err != nil {
		return nil, err
	}
	return rowModel(row), nil
}

func (s *Local) List(ctx context.Context, scope, stack string) ([]Config, error) {
	rows, err := s.Store.ListConfigs(ctx, scope, stack)
	if err != nil {
		return nil, err
	}
	out := make([]Config, 0, len(rows))
	for _, r := range rows {
		out = append(out, *rowModel(r))
	}
	return out, nil
}

func (s *Local) Update(ctx context.Context, name, content, version string) (string, error) {
	return s.Store.UpdateConfig(ctx, name, content, version)
}

func (s *Local) Rollback(ctx context.Context, name string, versionID int64) (string, error) {
	return s.Store.RollbackConfig(ctx, name, versionID)
}

func (s *Local) Delete(ctx context.Context, name string) error {
	return s.Store.DeleteConfig(ctx, name)
}

func (s *Local) ListVersions(ctx context.Context, name string) ([]ConfigVersion, error) {
	rows, err := s.Store.ListConfigVersions(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]ConfigVersion, 0, len(rows))
	for _, v := range rows {
		out = append(out, ConfigVersion{ID: v.ID, Hash: v.Hash, CreatedAt: v.CreatedAt})
	}
	return out, nil
}

func (s *Local) ListRendered(ctx context.Context) ([]Config, error) {
	rows, err := s.Store.ListRenderedConfigs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Config, 0, len(rows))
	for _, r := range rows {
		m := rowModel(r)
		m.Content = ""
		m.Rendered = r.RenderedContent
		out = append(out, *m)
	}
	return out, nil
}

func (s *Local) SetRendered(ctx context.Context, name, content string) error {
	return s.Store.SetRendered(ctx, name, content)
}

func rowModel(r *store.ConfigRow) *Config {
	return &Config{
		ID: r.ID, Scope: r.Scope, Stack: r.Stack, Name: r.Name, Kind: r.Kind,
		Version: r.Version, Hash: r.Hash, Content: r.Content,
		Rendered: r.RenderedContent, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

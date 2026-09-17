// Package impl contains the local adapters that implement the service ports:
// each adapter wires the domain logic (store, cipher, cluster, deploy) behind
// the interface defined in internal/service.
package impl

import (
	"context"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/service"
	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Configs implements service.ConfigsService against the local store.
type Configs struct {
	Store *store.Store
}

// NewConfigs wires the configs service onto the store.
func NewConfigs(st *store.Store) service.ConfigsService {
	return &Configs{Store: st}
}

func (s *Configs) Create(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error) {
	return s.Store.CreateConfig(ctx, scope, stack, name, kind, content, version)
}

func (s *Configs) Get(ctx context.Context, name string) (*store.ConfigRow, error) {
	return s.Store.GetConfig(ctx, name)
}

func (s *Configs) List(ctx context.Context, scope, stack string) ([]*store.ConfigRow, error) {
	return s.Store.ListConfigs(ctx, scope, stack)
}

func (s *Configs) Update(ctx context.Context, name, content, version string) (string, error) {
	return s.Store.UpdateConfig(ctx, name, content, version)
}

func (s *Configs) Rollback(ctx context.Context, name string, versionID int64) (string, error) {
	return s.Store.RollbackConfig(ctx, name, versionID)
}

func (s *Configs) Delete(ctx context.Context, name string) error {
	return s.Store.DeleteConfig(ctx, name)
}

func (s *Configs) ListVersions(ctx context.Context, name string) ([]*store.ConfigVersionRow, error) {
	return s.Store.ListConfigVersions(ctx, name)
}

func (s *Configs) SetRendered(ctx context.Context, name, content string) error {
	return s.Store.SetRendered(ctx, name, content)
}

func (s *Configs) ListRendered(ctx context.Context) ([]*store.ConfigRow, error) {
	return s.Store.ListRenderedConfigs(ctx)
}

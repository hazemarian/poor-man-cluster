package configs

import (
	"context"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// Local implements Service and Renderer against the local store.
//
// When Docker is set (daemon-side), every config write also materializes a
// content-addressed swarm config (<name>_<sha8>) so the value lives in the
// swarm's Raft store — the DB row is the index (and holds the value for
// rebuilds), the swarm object is what containers actually mount. Docker nil
// (tests, remote) keeps the old DB-only behavior.
type Local struct {
	Store  *store.Store
	Docker runtime.Client
}

// NewLocal wires the configs service onto the store.
func NewLocal(st *store.Store) *Local { return &Local{Store: st} }

var (
	_ Service  = (*Local)(nil)
	_ Renderer = (*Local)(nil)
)

// mirrorSwarmConfig materializes the config value into a content-addressed
// swarm config. Best-effort: failures are ignored rather than failing the
// caller's write — a missing swarm object is repaired by the next cluster
// update's rebuild pass.
func (s *Local) mirrorSwarmConfig(ctx context.Context, name, content string) {
	if s.Docker == nil {
		return
	}
	target := store.SwarmConfigName(name, store.ConfigHash(content))
	exists, err := s.Docker.ConfigExists(ctx, target)
	if err != nil {
		return
	}
	if exists {
		return
	}
	_ = s.Docker.ConfigCreate(ctx, runtime.ConfigSpec{
		Name: target,
		Data: []byte(content),
		Labels: map[string]string{
			"io.pmcluster.managed": "true",
			"pmcluster.base":       name,
			"pmcluster.data_hash":  store.ConfigHash(content),
		},
	})
}

// Create stores a new config row and mirrors the value into the swarm.
func (s *Local) Create(ctx context.Context, scope, stack, name, kind, content, version string) (int64, error) {
	id, err := s.Store.CreateConfig(ctx, scope, stack, name, kind, content, version)
	if err != nil {
		return 0, err
	}
	s.mirrorSwarmConfig(ctx, name, content)
	return id, nil
}

// Get returns one config row by name.
func (s *Local) Get(ctx context.Context, name string) (*Config, error) {
	row, err := s.Store.Config(ctx, name)
	if err != nil {
		return nil, err
	}
	return rowModel(row), nil
}

// List returns the config rows matching the optional scope and stack filters.
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

// Update writes new content for a config, mirrors the value into the swarm,
// and returns the new content hash.
func (s *Local) Update(ctx context.Context, name, content, version string) (string, error) {
	hash, err := s.Store.UpdateConfig(ctx, name, content, version)
	if err != nil {
		return "", err
	}
	s.mirrorSwarmConfig(ctx, name, content)
	return hash, nil
}

// Retag reassigns the config's scope and stack without touching its content.
func (s *Local) Retag(ctx context.Context, name, scope, stack string) error {
	return s.Store.UpdateConfigStack(ctx, name, scope, stack)
}

// Rollback restores a stored config version, re-mirrors the restored value
// into the swarm, and returns the restored content hash.
func (s *Local) Rollback(ctx context.Context, name string, versionID int64) (string, error) {
	hash, err := s.Store.RollbackConfig(ctx, name, versionID)
	if err != nil {
		return "", err
	}
	// Re-materialize the restored value into the swarm so the object matches
	// the rolled-back DB row.
	if row, err := s.Store.Config(ctx, name); err == nil {
		s.mirrorSwarmConfig(ctx, name, row.Content)
	}
	return hash, nil
}

// Delete removes a config row by name.
func (s *Local) Delete(ctx context.Context, name string) error {
	return s.Store.DeleteConfig(ctx, name)
}

// ListVersions returns a config's edit history.
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

// ListRendered returns the rendered post-substitution snapshots (rendered
// content only; source content is cleared).
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

// SetRendered stamps a config row with its rendered post-substitution content.
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

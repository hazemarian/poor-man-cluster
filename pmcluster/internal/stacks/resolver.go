package stacks

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// StoreConfigResolver implements manifest.EnvResolver against the DB config
// store, so `env: X: config(name)` values are injected from data.db.
//
// Resolutions are STACK-SCOPED: each lookup names the stack being deployed,
// and only that stack's own rows (or unattached shared rows with an empty
// stack) are eligible. A config created for a different stack is never
// resolved — it would otherwise silently leak into another stack's env.
//
// Swarm-first: when Docker is set (daemon-side renders), the config VALUE is
// read from the swarm — the DB row is the index, its Hash derives the
// content-addressed swarm config name (store.SwarmConfigName), and
// ConfigInspect returns the bytes. If the swarm object is missing it is
// rebuilt from the DB value (the DB always holds the value, so a lost swarm
// object is recoverable). When Docker is nil (CLI remote deploys, tests) the
// DB Content is used directly.
type StoreConfigResolver struct {
	Store  *store.Store
	Docker runtime.Client
}

// swarmConfigNameFor returns the content-addressed swarm config name for a
// DB config row (name_<sha8>), or the bare name when the row has no hash.
func swarmConfigNameFor(c *store.ConfigRow) string {
	return store.SwarmConfigName(c.Name, c.Hash)
}

// ResolveConfig returns the config's content for the given stack, scoped to
// that stack (or a shared empty-stack row). Swarm-first when Docker is set;
// falls back to the DB value otherwise. Wraps store errors with
// operator-friendly context.
func (r *StoreConfigResolver) ResolveConfig(ctx context.Context, stack, name string) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("config resolution unavailable (no store)")
	}
	c, err := r.Store.GetConfigForStack(ctx, stack, name)
	if err != nil {
		if errors.Is(err, store.ErrConfigNotFound) {
			return "", fmt.Errorf("config %q not found for stack %q — create it with `pmcluster config create --scope service --stack %s %s`, or leave the stack empty to share it across stacks", name, stack, stack, name)
		}
		return "", fmt.Errorf("get config %q for stack %q: %w", name, stack, err)
	}

	if r.Docker != nil {
		swarmName := swarmConfigNameFor(c)
		if cur, err := r.Docker.ConfigInspect(ctx, swarmName); err == nil {
			return string(cur.Data), nil
		}
		// Swarm object missing or unreadable — rebuild it from the DB value
		// (the DB is the index AND holds the value, so a lost swarm object is
		// recovered automatically).
		if err := r.Docker.ConfigCreate(ctx, runtime.ConfigSpec{
			Name: swarmName,
			Data: []byte(c.Content),
			Labels: map[string]string{
				"io.pmcluster.managed": "true",
				"pmcluster.base":       c.Name,
			},
		}); err == nil {
			return c.Content, nil
		}
		// Rebuild failed — degrade to the DB value (still correct, just not
		// swarm-replicated until the next cluster update repair pass).
		return c.Content, nil
	}

	return c.Content, nil
}

// ResolveSetting implements manifest.SettingsResolver so `env: X:
// settings(name)` values are injected from the cluster_settings table (the
// same source the CLI `cluster settings` and the console read). Unknown
// settings surface as an error so a typo in a manifest fails loud.
func (r *StoreConfigResolver) ResolveSetting(ctx context.Context, stack, name string) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("settings resolution unavailable (no store)")
	}
	v, err := r.Store.GetSetting(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrSettingNotFound) {
			return "", fmt.Errorf("setting %q not found — set it with `pmcluster cluster settings set %s=...`", name, name)
		}
		return "", fmt.Errorf("get setting %q: %w", name, err)
	}
	return v, nil
}

// ResolveConfigPath implements manifest.ConfigPathResolver so `config_path(name)`
// file mounts resolve to their in-container target path. The daemon-side
// resolver (serve.go) wires a custom path resolver; this default keeps the
// DB-backed default (/etc/<name>) for CLI deploys.
func (r *StoreConfigResolver) ResolveConfigPath(ctx context.Context, stack, name string) (string, error) {
	return "/etc/" + name, nil
}

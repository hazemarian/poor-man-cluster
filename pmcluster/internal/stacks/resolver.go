package stacks

import (
	"context"
	"errors"
	"fmt"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
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
//
// Cipher decrypts secret VALUES for `env: X: secret(name)` refs: Docker
// secrets are write-only (no payload readback), so secret() must decrypt the
// DB ciphertext. Nil Cipher makes secret() refs fail loud.
type StoreConfigResolver struct {
	Store  *store.Store
	Docker runtime.Client
	Cipher *credentials.Cipher
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

// ResolveSecretValue implements manifest.SecretValueResolver so
// `env: X: secret(name)` injects the secret's VALUE as-is. Docker secrets are
// write-only (no payload readback), so the plaintext is decrypted from the DB
// ciphertext with the cluster encryption key. Stack-scoped: only rows tagged
// to this stack (or shared unattached rows) are eligible.
func (r *StoreConfigResolver) ResolveSecretValue(ctx context.Context, stack, name string) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("secret-value resolution unavailable (no store)")
	}
	if r.Cipher == nil {
		return "", fmt.Errorf("secret-value resolution unavailable (no encryption key — run locally on a node with ~/.pmcluster/.encryption_key)")
	}
	sec, err := r.Store.GetSecret(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrSecretNotFound) {
			return "", fmt.Errorf("secret %q not found — create it with `pmcluster secret create %s <value> --scope service --stack %s`", name, name, stack)
		}
		return "", fmt.Errorf("get secret %q: %w", name, err)
	}
	if sec.Stack != "" && sec.Stack != stack {
		return "", fmt.Errorf("secret %q belongs to stack %q — a stack-scoped secret cannot be resolved by another stack", name, sec.Stack)
	}
	plain, err := r.Cipher.Decrypt(sec.Payload)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %q: %w", name, err)
	}
	return string(plain), nil
}

package stacks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/credentials"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/errs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/refs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/runtime"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
)

// configRefRe matches the references a config file's CONTENT may carry:
// config(x)/secret(x)/secrets(x)/settings(x)/config_path(x). Config files are
// not env values — they are whole documents — so these are substituted inline,
// anywhere in the text, unlike the anchored whole-value form used for env.
var configRefRe = regexp.MustCompile(`\b(config_path|config|secrets|settings|secret)\(([^)]+)\)`)

// configExpandDepth bounds config → config reference chains so a cycle
// (a.yaml → b.yaml → a.yaml) fails loudly instead of hanging the deploy.
const configExpandDepth = 8

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
// ExpandConfigRefs substitutes every config()/secret()/secrets()/settings()/
// config_path() reference inside a config file's content.
//
// Secrets are resolved at RENDER time, never stored: the DB row keeps
// "password: secret(db_password)" and only the swarm object materialised for a
// specific deploy carries the plaintext. That is deliberate — persisting the
// expanded text would copy every referenced secret into a config row readable by
// anyone with config access.
func (r *StoreConfigResolver) ExpandConfigRefs(ctx context.Context, stack, content string) (string, error) {
	return r.expandConfigRefs(ctx, stack, content, 0)
}

func (r *StoreConfigResolver) expandConfigRefs(ctx context.Context, stack, content string, depth int) (string, error) {
	if depth > configExpandDepth {
		return "", fmt.Errorf("config reference chain too deep (%d) — check for a cycle between config() references", configExpandDepth)
	}
	if !strings.Contains(content, "(") {
		return content, nil
	}
	var firstErr error
	out := configRefRe.ReplaceAllStringFunc(content, func(match string) string {
		if firstErr != nil {
			return match
		}
		sub := configRefRe.FindStringSubmatch(match)
		kind, name := sub[1], strings.TrimSpace(sub[2])
		if name == "" {
			firstErr = fmt.Errorf("%s() reference with an empty name in config content", kind)
			return match
		}
		var (
			resolved string
			err      error
		)
		switch kind {
		case "config_path":
			resolved, err = r.ResolveConfigPath(ctx, stack, name)
		case "config":
			var nested string
			nested, err = r.resolveConfig(ctx, stack, name, depth+1)
			// ResolveConfig already expands; only chain through the depth guard.
			if err == nil {
				resolved = nested
			}
		case "secrets":
			resolved = refs.SecretMountPath(name)
		case "secret":
			resolved, err = r.ResolveSecretValue(ctx, stack, name)
		case "settings":
			resolved, err = r.ResolveSetting(ctx, stack, name)
		}
		if err != nil {
			firstErr = fmt.Errorf("%s(%s) in config content: %w", kind, name, err)
			return match
		}
		return resolved
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

func (r *StoreConfigResolver) ResolveConfig(ctx context.Context, stack, name string) (string, error) {
	return r.resolveConfig(ctx, stack, name, 0)
}

// resolveConfig fetches the stored content and expands the references inside it.
// depth tracks config() → config() chains so a cycle fails loudly.
func (r *StoreConfigResolver) resolveConfig(ctx context.Context, stack, name string, depth int) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("config resolution unavailable (no store)")
	}
	c, err := r.Store.ConfigForStack(ctx, stack, name)
	if err != nil {
		if errors.Is(err, errs.ErrConfigNotFound) {
			return "", fmt.Errorf("config %q not found for stack %q — create it with `pmcluster config create --scope service --stack %s %s`, or leave the stack empty to share it across stacks", name, stack, stack, name)
		}
		return "", fmt.Errorf("get config %q for stack %q: %w", name, stack, err)
	}

	if r.Docker != nil {
		swarmName := swarmConfigNameFor(c)
		if cur, err := r.Docker.ConfigInspect(ctx, swarmName); err == nil {
			return r.expandConfigRefs(ctx, stack, string(cur.Data), depth)
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
			return r.expandConfigRefs(ctx, stack, c.Content, depth)
		}
		// Rebuild failed — degrade to the DB value (still correct, just not
		// swarm-replicated until the next cluster update repair pass).
		return r.expandConfigRefs(ctx, stack, c.Content, depth)
	}

	return r.expandConfigRefs(ctx, stack, c.Content, depth)
}

// ResolveSetting implements manifest.SettingsResolver so `env: X:
// settings(name)` values are injected from the cluster_settings table (the
// same source the CLI `cluster settings` and the console read). Unknown
// settings surface as an error so a typo in a manifest fails loud.
func (r *StoreConfigResolver) ResolveSetting(ctx context.Context, stack, name string) (string, error) {
	if r == nil || r.Store == nil {
		return "", fmt.Errorf("settings resolution unavailable (no store)")
	}
	v, err := r.Store.Setting(ctx, name)
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
	// The mount location is the platform's decision, and it is a cluster
	// setting so a deployment can relocate its config tree without touching a
	// single manifest. Defaults to /etc/config/<name>.yaml.
	root := ""
	if r != nil && r.Store != nil {
		root = r.Store.SettingDefault(ctx, SettingConfigMountRoot, refs.DefaultConfigMountRoot)
	}
	return refs.ConfigMountPath(root, name), nil
}

// ConfigMountRoot implements manifest.ConfigRootResolver so the injected
// CONFIG_PATH follows the cluster's configured root.
func (r *StoreConfigResolver) ConfigMountRoot(ctx context.Context, stack string) (string, error) {
	root := ""
	if r != nil && r.Store != nil {
		root = r.Store.SettingDefault(ctx, SettingConfigMountRoot, refs.DefaultConfigMountRoot)
	}
	if strings.TrimSpace(root) == "" {
		return refs.DefaultConfigMountRoot, nil
	}
	return strings.TrimSuffix(root, "/"), nil
}

// SettingConfigMountRoot is the cluster setting holding the directory app
// config files are mounted under. Empty/unset means refs.DefaultConfigMountRoot.
const SettingConfigMountRoot = "config_mount_root"

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
	sec, err := r.Store.Secret(ctx, name)
	if err != nil {
		if errors.Is(err, errs.ErrSecretNotFound) {
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

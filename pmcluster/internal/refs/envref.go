package refs

import (
	"path"
	"regexp"
	"strings"
)

// envRefRe matches a config_path(name)/config(name)/secret_path(name)/
// secret(name) reference when it is the WHOLE value of an env entry
// (`env: KEY: config(x)`). Unlike inlineRefRe it is anchored — it must not
// match text that has extra content around the reference.
var envRefRe = regexp.MustCompile(`^(config_path|config|secret_path|secret)\(([^)]+)\)$`)

// configPathRe matches a config_path(<name>) expression — the DSL syntax for
// declaring a Docker config FILE mount (`configs: [config_path(cfg)]`).
var configPathRe = regexp.MustCompile(`^config_path\(([^)]+)\)$`)

// EnvRef is a parsed config_path(name)/config(name)/secrets(name)/
// settings(name)/secret(name) env-value reference.
type EnvRef struct {
	Kind string
	Name string
}

// ParseEnvRef reports whether v is exactly a reference of one of those kinds
// (whole value) and returns its kind + name.
func ParseEnvRef(v string) (EnvRef, bool) {
	sub := envRefRe.FindStringSubmatch(v)
	if len(sub) != 3 {
		return EnvRef{}, false
	}
	return EnvRef{Kind: sub[1], Name: strings.TrimSpace(sub[2])}, true
}

// MalformedEnvRef reports whether v starts like a reference but does not parse
// as one — a typo or a forgotten close paren. Used by manifest validation so
// operators fail loud.
func MalformedEnvRef(v string) bool {
	// settings( is deliberately still listed even though the token is no longer a
	// reference kind: a manifest that kept it would otherwise pass through as the
	// literal string "settings(x)" in the container environment — a silent
	// misconfiguration instead of a validation error.
	// secrets( and settings( are deliberately listed even though neither is a
	// reference kind any more: a manifest that still carries one would otherwise
	// pass through as the literal string "secrets(x)" in the container
	// environment — a silent misconfiguration instead of a validation error.
	if !strings.HasPrefix(v, "config_path(") && !strings.HasPrefix(v, "config(") &&
		!strings.HasPrefix(v, "secret_path(") && !strings.HasPrefix(v, "secrets(") &&
		!strings.HasPrefix(v, "secret(") && !strings.HasPrefix(v, "settings(") {
		return false
	}
	_, ok := ParseEnvRef(v)
	return !ok
}

// SecretMountPath is the in-container path where a mounted Swarm secret file
// appears (compose `secrets:` mounts default to /run/secrets/<name>).
func SecretMountPath(name string) string {
	return "/run/secrets/" + name
}

// DefaultConfigMountRoot is the directory every app config is mounted under.
//
// The mount location is a PLATFORM decision, not a manifest one: a stack names
// the config it needs (`configs: [config_path(cfg)]`) and the platform decides
// where that file lands. Keeping every config in one directory means an app can
// read its whole configuration as a directory — the Kubernetes ConfigMap shape —
// with no container path in the manifest and no per-config env var restating a
// path only the platform knows.
//
// Platform configs whose consuming binary hardcodes a path (Traefik, the OTel
// collector) are unaffected: those are resolved before this default applies.
const DefaultConfigMountRoot = "/etc/config"

// DefaultConfigExtension is appended to a config name that carries no extension
// of its own, so a config called "plugins" mounts as /etc/config/plugins.yaml.
// A name that already looks like a file ("nginx.conf") keeps its extension —
// appending ".yaml" to that would produce "nginx.conf.yaml".
const DefaultConfigExtension = ".yaml"

// DefaultConfigMountPath is the in-container path a config file appears at when
// neither a platform override nor the cluster's config_mount_root setting says
// otherwise: /etc/config/<name>[.yaml].
func DefaultConfigMountPath(name string) string {
	return ConfigMountPath(DefaultConfigMountRoot, name)
}

// ConfigMountPath joins a mount root and a config name, adding the default
// extension when the name has none. An empty root falls back to
// DefaultConfigMountRoot, so a cluster that never set the knob behaves exactly
// like one that set it to the default.
func ConfigMountPath(root, name string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		root = DefaultConfigMountRoot
	}
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" {
		// The root itself — used when the platform needs the directory rather
		// than a file inside it.
		return strings.TrimSuffix(root, "/")
	}
	if path.Ext(name) == "" {
		name += DefaultConfigExtension
	}
	return strings.TrimSuffix(root, "/") + "/" + name
}

// ParseConfigPath reports whether v is exactly a config_path(<name>)
// expression and returns the config name it references.
//
// It carries no target path by design: the manifest names a config, the
// platform decides where it is mounted (see DefaultConfigMountPath). An app that
// needs the location asks for it with `env: VAR: config_path(name)`, which
// resolves to the same value the mount uses — so the two cannot drift.
func ParseConfigPath(v string) (string, bool) {
	sub := configPathRe.FindStringSubmatch(v)
	if len(sub) != 2 {
		return "", false
	}
	return strings.TrimSpace(sub[1]), true
}

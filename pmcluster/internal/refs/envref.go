package refs

import (
	"regexp"
	"strings"
)

// envRefRe matches a config(name)/secrets(name)/settings(name)/secret(name)
// reference when it is the WHOLE value of an env entry
// (`env: KEY: config(x)`). Unlike inlineRefRe it is anchored — it must not
// match text that has extra content around the reference.
var envRefRe = regexp.MustCompile(`^(config|secrets|settings|secret)\(([^)]+)\)$`)

// configPathRe matches a config_path(<name>) expression — the DSL syntax for
// declaring a Docker config FILE mount (`configs: [config_path(cfg)]`).
var configPathRe = regexp.MustCompile(`^config_path\(([^)]+)\)$`)

// EnvRef is a parsed config(name)/secrets(name)/settings(name)/secret(name)
// env-value reference.
type EnvRef struct {
	Kind string
	Name string
}

// ParseEnvRef reports whether v is exactly a config(name)/secrets(name)/
// settings(name)/secret(name) reference (whole value) and returns its kind +
// name.
func ParseEnvRef(v string) (EnvRef, bool) {
	sub := envRefRe.FindStringSubmatch(v)
	if len(sub) != 3 {
		return EnvRef{}, false
	}
	return EnvRef{Kind: sub[1], Name: strings.TrimSpace(sub[2])}, true
}

// MalformedEnvRef reports whether v starts like a reference (config(,
// secrets(, settings( or secret( prefix) but does not parse as one — a typo
// or a forgotten close paren. Used by manifest validation so operators fail
// loud.
func MalformedEnvRef(v string) bool {
	if !strings.HasPrefix(v, "config(") && !strings.HasPrefix(v, "secrets(") && !strings.HasPrefix(v, "settings(") && !strings.HasPrefix(v, "secret(") {
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

// DefaultConfigMountPath is the in-container path where a Docker config file
// appears when the DSL does not override it (`configs: [config_path(cfg)]`
// mounts at /etc/<name>).
func DefaultConfigMountPath(name string) string {
	return "/etc/" + name
}

// ParseConfigPath reports whether v is exactly a config_path(<name>)
// expression and returns the config name it references.
func ParseConfigPath(v string) (string, bool) {
	sub := configPathRe.FindStringSubmatch(v)
	if len(sub) != 2 {
		return "", false
	}
	return strings.TrimSpace(sub[1]), true
}

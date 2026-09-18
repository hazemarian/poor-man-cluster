package refs

import (
	"regexp"
	"strings"
)

// envRefRe matches a config(name)/secrets(name) reference when it is the
// WHOLE value of an env entry (`env: KEY: config(x)`). Unlike inlineRefRe it
// is anchored — it must not match text that has extra content around the
// reference.
var envRefRe = regexp.MustCompile(`^(config|secrets)\(([^)]+)\)$`)

// EnvRef is a parsed config(name)/secrets(name) env-value reference.
type EnvRef struct {
	Kind string
	Name string
}

// ParseEnvRef reports whether v is exactly a config(name)/secrets(name)
// reference (whole value) and returns its kind + name.
func ParseEnvRef(v string) (EnvRef, bool) {
	sub := envRefRe.FindStringSubmatch(v)
	if len(sub) != 3 {
		return EnvRef{}, false
	}
	return EnvRef{Kind: sub[1], Name: strings.TrimSpace(sub[2])}, true
}

// MalformedEnvRef reports whether v starts like a reference (config( or
// secrets() prefix) but does not parse as one — a typo or a forgotten close
// paren. Used by manifest validation so operators fail loud.
func MalformedEnvRef(v string) bool {
	if !strings.HasPrefix(v, "config(") && !strings.HasPrefix(v, "secrets(") {
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

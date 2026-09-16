package manifest

import (
	"regexp"
	"strings"
)

// envRefRe matches the DSL env-value reference syntax:
//
//	config(app_config)     — inject the DB config's content as the env value
//	secrets(app_secret)    — env value = /run/secrets/<name> (the mounted
//	                         file path); the secret is auto-added to the
//	                         service's mounts so the file actually exists
//
// Exact-match only: a value that merely contains "config(" as part of a
// larger string is treated as a plain literal (compatibility with any app
// that legitimately emits such text).
var envRefRe = regexp.MustCompile(`^(config|secrets)\(([^)]+)\)$`)

// envRef describes a parsed config()/secrets() env reference.
type envRef struct {
	Kind string
	Name string
}

// parseEnvRef extracts a reference from an env value. ok=false means the
// value is a plain literal (or a malformed reference, see malformedEnvRef).
func parseEnvRef(v string) (ref envRef, ok bool) {
	m := envRefRe.FindStringSubmatch(v)
	if m == nil {
		return envRef{}, false
	}
	return envRef{Kind: m[1], Name: strings.TrimSpace(m[2])}, true
}

// malformedEnvRef reports whether v looks like a half-written reference
// (e.g. "config(" with no closing paren, or an empty name). Used by
// Validate to surface typos instead of silently treating them as literals.
func malformedEnvRef(v string) bool {
	if !strings.HasPrefix(v, "config(") && !strings.HasPrefix(v, "secrets(") {
		return false
	}
	_, ok := parseEnvRef(v)
	return !ok
}

// secretMountPath is where Docker Swarm mounts a secret named n inside the
// container. secrets(name) env values resolve to this path.
func secretMountPath(name string) string {
	return "/run/secrets/" + name
}

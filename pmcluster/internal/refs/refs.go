// Package refs is the shared config()/secrets() reference language used
// across the control plane. The DSL env values (internal/manifest) and the
// platform stack templates (internal/cluster) both resolve references through
// this package — one mechanism for replacing configs and secrets everywhere.
//
// Two reference shapes exist:
//
//   - inline references embedded in compose text (`config(x):` as a YAML key,
//     `- source: secrets(y)` as a list item) resolved via ReplaceRefs;
//   - whole-value env references (`env: KEY: config(x)`) parsed via ParseEnvRef.
package refs

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// inlineRefRe matches config(name)/secrets(name) references ANYWHERE in a
// compose file — including inline YAML keys like `config(pmcluster_otel_config):`
// and list items like `- source: config(pmcluster_traefik_dynamic)`. Unlike
// envRefRe (which is exact-match on whole env values), this finds references
// embedded in larger text.
var inlineRefRe = regexp.MustCompile(`(config|secrets)\(([^)]+)\)`)

// RefResolver resolves config()/secrets() references found in compose text.
// The cluster side (internal/cluster) implements it with a RenderInput so the
// platform stack templates use the SAME reference syntax as the DSL env
// values — one mechanism for replacing configs and secrets everywhere.
type RefResolver interface {
	// ResolveConfig returns the Docker config name (or content) for a
	// config(<name>) reference.
	ResolveConfig(ctx context.Context, name string) (string, error)
	// ResolveSecret returns the Docker secret name (or content) for a
	// secrets(<name>) reference.
	ResolveSecret(ctx context.Context, name string) (string, error)
}

// ReplaceRefs rewrites every config(...)/secrets(...) reference in text via
// r. Unknown or malformed references surface as errors so a typo in a
// platform template fails loud instead of silently reaching the swarm.
func ReplaceRefs(ctx context.Context, text string, r RefResolver) (string, error) {
	var err error
	out := inlineRefRe.ReplaceAllStringFunc(text, func(m string) string {
		if err != nil {
			return m
		}
		sub := inlineRefRe.FindStringSubmatch(m)
		if len(sub) != 3 {
			err = fmt.Errorf("malformed reference %q", m)
			return m
		}
		name := strings.TrimSpace(sub[2])
		var resolved string
		switch sub[1] {
		case "config":
			resolved, err = r.ResolveConfig(ctx, name)
		case "secrets":
			resolved, err = r.ResolveSecret(ctx, name)
		default:
			err = fmt.Errorf("unknown reference kind %q", sub[1])
			return m
		}
		if err != nil {
			return m
		}
		return resolved
	})
	return out, err
}

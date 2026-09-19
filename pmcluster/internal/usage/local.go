package usage

import (
	"context"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// Local is the store-backed adapter for the usage port. It reads the latest
// rendered compose of every stack and extracts its configs:/secrets:
// references.
type Local struct {
	Store *store.Store
}

// NewLocal builds the local usage adapter.
func NewLocal(st *store.Store) *Local { return &Local{Store: st} }

// Get computes the config/secret → stacks reference graph.
func (l *Local) Get(ctx context.Context) (*Usage, error) {
	stacks, err := l.Store.ListStacks(ctx)
	if err != nil {
		return nil, err
	}
	configs := map[string]map[string]bool{}
	secrets := map[string]map[string]bool{}
	for _, s := range stacks {
		rev, err := l.Store.GetRevision(ctx, s.Name, s.CurrentRevision)
		if err != nil {
			continue // stack with no stored latest revision — skip
		}
		cfgNames, secNames := parseComposeReferences([]byte(rev.RenderedYAML))
		for _, n := range cfgNames {
			if configs[n] == nil {
				configs[n] = map[string]bool{}
			}
			configs[n][s.Name] = true
		}
		for _, n := range secNames {
			if secrets[n] == nil {
				secrets[n] = map[string]bool{}
			}
			secrets[n][s.Name] = true
		}
	}
	return &Usage{
		Configs: flatten(configs),
		Secrets: flatten(secrets),
	}, nil
}

// parseComposeReferences extracts the names referenced by a rendered compose's
// top-level configs:/secrets: sections. Invalid YAML yields nothing.
func parseComposeReferences(rendered []byte) (configs, secrets []string) {
	var doc struct {
		Configs map[string]any `json:"configs"`
		Secrets map[string]any `json:"secrets"`
	}
	if err := yaml.Unmarshal(rendered, &doc); err != nil {
		return nil, nil
	}
	for name := range doc.Configs {
		configs = append(configs, name)
	}
	for name := range doc.Secrets {
		secrets = append(secrets, name)
	}
	return configs, secrets
}

func flatten(m map[string]map[string]bool) map[string][]string {
	out := make(map[string][]string, len(m))
	for name, stacks := range m {
		list := make([]string, 0, len(stacks))
		for s := range stacks {
			list = append(list, s)
		}
		sort.Strings(list)
		out[name] = list
	}
	return out
}

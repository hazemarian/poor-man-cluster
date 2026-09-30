package usage

import (
	"context"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/refs"
	"github.com/hazemarian/poor-man-cluster/pmcluster/internal/store"
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
		// The rendered compose carries real top-level secrets:/configs:
		// blocks (platform stacks, and app secrets rendered external:true).
		cfgNames, secNames := parseComposeReferences([]byte(rev.RenderedYAML))
		// The source DSL manifest is the authority for config()/secrets()
		// references: config() is resolved into env CONTENT at translation
		// time, so the rendered compose has no top-level configs: block for
		// app stacks. Union both sources so every reference is visible.
		srcCfg, srcSec := refs.FindAll(rev.SourceYAML)
		cfgNames = append(cfgNames, srcCfg...)
		secNames = append(secNames, srcSec...)
		for _, n := range dedupe(cfgNames) {
			if configs[n] == nil {
				configs[n] = map[string]bool{}
			}
			configs[n][s.Name] = true
		}
		for _, n := range dedupe(secNames) {
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

// dedupe returns names in order, keeping only the first occurrence.
func dedupe(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
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

package server

import (
	"context"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"
	"sigs.k8s.io/yaml"

	"github.com/hazemarian/poor-man-stack/pmcluster/internal/store"
)

// usageHTTP exposes GET /api/usage: which stacks reference which configs and
// secrets, computed from the latest rendered compose of each stack.
type usageHTTP struct {
	Store *store.Store
}

func (h *usageHTTP) Mount(r chi.Router) {
	r.Get("/usage", h.get)
}

func (h *usageHTTP) get(w http.ResponseWriter, r *http.Request) {
	configs, secrets, err := computeUsage(r.Context(), h.Store)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "compute usage: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configs": configs,
		"secrets": secrets,
	})
}

// computeUsage maps config/secret name → the stacks that reference it, from
// the latest rendered compose of each stack. Both maps are keyed
// alphabetically (encoding/json sorts map keys) and the stack lists are
// deduped + sorted.
func computeUsage(ctx context.Context, st *store.Store) (map[string][]string, map[string][]string, error) {
	stacks, err := st.ListStacks(ctx)
	if err != nil {
		return nil, nil, err
	}
	configs := map[string]map[string]bool{}
	secrets := map[string]map[string]bool{}
	for _, s := range stacks {
		rev, err := st.GetRevision(ctx, s.Name, s.CurrentRevision)
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
	return flattenUsage(configs), flattenUsage(secrets), nil
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

func flattenUsage(m map[string]map[string]bool) map[string][]string {
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
